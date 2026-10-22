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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

const scheduledReasonWaitingForPlacementTurn = "WaitingForPlacementTurn"

// placementAdmissionReadError means no activation was attempted. A transient
// account read must not move an ordinary claim into the Failed retry lane.
type placementAdmissionReadError struct{ err error }

func (e placementAdmissionReadError) Error() string {
	return "cannot read claims for placement admission: " + e.err.Error()
}

func (e placementAdmissionReadError) Unwrap() error { return e.err }

// placementPriorityFirst orders unplaced claims by descending priority while
// retaining FIFO order among claims with equal priority.
func placementPriorityFirst(claims []modelv1alpha1.ModelClaim) {
	sort.SliceStable(claims, func(i, j int) bool {
		if claims[i].Spec.Priority != claims[j].Spec.Priority {
			return claims[i].Spec.Priority > claims[j].Spec.Priority
		}
		if !claims[i].CreationTimestamp.Equal(&claims[j].CreationTimestamp) {
			return claims[i].CreationTimestamp.Before(&claims[j].CreationTimestamp)
		}
		return claims[i].Name < claims[j].Name
	})
}

// priorityEligible reports whether a claim can compete for a new placement in
// the given candidate pods.
func priorityEligible(claim *modelv1alpha1.ModelClaim, candidates []corev1.Pod) bool {
	if claim == nil || !claim.DeletionTimestamp.IsZero() || len(claim.Status.Instances) != 0 ||
		desiredReplicas(claim) <= 0 {
		return false
	}
	if _, err := perGPUBytesOf(claim); err != nil {
		return false
	}
	shared, valid := candidatePodsForClaim(claim, candidates)
	return valid && len(shared) > 0
}

// candidatePodsForClaim applies the same selector and declared GPU topology
// checks to placement admission and make-room ordering. Runtime-reported GPU
// topology is checked later against the ledger. False means invalid parallelism.
func candidatePodsForClaim(claim *modelv1alpha1.ModelClaim, candidates []corev1.Pod) ([]corev1.Pod, bool) {
	parallelism, err := modelParallelism(claim)
	if err != nil {
		return nil, false
	}
	shared := make([]corev1.Pod, 0, len(candidates))
	for i := range candidates {
		pod := candidates[i]
		if !selects(claim, &pod) || (isVLLMModel(claim) && !podSupportsVLLMParallelism(pod, parallelism)) {
			continue
		}
		shared = append(shared, pod)
	}
	return shared, true
}

// claimFeasibleForPlacement uses the current pass's account and readings to
// determine whether a claim can fit directly or have room made for it.
func (r *ModelClaimReconciler) claimFeasibleForPlacement(
	ctx context.Context,
	claim *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	ledgers map[string]podLedger,
	readings *runtimeReadings,
) bool {
	perGPU, err := perGPUBytesOf(claim)
	if err != nil {
		return false
	}
	shared, valid := candidatePodsForClaim(claim, candidates)
	if !valid {
		return false
	}
	admissible, _ := admissibleCandidates(shared, ledgers, perGPU.minimumReserveBytes(), instanceGPUCount(claim))
	if len(admissible) > 0 {
		return true
	}
	for i := range shared {
		pod := &shared[i]
		if _, found := r.planRoom(ctx, claim, pod, ledgers[pod.Name], perGPU.minimumReserveBytes(), readings); found {
			return true
		}
	}
	return false
}

// claimFeasibleForPlacementWithPolicy applies the claim's make-room policy
// when deciding whether it keeps its priority/FIFO placement turn.
func (r *ModelClaimReconciler) claimFeasibleForPlacementWithPolicy(
	ctx context.Context,
	claim *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	ledgers map[string]podLedger,
	readings *runtimeReadings,
) bool {
	if preemptionPolicyOf(claim) != modelv1alpha1.ModelClaimPreemptionSleepLowerPriority {
		return r.claimFeasibleForPlacement(ctx, claim, candidates, ledgers, readings)
	}
	perGPU, err := perGPUBytesOf(claim)
	if err != nil {
		return false
	}
	shared, valid := candidatePodsForClaim(claim, candidates)
	if !valid {
		return false
	}
	admissible, _ := admissibleCandidates(shared, ledgers, perGPU.minimumReserveBytes(), instanceGPUCount(claim))
	if len(admissible) > 0 {
		return true
	}
	parallelism, err := modelParallelism(claim)
	if err != nil {
		return false
	}
	states := placementStatesFrom(readings.ofPods(ctx, shared), shared, claim.Spec.ArtifactURL, parallelism)
	plan, err := r.planSleepPreemption(ctx, claim, shared, claims, ledgers, states, readings)
	return err == nil && plan != nil
}

// placementTurnBlocker identifies the first feasible predecessor using the
// same authoritative claims that built the card account. Existing instances
// and acquired reservations retain their placement turns.
func (r *ModelClaimReconciler) placementTurnBlocker(
	ctx context.Context,
	current *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	ledgers map[string]podLedger,
	readings *runtimeReadings,
) *modelv1alpha1.ModelClaim {
	if len(current.Status.Instances) != 0 {
		return nil
	}
	now := r.now()
	if _, held := r.reservations().heldFor(current.Namespace, current.Name, now); held {
		return nil
	}
	// An infeasible current claim must retain its actual refusal and backoff.
	if !r.claimFeasibleForPlacementWithPolicy(ctx, current, candidates, claims, ledgers, readings) {
		return nil
	}
	waiting := make([]modelv1alpha1.ModelClaim, 0, len(claims.Items)+1)
	// The current claim has passed its own backoff gate, so a due Failed
	// retry competes by the same priority and FIFO order as an ordinary claim.
	waiting = append(waiting, *current)
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Namespace == current.Namespace && claim.Name != current.Name && priorityEligible(claim, candidates) {
			waiting = append(waiting, *claim)
		}
	}
	placementPriorityFirst(waiting)
	for i := range waiting {
		claim := &waiting[i]
		if claim.Name == current.Name {
			return nil
		}
		// The current ledger already charges this owner's reservation, so
		// checking only physical fit would incorrectly discard its turn.
		if _, held := r.reservations().heldFor(claim.Namespace, claim.Name, now); held {
			return claim
		}
		shared, _ := candidatePodsForClaim(claim, candidates)
		if r.backoff().waiting(types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name},
			claim.Generation, roomSignatureOf(shared, claims), ledgers) {
			continue
		}
		if r.claimFeasibleForPlacementWithPolicy(ctx, claim, candidates, claims, ledgers, readings) {
			return claim
		}
	}
	return nil
}
