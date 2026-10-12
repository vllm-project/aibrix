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
	"sort"

	corev1 "k8s.io/api/core/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

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
		claim.Status.Phase == modelv1alpha1.ModelClaimFailed || desiredReplicas(claim) <= 0 ||
		!selectsAny(claim, candidates) {
		return false
	}
	if _, err := perGPUBytesOf(claim); err != nil {
		return false
	}
	_, err := modelParallelism(claim)
	return err == nil
}
