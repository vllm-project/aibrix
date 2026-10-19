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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// sleepPreemptionVictim is an idle lower-priority engine selected for a
// priority-aware Sleep. It carries the same runtime facts as ordinary
// make-room sleep plus the priority used to compare plans.
type sleepPreemptionVictim struct {
	claim     *modelv1alpha1.ModelClaim
	model     RuntimeSnapshotModel
	port      int32
	idleSince time.Time
	priority  int32
}

func (v sleepPreemptionVictim) idleEngine() idleEngine {
	return idleEngine{claim: v.claim, model: v.model, port: v.port, idleSince: v.idleSince}
}

// sleepPreemptionPlan is the lower-priority engines that would make room for
// a claim on one Pod. An unknown sleeping footprint deliberately ends a plan
// after one victim; a fresh reading after that Sleep supplies the next plan.
type sleepPreemptionPlan struct {
	pod            *corev1.Pod
	victims        []sleepPreemptionVictim
	known          bool
	artifactCached bool
}

func preemptionPolicyOf(pm *modelv1alpha1.ModelClaim) modelv1alpha1.ModelClaimPreemptionPolicy {
	if pm == nil || pm.Spec.PreemptionPolicy == "" {
		return modelv1alpha1.ModelClaimPreemptionNever
	}
	return pm.Spec.PreemptionPolicy
}

// makeRoomByPrioritySleep reserves the selected card and sleeps one victim.
// The next reconcile rebuilds the ledger from the post-Sleep snapshot and
// either places the preemptor or plans the next victim.
func (r *ModelClaimReconciler) makeRoomByPrioritySleep(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	perGPU perGPUBytes,
	candidates []corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	ledgers map[string]podLedger,
	states map[string]PodPlacementState,
	readings *runtimeReadings,
) (string, bool) {
	now := r.now()
	held, holding := r.reservations().heldFor(pm.Namespace, pm.Name, now)
	eligible := candidates
	if holding {
		eligible = nil
		for i := range candidates {
			if candidates[i].Name == held.Name {
				eligible = append(eligible, candidates[i])
				break
			}
		}
	}
	plan, err := r.planSleepPreemption(ctx, pm, eligible, claims, ledgers, states, readings)
	if err != nil {
		klog.ErrorS(err, "could not plan lower-priority sleep preemption", "modelClaim", klog.KObj(pm))
		r.reservations().release(pm.Namespace, pm.Name)
		return "", false
	}
	if plan == nil || len(plan.victims) == 0 {
		r.reservations().release(pm.Namespace, pm.Name)
		return "", false
	}
	seat := perGPU.minimumReserveBytes()
	if !r.reservations().hold(cardOf(plan.pod), pm.Name, seat, now) {
		return "", false
	}
	victim := plan.victims[0]
	if !r.putIdleEngineToSleep(ctx, victim.idleEngine(), plan.pod, pm, readings) {
		r.reservations().release(pm.Namespace, pm.Name)
		return "", false
	}
	more := ""
	if left := len(plan.victims) - 1; left > 0 {
		more = fmt.Sprintf(", and %d more may follow", left)
	}
	return fmt.Sprintf("no pod has room for this model, which needs %s on a card; "+
		"lower-priority model %s (priority %d) was put to sleep on pod %s to make room%s",
		gibibytes(seat), victim.model.ModelName, victim.priority, plan.pod.Name, more), true
}

// planSleepPreemption chooses the best candidate Pod and the lower-priority
// request-idle engines that would have to sleep there. Planning is read-only:
// reservation and Sleep execution are handled by the reconcile path.
func (r *ModelClaimReconciler) planSleepPreemption(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	ledgers map[string]podLedger,
	states map[string]PodPlacementState,
	readings *runtimeReadings,
) (*sleepPreemptionPlan, error) {
	if pm == nil || claims == nil || readings == nil || !pm.DeletionTimestamp.IsZero() ||
		len(pm.Status.Instances) != 0 || desiredReplicas(pm) <= 0 {
		return nil, nil
	}
	perGPU, err := perGPUBytesOf(pm)
	if err != nil {
		return nil, err
	}
	if _, err := modelParallelism(pm); err != nil {
		return nil, fmt.Errorf("invalid engineConfig parallelism: %w", err)
	}
	seat := perGPU.minimumReserveBytes()
	var best *sleepPreemptionPlan
	for i := range candidates {
		pod := &candidates[i]
		ledger, found := ledgers[pod.Name]
		if !found || !selects(pm, pod) || !ledger.judgeable || !podHasGPUs(*pod, ledger.accelerators) ||
			!fitsTopology(*pod, ledger, instanceGPUCount(pm)) || seat > ledger.hbmUsableBytes {
			continue
		}
		if ledger.maximumRoomBytes() >= seat && ledger.heldRoomBytes() >= seat {
			continue
		}
		if _, can := r.roomCanBeMadeOn(ctx, pm, pod, ledger); !can || r.wakeWaitsOn(ctx, pod, ledger, readings) {
			continue
		}
		victims := r.sleepPreemptionVictims(ctx, pm, pod, claims, readings)
		if len(victims) == 0 || !r.preemptionVictimsCanMakeRoom(ledger, victims, seat) {
			continue
		}
		plan, found := r.planSleepPreemptionOnPod(pod, ledger, victims, seat)
		if !found {
			continue
		}
		plan.artifactCached = states[pod.Name].ArtifactCached
		if best == nil || betterSleepPreemptionPlan(plan, *best) {
			candidate := plan
			best = &candidate
		}
	}
	return best, nil
}

// sleepPreemptionVictims lists safe victims without applying the ordinary
// sleepToMakeRoomAfterSeconds threshold. The pool activity record is still
// required so deterministic idle ordering never invents an idle start time.
func (r *ModelClaimReconciler) sleepPreemptionVictims(
	ctx context.Context,
	preemptor *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	readings *runtimeReadings,
) []sleepPreemptionVictim {
	snapshot, err := readings.of(ctx, pod)
	if err != nil || snapshot == nil {
		return nil
	}
	manager := r.poolPolicyManager()
	victims := make([]sleepPreemptionVictim, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready || !model.RequestMetricsObserved ||
			model.RequestsRunning > 0 || model.RequestsWaiting > 0 {
			continue
		}
		claim := claimForRuntimeSnapshot(claims, model)
		if claim == nil || claim.Namespace != preemptor.Namespace || claim.Name == preemptor.Name ||
			!claim.DeletionTimestamp.IsZero() || !isVLLMModel(claim) || claim.Spec.Priority >= preemptor.Spec.Priority {
			continue
		}
		port, active := activeClaimInstancePort(claim, pod.Name)
		if !active {
			continue
		}
		idleSince, seen := manager.lastActive(poolActivityKey(pod, model))
		if !seen {
			continue
		}
		if model.LastTransition != nil && model.LastTransition.After(idleSince) {
			idleSince = *model.LastTransition
		}
		if routed := routedSince(claim); routed.After(idleSince) {
			idleSince = routed
		}
		victims = append(victims, sleepPreemptionVictim{
			claim: claim, model: model, port: port, idleSince: idleSince, priority: claim.Spec.Priority,
		})
	}
	sort.SliceStable(victims, func(i, j int) bool {
		if victims[i].priority != victims[j].priority {
			return victims[i].priority < victims[j].priority
		}
		if !victims[i].idleSince.Equal(victims[j].idleSince) {
			return victims[i].idleSince.Before(victims[j].idleSince)
		}
		return victims[i].claim.Name < victims[j].claim.Name
	})
	return victims
}

// preemptionVictimsCanMakeRoom uses unknown sleeping footprints optimistically
// only to rule out impossible Pods. Unknown memory is never credited to a
// concrete plan; planSleepPreemptionOnPod stops at that victim instead.
func (r *ModelClaimReconciler) preemptionVictimsCanMakeRoom(
	ledger podLedger,
	victims []sleepPreemptionVictim,
	seat int64,
) bool {
	idle := make([]idleEngine, 0, len(victims))
	for _, victim := range victims {
		idle = append(idle, victim.idleEngine())
	}
	return r.sleepsMakeRoom(ledger, idle, seat)
}

func (r *ModelClaimReconciler) planSleepPreemptionOnPod(
	pod *corev1.Pod,
	ledger podLedger,
	victims []sleepPreemptionVictim,
	seat int64,
) (sleepPreemptionPlan, bool) {
	charged := make(map[string]engineOnPod, len(ledger.engines))
	for _, engine := range ledger.engines {
		charged[engine.claimName] = engine
	}
	floors, holding := ledger.maximumRoomBytes(), ledger.heldRoomBytes()
	plan := sleepPreemptionPlan{pod: pod}
	for _, victim := range victims {
		engine, found := charged[victim.claim.Name]
		if !found {
			continue
		}
		plan.victims = append(plan.victims, victim)
		asleep, known := r.footprints().seenAsleep(victim.claim)
		if !known {
			return plan, true
		}
		floors += engine.minimumReserveBytes() - asleep
		holding += engine.heldBytes() - asleep
		if floors >= seat && holding >= seat {
			plan.known = true
			return plan, true
		}
	}
	return sleepPreemptionPlan{}, false
}

// betterSleepPreemptionPlan compares plans in the policy's protection order:
// victim priority dominates certainty and victim count.
func betterSleepPreemptionPlan(plan, than sleepPreemptionPlan) bool {
	planMax, planSum := sleepPreemptionPriority(plan)
	thanMax, thanSum := sleepPreemptionPriority(than)
	switch {
	case planMax != thanMax:
		return planMax < thanMax
	case planSum != thanSum:
		return planSum < thanSum
	case plan.known != than.known:
		return plan.known
	case len(plan.victims) != len(than.victims):
		return len(plan.victims) < len(than.victims)
	case !plan.victims[0].idleSince.Equal(than.victims[0].idleSince):
		return plan.victims[0].idleSince.Before(than.victims[0].idleSince)
	case plan.artifactCached != than.artifactCached:
		return plan.artifactCached
	case plan.pod.Name != than.pod.Name:
		return plan.pod.Name < than.pod.Name
	}
	for i := 0; i < len(plan.victims) && i < len(than.victims); i++ {
		if plan.victims[i].claim.Name != than.victims[i].claim.Name {
			return plan.victims[i].claim.Name < than.victims[i].claim.Name
		}
	}
	return false
}

func sleepPreemptionPriority(plan sleepPreemptionPlan) (int32, int64) {
	var maximum int32
	var sum int64
	for i, victim := range plan.victims {
		if i == 0 || victim.priority > maximum {
			maximum = victim.priority
		}
		sum += int64(victim.priority)
	}
	return maximum, sum
}
