/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package modelclaim

import (
	"fmt"
	"math"
	"sort"
)

// kvExtraWeight is how much of a card's spare KV one engine is given relative
// to the others: one, plus its requests in flight, capped at four as the pool
// policy caps them. The constant one keeps an idle engine in the division
// rather than starving it at its floor.
//
// A sleeping engine weighs nothing. It serves no request, so it keeps only what
// it holds, which after a sleep is normally its floor, and the rest goes to the
// engines that are awake. It gets its part back when the card is divided after
// it wakes.
//
// A serving engine whose request metrics could not be read weighs as much as
// the busiest engine counts. A scrape that timed out says nothing about load,
// and taking the engine for idle would squeeze the one most likely to be too
// busy to answer.
//
// Completions are not counted. The pool policy counts them as the change in a
// counter between its own rounds, and reading that change here would take it
// from the idle-sleep decision that depends on it.
func kvExtraWeight(engine engineOnPod) int64 {
	if engine.asleep {
		return 0
	}
	if engine.demandUnknown {
		// Not known to be idle, so weighed as the busiest an engine counts as.
		return 1 + boundedActivity(math.MaxInt64)
	}
	return 1 + boundedActivity(engine.inFlightRequests)
}

// plannedKVLimit is the limit one engine should be held to, and the limit it is
// held to now.
type plannedKVLimit struct {
	claimName    string
	modelName    string
	kvLimitBytes int64
	// kvCapacityBytes is what the engine's segment says now, and is negative when
	// there is no segment yet. An engine whose limit is already the planned one
	// needs no write.
	kvCapacityBytes int64
	// kvRecordedBytes is what the instance records now, and is zero when it
	// records nothing.
	kvRecordedBytes int64
}

// lowersRecord says whether recording this limit lowers what the instance
// records.
func (l plannedKVLimit) lowersRecord() bool {
	return l.kvLimitBytes < l.kvRecordedBytes
}

// planKVLimits divides a card among the engines on it.
//
// Each engine keeps what it holds, and the room left over is shared out by
// weight. The result therefore spends the card exactly: every footprint, every
// engine's held KV, and every share together come to what the card can hold, so
// an engine that grows into its new limit cannot grow into another engine's
// memory. The one exception is a card whose engines are all asleep. Nothing
// weighs anything there, so the room left over stays unassigned until an engine
// wakes and the card is divided again.
//
// The newcomer in a placement is one of these engines, with nothing mapped and
// no limit in force. Planning it alongside the engines already there is what
// makes the room it was promised its own.
func planKVLimits(hbmUsableBytes int64, engines []engineOnPod) ([]plannedKVLimit, error) {
	if len(engines) == 0 {
		return nil, nil
	}
	ordered := make([]engineOnPod, len(engines))
	copy(ordered, engines)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].claimName < ordered[j].claimName })

	kvUnassignedBytes := hbmUsableBytes
	kvExtraWeights := make([]int64, len(ordered))
	totalKVExtraWeight := int64(0)
	for i, engine := range ordered {
		if engine.maximumFootprintBytes <= 0 || engine.kvFloorBytes <= 0 {
			return nil, fmt.Errorf("%s declares no per-GPU cost", engine.claimName)
		}
		kvUnassignedBytes -= engine.heldBytes()
		kvExtraWeights[i] = kvExtraWeight(engine)
		totalKVExtraWeight += kvExtraWeights[i]
	}
	if kvUnassignedBytes < 0 {
		return nil, fmt.Errorf("the engines on this card hold %s more than it has",
			gibibytes(-kvUnassignedBytes))
	}

	limits := make([]plannedKVLimit, len(ordered))
	totalKVExtraBytes := int64(0)
	for i, engine := range ordered {
		kvExtraBytes := int64(0)
		if totalKVExtraWeight > 0 {
			kvExtraBytes = kvUnassignedBytes * kvExtraWeights[i] / totalKVExtraWeight
		}
		totalKVExtraBytes += kvExtraBytes
		limits[i] = plannedKVLimit{
			claimName:       engine.claimName,
			modelName:       engine.modelName,
			kvLimitBytes:    engine.kvHeldBytes() + kvExtraBytes,
			kvCapacityBytes: engine.kvCapacityBytes,
			kvRecordedBytes: engine.kvRecordedBytes,
		}
	}
	if totalKVExtraWeight == 0 {
		return limits, nil
	}
	// Integer division leaves a few bytes over. Hand them out in a fixed order,
	// among the engines that weigh anything, so two runs of the same arithmetic
	// agree and a limit does not move by a byte on every pass.
	for i, left := 0, kvUnassignedBytes-totalKVExtraBytes; left > 0; i = (i + 1) % len(limits) {
		if kvExtraWeights[i] == 0 {
			continue
		}
		limits[i].kvLimitBytes++
		left--
	}
	return limits, nil
}

// minimumKVLimitChangeBytes is how far a card has to have drifted from its plan
// before dividing it again to follow load is worth the writes.
//
// Two things set the size. A KV allocator hands out whole bundles of pages, and
// a bundle is the page size times the layer count times the number of buffers
// per layer, which came to 112 MiB for a small model and about 504 MiB for a
// 126-layer one on the cards this was measured on. A change smaller than a
// bundle moves no memory at all. And a card divided again on every pass spends
// its time writing limits rather than serving, so the threshold also rises with
// the card.
//
// Placement does not use this. The room it admitted a model against has to be
// made exactly, and a byte skipped there is a byte two engines both own.
func minimumKVLimitChangeBytes(hbmUsableBytes int64) int64 {
	const perBundleBytes = int64(512) << 20
	if perCard := hbmUsableBytes / 100; perCard > perBundleBytes {
		return perCard
	}
	return perBundleBytes
}

// needsDividing reports whether a round is to carry out the plan of a card.
//
// A limit is a ceiling, and an engine maps KV as it needs it. The shares are
// weighed by the requests in flight, which come and go, so the plan moves with
// every reading. Carrying it out each time would cost the writes, on every
// card and in every round, and would give nothing to an engine that is far
// from its limit. So a round carries the plan out in three cases only.
//
// The plan gives an engine that is short of KV more than it is held to. That
// is when a share has to follow its load.
//
// Some engine is held to a limit other than the one its instance records.
// That is what a division leaves behind when a write of it did not take, and
// only a division puts it right.
//
// The card is at rest, and some engine is held to less than half of its share.
// That is what a burst on the engine beside it leaves behind. Left like that,
// the engine would start its own burst with little room.
func needsDividing(engines []engineOnPod, limits []plannedKVLimit, minimumChangeBytes int64) bool {
	return givesAShortEngineMore(engines, limits, minimumChangeBytes) ||
		leftUnfinished(engines) ||
		(atRest(engines) && heldToUnderHalf(limits))
}

// shortOfKV reports whether an engine is short of KV: it has mapped half of
// the limit it is held to, it has requests waiting, or it serves and its load
// could not be read.
//
// Half is where the round starts to act. An engine that has mapped half of its
// limit may reach the limit before its card's next round, and nothing bounds
// how fast an engine maps. An engine that is asleep is never short, and
// neither is one with no limit in force to be short of.
func shortOfKV(engine engineOnPod) bool {
	if engine.asleep || engine.kvCapacityBytes <= 0 {
		return false
	}
	return engine.requestsWaiting > 0 || engine.demandUnknown ||
		engine.kvUsedBytes >= engine.kvCapacityBytes-engine.kvCapacityBytes/2
}

// givesAShortEngineMore reports whether the plan raises the limit of some
// engine that is short of KV by at least the smallest change worth writing.
//
// A plan that gives such an engine less, or the same, is left out. While an
// engine stays short, the plan still moves with the requests in flight on the
// engines beside it, and carrying that out would help nobody.
func givesAShortEngineMore(engines []engineOnPod, limits []plannedKVLimit, minimumChangeBytes int64) bool {
	planned := make(map[string]int64, len(limits))
	for _, limit := range limits {
		planned[limit.claimName] = limit.kvLimitBytes
	}
	for _, engine := range engines {
		if !shortOfKV(engine) {
			continue
		}
		if more := planned[engine.claimName] - engine.kvCapacityBytes; more > 0 && more >= minimumChangeBytes {
			return true
		}
	}
	return false
}

// leftUnfinished reports whether some engine is held to a limit other than the
// one its instance records. An engine with no segment is held to nothing, and
// an instance that records nothing has nothing to differ from.
func leftUnfinished(engines []engineOnPod) bool {
	for _, engine := range engines {
		if engine.kvCapacityBytes >= 0 && engine.kvRecordedBytes > 0 &&
			engine.kvCapacityBytes != engine.kvRecordedBytes {
			return true
		}
	}
	return false
}

// atRest reports whether no engine that is awake has a request in flight. The
// engines that are awake then weigh the same, so the plan of the card depends
// on no sample of its load. An engine whose load could not be read is not
// known to be at rest.
func atRest(engines []engineOnPod) bool {
	for _, engine := range engines {
		if engine.asleep {
			continue
		}
		if engine.inFlightRequests > 0 || engine.demandUnknown {
			return false
		}
	}
	return true
}

// heldToUnderHalf reports whether some engine is held to less than half of the
// limit planned for it.
func heldToUnderHalf(limits []plannedKVLimit) bool {
	for _, limit := range limits {
		if limit.kvCapacityBytes >= 0 && limit.kvCapacityBytes < limit.kvLimitBytes-limit.kvLimitBytes/2 {
			return true
		}
	}
	return false
}

// worthWriting reports whether any engine's limit has drifted far enough from
// the plan to be worth the write.
//
// It is all or nothing. Carrying out half a plan would leave one engine shrunk
// and the engine that was to take the memory still at its old limit, or worse,
// one engine grown into memory another was to give back.
func worthWriting(limits []plannedKVLimit, minimumChangeBytes int64) bool {
	for _, limit := range limits {
		if limit.kvCapacityBytes < 0 {
			continue
		}
		change := limit.kvLimitBytes - limit.kvCapacityBytes
		if change < 0 {
			change = -change
		}
		if change >= minimumChangeBytes {
			return true
		}
	}
	return false
}

// writeOrder puts the limits that shrink an engine before the ones that grow
// one, so no two engines are entitled to the same byte in between. Within each
// group the claim order is kept, which keeps a run reproducible.
//
// A limit with nothing to write is left out: an engine already at its planned
// limit needs no write, and an engine with no KV segment has nothing to write
// into. The second case is not a gap in the arithmetic. An engine without a
// segment has mapped nothing, and it stays off the routing annotation until its
// limit is in force, so it has neither the memory nor the traffic to grow.
func writeOrder(limits []plannedKVLimit) []plannedKVLimit {
	writable := make([]plannedKVLimit, 0, len(limits))
	for _, limit := range limits {
		if limit.kvCapacityBytes < 0 || limit.kvCapacityBytes == limit.kvLimitBytes {
			continue
		}
		writable = append(writable, limit)
	}
	sort.SliceStable(writable, func(i, j int) bool {
		return writable[i].shrinks() && !writable[j].shrinks()
	})
	return writable
}

// shrinksAndGrows splits the limits that need writing into the ones that take
// memory away from an engine and the ones that give it more, each in write
// order. The two are written as separate steps, with a reading in between.
func shrinksAndGrows(limits []plannedKVLimit) (shrinks, grows []plannedKVLimit) {
	for _, limit := range writeOrder(limits) {
		if limit.shrinks() {
			shrinks = append(shrinks, limit)
			continue
		}
		grows = append(grows, limit)
	}
	return shrinks, grows
}

// shrinks says whether writing this limit takes memory away from an engine.
func (l plannedKVLimit) shrinks() bool {
	return l.kvCapacityBytes >= 0 && l.kvLimitBytes < l.kvCapacityBytes
}

// confirmKVLimits reports the first limit a snapshot does not confirm.
//
// Two things have to be true of every engine that was written. Its segment has
// to hold the new limit, since a write that reached no segment is reported as a
// success either way. And it must not already have mapped more than the new
// limit allows, because a limit does not evict what is mapped, and the room it
// was supposed to free would not be there.
func confirmKVLimits(snapshot *RuntimeSnapshot, written []plannedKVLimit) error {
	for _, limit := range written {
		var engine *RuntimeSnapshotModel
		for i := range snapshot.models() {
			if snapshot.Models[i].ModelName == limit.modelName {
				engine = &snapshot.Models[i]
				break
			}
		}
		if engine == nil {
			return fmt.Errorf("%s is no longer on this card", limit.modelName)
		}
		if engine.KVCapacityBytes != limit.kvLimitBytes {
			return fmt.Errorf("%s did not take a KV limit of %s",
				limit.modelName, gibibytes(limit.kvLimitBytes))
		}
		if engine.KVUsedBytes > limit.kvLimitBytes {
			return fmt.Errorf("%s holds %s, past the %s it was given",
				limit.modelName, gibibytes(engine.KVUsedBytes), gibibytes(limit.kvLimitBytes))
		}
	}
	return nil
}
