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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// minimumReserveBytes is what one instance of a claim takes off a card and does
// not give back while it is awake: the maximum footprint it declared plus its
// KV floor. An engine's KV can be squeezed towards that floor but never past
// it, so this is a lower bound on what the instance occupies rather than an
// estimate of it. It is zero for a claim that declares nothing.
func minimumReserveBytes(pm *modelv1alpha1.ModelClaim) int64 {
	if pm == nil || pm.Spec.PerGPU == nil {
		return 0
	}
	footprint, floor := pm.Spec.PerGPU.MaximumFootprint.Value(), pm.Spec.PerGPU.KVFloor.Value()
	// A quantity carries no schema minimum, so a figure that is not positive is
	// caught here instead. It is read as no declaration rather than as a model
	// that costs nothing, which is what a zero would otherwise say.
	if footprint <= 0 || floor <= 0 {
		return 0
	}
	return footprint + floor
}

// kvFloorBytes is the KV cache a claim declared one instance must keep on a
// card, and zero for a claim that declares nothing.
func kvFloorBytes(pm *modelv1alpha1.ModelClaim) int64 {
	if pm == nil || pm.Spec.PerGPU == nil {
		return 0
	}
	if floor := pm.Spec.PerGPU.KVFloor.Value(); floor > 0 {
		return floor
	}
	return 0
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
}

// maximumRoomBytes is the most memory this card could ever offer another
// instance: what is left once every instance on it is down to the KV floor its
// claim declared. Getting there means engines give back everything above their
// floor, so a model that needs more than this cannot be placed here by waiting.
// It is negative when the card is already promised more than it has.
func (l podLedger) maximumRoomBytes() int64 {
	return l.usableBytes - l.owedBytes
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
// Card sizes come from the runtime snapshots placement has already gathered.
// What a card owes comes from ModelClaim status, which only this controller
// writes, so the account survives a controller restart and charges an instance
// from the moment it is recorded rather than from the moment its engine gets
// around to allocating.
//
// Status alone is not enough to judge a card. It says what this controller
// committed, and the snapshot says what is actually running. An engine in the
// snapshot that answers to no recorded instance is memory nobody is accounting
// for, so the card is refused rather than offered. Free memory used to cover
// that case by accident; nothing here reads free memory, so the check has to
// be deliberate.
func (r *ModelClaimReconciler) collectPodLedgers(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
	states map[string]PodPlacementState,
	snapshots map[string]*RuntimeSnapshot,
) map[string]podLedger {
	ledgers := make(map[string]podLedger, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		state, reported := states[pod.Name]
		switch {
		case !reported:
			ledgers[pod.Name] = podLedger{blocked: "its runtime did not answer"}
		case !state.HBMUsableKnown:
			ledgers[pod.Name] = podLedger{blocked: "its cards could not be measured"}
		default:
			ledgers[pod.Name] = podLedger{judgeable: true, usableBytes: state.HBMUsableBytes}
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

	// Every engine a recorded instance answers to, so the ones left over can be
	// found afterwards.
	accounted := make(map[string]map[string]struct{}, len(ledgers))
	for i := range claims.Items {
		claim := &claims.Items[i]
		reserve := minimumReserveBytes(claim)
		served := servedModelName(claim)
		for _, instance := range claim.Status.Instances {
			if _, tracked := ledgers[instance.Pod]; tracked {
				if engine := snapshotModelForClaim(snapshots[instance.Pod], claim, served); engine != nil {
					if _, seen := accounted[instance.Pod]; !seen {
						accounted[instance.Pod] = map[string]struct{}{}
					}
					accounted[instance.Pod][snapshotActivityKey(*engine)] = struct{}{}
				}
			}
			// A failed instance has exhausted its restarts and its engine is
			// gone, so its memory is back with the card. Charging for it would
			// take a slice of GPU out of circulation for as long as the claim
			// exists, and nothing would put it back. An activating instance is
			// charged, and deliberately: placement has already committed those
			// bytes, and waiting for readiness would let a second claim be
			// placed against the same memory.
			if instance.Phase == modelv1alpha1.ModelClaimFailed {
				continue
			}
			ledger, tracked := ledgers[instance.Pod]
			if !tracked {
				continue
			}
			if claim.Spec.PerGPU == nil {
				ledger = ledger.withHole(fmt.Sprintf(
					"%s runs there and declares no per-GPU cost", claim.Name))
			}
			ledger.owedBytes += reserve
			ledgers[instance.Pod] = ledger
		}
	}

	// An engine nobody claimed is memory nobody can account for. Offering that
	// card would hand the same memory out twice, so the card is refused until
	// the engine is either claimed or gone.
	for name, ledger := range ledgers {
		snapshot := snapshots[name]
		if snapshot == nil {
			continue
		}
		for _, engine := range snapshot.Models {
			if !engine.Alive {
				continue
			}
			if _, known := accounted[name][snapshotActivityKey(engine)]; known {
				continue
			}
			ledgers[name] = ledger.withHole(fmt.Sprintf(
				"the engine serving %s there answers to no claim", engine.ModelName))
			break
		}
	}
	return ledgers
}
