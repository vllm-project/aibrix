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
	"sort"
)

// kvWeight is how large a share of a card's spare KV one engine is owed. It is
// the weight the pool policy already uses, so an engine's share does not change
// with which loop is doing the arithmetic. The constant one keeps an idle
// engine in the division rather than starving it at its floor.
func kvWeight(inFlightRequests, completionDelta int64) int64 {
	return 1 + boundedActivity(inFlightRequests) + boundedActivity(completionDelta)
}

// plannedKVLimit is the limit one engine should be held to, and the limit it is
// held to now.
type plannedKVLimit struct {
	claimName  string
	modelName  string
	limitBytes int64
	// fromBytes is what the engine's segment says now, and is negative when
	// there is no segment yet. An engine whose limit is already the planned one
	// needs no write.
	fromBytes int64
}

// planKVLimits divides a card among the engines on it.
//
// Each engine keeps what it holds, and the room left over is shared out by
// weight. The result therefore spends the card exactly: every footprint, every
// engine's held KV, and every share together come to what the card can hold, so
// an engine that grows into its new limit cannot grow into another engine's
// memory.
//
// The newcomer in a placement is one of these engines, with nothing mapped and
// no limit in force. Planning it alongside the engines already there is what
// makes the room it was promised its own.
func planKVLimits(usableBytes int64, engines []engineOnPod) ([]plannedKVLimit, error) {
	if len(engines) == 0 {
		return nil, nil
	}
	ordered := make([]engineOnPod, len(engines))
	copy(ordered, engines)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].claimName < ordered[j].claimName })

	unassignedBytes := usableBytes
	weights := make([]int64, len(ordered))
	totalWeight := int64(0)
	for i, engine := range ordered {
		if engine.footprintBytes <= 0 || engine.kvFloorBytes <= 0 {
			return nil, fmt.Errorf("%s declares no per-GPU cost", engine.claimName)
		}
		unassignedBytes -= engine.footprintBytes + engine.kvHeldBytes()
		weights[i] = kvWeight(engine.inFlightRequests, engine.completionDelta)
		totalWeight += weights[i]
	}
	if unassignedBytes < 0 {
		return nil, fmt.Errorf("the engines on this card hold %s more than it has",
			gibibytes(-unassignedBytes))
	}

	limits := make([]plannedKVLimit, len(ordered))
	grantedBytes := int64(0)
	for i, engine := range ordered {
		extraBytes := unassignedBytes * weights[i] / totalWeight
		grantedBytes += extraBytes
		limits[i] = plannedKVLimit{
			claimName:  engine.claimName,
			modelName:  engine.modelName,
			limitBytes: engine.kvHeldBytes() + extraBytes,
			fromBytes:  engine.kvCapacityBytes,
		}
	}
	// Integer division leaves a few bytes over. Hand them out in a fixed order
	// so two runs of the same arithmetic agree, and a limit does not move by a
	// byte on every pass.
	for i := int64(0); i < unassignedBytes-grantedBytes; i++ {
		limits[i%int64(len(limits))].limitBytes++
	}
	return limits, nil
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
		if limit.fromBytes < 0 || limit.fromBytes == limit.limitBytes {
			continue
		}
		writable = append(writable, limit)
	}
	sort.SliceStable(writable, func(i, j int) bool {
		return writable[i].shrinks() && !writable[j].shrinks()
	})
	return writable
}

// shrinks says whether writing this limit takes memory away from an engine.
func (l plannedKVLimit) shrinks() bool {
	return l.fromBytes >= 0 && l.limitBytes < l.fromBytes
}

// kvLimitsInForce reports the first limit a snapshot does not confirm.
//
// Two things have to be true of every engine that was written. Its segment has
// to hold the new limit, since a write that reached no segment is reported as a
// success either way. And it must not already have mapped more than the new
// limit allows, because a limit does not evict what is mapped, and the room it
// was supposed to free would not be there.
func kvLimitsInForce(snapshot *RuntimeSnapshot, written []plannedKVLimit) error {
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
		if engine.KVCapacityBytes != limit.limitBytes {
			return fmt.Errorf("%s did not take a KV limit of %s",
				limit.modelName, gibibytes(limit.limitBytes))
		}
		if engine.KVUsedBytes > limit.limitBytes {
			return fmt.Errorf("%s holds %s, past the %s it was given",
				limit.modelName, gibibytes(engine.KVUsedBytes), gibibytes(limit.limitBytes))
		}
	}
	return nil
}
