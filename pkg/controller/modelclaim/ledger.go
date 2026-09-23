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
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// perGPUBytes is a claim's spec.perGPU in bytes: what one instance costs on
// each GPU it runs on.
type perGPUBytes struct {
	maximumFootprintBytes int64
	kvFloorBytes          int64
}

// minimumReserveBytes is what one instance takes off a card and does not give
// back while it is awake: its maximum footprint plus its KV floor. An engine's
// KV can be squeezed towards that floor but never past it, so this is a lower
// bound on what the instance occupies rather than an estimate of it.
func (p perGPUBytes) minimumReserveBytes() int64 {
	return p.maximumFootprintBytes + p.kvFloorBytes
}

// perGPUBytesOf reads what a claim declared one instance costs on a GPU, and
// says what is wrong with the declaration when it cannot be used.
//
// A quantity carries no schema minimum, so a figure that is not positive is
// caught here. It is refused rather than read as a model that costs nothing,
// which is what a zero would otherwise say.
func perGPUBytesOf(pm *modelv1alpha1.ModelClaim) (perGPUBytes, error) {
	if pm == nil || pm.Spec.PerGPU == nil {
		return perGPUBytes{}, errors.New("spec.perGPU is missing")
	}
	declared := pm.Spec.PerGPU
	if declared.MaximumFootprint.Value() <= 0 {
		return perGPUBytes{}, fmt.Errorf("spec.perGPU.maximumFootprint is %s, which is not positive",
			declared.MaximumFootprint.String())
	}
	if declared.KVFloor.Value() <= 0 {
		return perGPUBytes{}, fmt.Errorf("spec.perGPU.kvFloor is %s, which is not positive",
			declared.KVFloor.String())
	}
	return perGPUBytes{
		maximumFootprintBytes: declared.MaximumFootprint.Value(),
		kvFloorBytes:          declared.KVFloor.Value(),
	}, nil
}

// kvLimitUnknown stands for an engine whose KV segment could not be read, which
// is what an engine that has not built one yet and one that is gone have in
// common.
const kvLimitUnknown = int64(-1)

// engineOnPod is one instance on a card: what its claim declared it costs, and
// what its engine is doing with the memory right now. An instance that has been
// recorded but whose engine has not started yet is described here too, with
// nothing mapped and no limit in force.
type engineOnPod struct {
	claimName string
	// modelName is what the runtime knows this engine as, which is the name a
	// limit is written against.
	modelName string
	// snapshotKey identifies the engine in a runtime snapshot, and is empty
	// while no engine has been seen.
	snapshotKey string
	// perGPUBytes is what the claim declared one instance costs on a card.
	perGPUBytes
	// kvUsedBytes is the KV this engine has mapped: its pages in use and the
	// ones it holds in reserve. An engine with no KV segment has mapped
	// nothing, so this is zero rather than unknown.
	kvUsedBytes int64
	// kvCapacityBytes is the limit written in the engine's segment now, and is
	// negative when there is no segment to read it from.
	kvCapacityBytes int64
	// inFlightRequests and completionDelta are the demand signals a share is
	// weighted by, taken from the same runtime figures the pool policy uses.
	inFlightRequests int64
	completionDelta  int64
}

// kvHeldBytes is the KV an engine keeps whatever else happens on the card: the
// floor its claim declared, or what it has already mapped when that is larger.
// Lowering a limit does not evict a mapped page, so the pages an engine holds
// are not room that can be offered to anyone else.
func (e engineOnPod) kvHeldBytes() int64 {
	if e.kvUsedBytes > e.kvFloorBytes {
		return e.kvUsedBytes
	}
	return e.kvFloorBytes
}

// podLedger is one card's account: how much it can hold, and how much of it the
// instances already recorded there were promised.
//
// judgeable is false when the account has a hole in it, and blocked says which
// one. An account with a hole never admits a placement: the memory it cannot
// see is memory it would otherwise hand out twice.
type podLedger struct {
	judgeable   bool
	blocked     string
	usableBytes int64
	owedBytes   int64
	heldBytes   int64
	// observedAt is when the snapshot this account was built from was taken. A
	// limit written from it carries the same moment, which is what tells the
	// runtime one attempt from the next.
	observedAt time.Time
	// engines are the instances recorded on this card, each with what its claim
	// declared and what its engine currently holds.
	engines []engineOnPod
}

// maximumRoomBytes is the most memory this card could ever offer another
// instance: what is left once every instance on it is down to the KV floor its
// claim declared. Getting there means engines give back everything above their
// floor, so a model that needs more than this cannot be placed here by waiting.
// It is negative when the card is already promised more than it has.
func (l podLedger) maximumRoomBytes() int64 {
	return l.usableBytes - l.owedBytes
}

// heldRoomBytes is what this card can offer another instance now, without
// anything being given back: what is left once every engine keeps its footprint
// and the KV it already holds. Lowering a limit evicts nothing, so a model that
// needs more than this cannot be placed here today even though the card may be
// able to hold it later.
func (l podLedger) heldRoomBytes() int64 {
	return l.usableBytes - l.heldBytes
}

// withHole marks an account that cannot be trusted, keeping the first cause
// found: one reason an operator can act on beats a list that grows with the
// pool.
func (l podLedger) withHole(reason string) podLedger {
	if l.blocked == "" {
		l.blocked = reason
	}
	l.judgeable = false
	return l
}

// collectPodLedgers builds one account per candidate pod.
//
// The snapshots must be fresh rather than cached. Ranking can work from a
// reading a few seconds old, but an account cannot: what an engine holds moves
// with traffic, and admitting a model against memory another engine has since
// mapped is how a card ends up oversubscribed.
//
// What a card owes comes from ModelClaim status, which only this controller
// writes, so the account charges an instance from the moment it is recorded
// rather than from the moment its engine gets around to allocating. The
// snapshot then says how much of that an engine has actually taken.
func (r *ModelClaimReconciler) collectPodLedgers(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
	snapshots map[string]*RuntimeSnapshot,
) map[string]podLedger {
	ledgers := make(map[string]podLedger, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		usableBytes, measured := snapshots[pod.Name].hbmUsableBytes()
		switch {
		case snapshots[pod.Name] == nil:
			ledgers[pod.Name] = podLedger{blocked: "its runtime did not answer"}
		case !measured:
			ledgers[pod.Name] = podLedger{blocked: "its cards could not be measured"}
		default:
			ledgers[pod.Name] = podLedger{
				judgeable:   true,
				usableBytes: usableBytes,
				observedAt:  snapshots[pod.Name].ObservedAt,
			}
		}
	}

	// Deliberately not the cached client. An instance recorded moments ago may
	// not have reached the informer yet, and an instance missing from the
	// account is memory a second claim would be told is free.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	claims := &modelv1alpha1.ModelClaimList{}
	if err := reader.List(ctx, claims, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "collect pod ledgers: list model claims", "namespace", namespace)
		for name, ledger := range ledgers {
			ledgers[name] = ledger.withHole("the claims on it could not be listed")
		}
		return ledgers
	}

	accounted := make(map[string]map[string]struct{}, len(ledgers))
	for i := range claims.Items {
		claim := &claims.Items[i]
		served := servedModelName(claim)
		for _, instance := range claim.Status.Instances {
			ledger, tracked := ledgers[instance.Pod]
			if !tracked {
				continue
			}
			perGPU, perGPUErr := perGPUBytesOf(claim)
			engine := engineOnPod{
				claimName:       claim.Name,
				modelName:       served,
				perGPUBytes:     perGPU,
				kvCapacityBytes: kvLimitUnknown,
			}
			if model := snapshotModelForClaim(snapshots[instance.Pod], claim, served); model != nil {
				engine.snapshotKey = snapshotActivityKey(*model)
				engine.kvCapacityBytes = model.KVCapacityBytes
				engine.inFlightRequests = max(model.RequestsRunning, 0) + max(model.RequestsWaiting, 0)
				// A negative figure means there is no KV segment to read, and
				// an engine without one has mapped nothing.
				engine.kvUsedBytes = max(model.KVUsedBytes, 0)
				if _, seen := accounted[instance.Pod]; !seen {
					accounted[instance.Pod] = map[string]struct{}{}
				}
				accounted[instance.Pod][engine.snapshotKey] = struct{}{}
			}
			// A failed instance has exhausted its restarts, so its memory is
			// back with the card unless its engine is somehow still there.
			// Charging for an engine that is gone would take a slice of GPU out
			// of circulation for as long as the claim exists, and nothing would
			// put it back. An activating instance is charged, and deliberately:
			// placement has already committed those bytes, and waiting for
			// readiness would let a second claim be placed against the same
			// memory.
			if instance.Phase == modelv1alpha1.ModelClaimFailed && engine.snapshotKey == "" {
				continue
			}
			// A claim whose declaration cannot be used is charged nothing, so its
			// card cannot be judged either. A zero written by mistake would
			// otherwise read as an engine that takes up no room.
			if perGPUErr != nil {
				ledger = ledger.withHole(fmt.Sprintf("%s runs there, and %v", claim.Name, perGPUErr))
			}
			ledger.owedBytes += engine.minimumReserveBytes()
			ledger.heldBytes += engine.maximumFootprintBytes + engine.kvHeldBytes()
			ledger.engines = append(ledger.engines, engine)
			ledgers[instance.Pod] = ledger
		}
	}

	// An engine nobody claimed is memory nobody can account for. It is charged
	// to no instance, so leaving the card judgeable would offer its memory to
	// the next model as though it were free.
	for name, ledger := range ledgers {
		for _, model := range snapshots[name].models() {
			// A dead engine has given its memory back, so it is not a hole.
			if !model.Alive {
				continue
			}
			if _, known := accounted[name][snapshotActivityKey(model)]; known {
				continue
			}
			ledgers[name] = ledger.withHole(fmt.Sprintf(
				"the engine serving %s there answers to no claim", model.ModelName))
			break
		}
	}
	return ledgers
}

// models is the engine list of a snapshot that may be missing.
func (s *RuntimeSnapshot) models() []RuntimeSnapshotModel {
	if s == nil {
		return nil
	}
	return s.Models
}
