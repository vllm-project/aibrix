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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestPlacementPriorityFirstOrdersByPriorityThenFIFO(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	claims := []modelv1alpha1.ModelClaim{
		*priorityTestClaim("older-low", 1, created),
		*priorityTestClaim("newer-high", 10, created.Add(time.Minute)),
		*priorityTestClaim("z-equal", 10, created.Add(2*time.Minute)),
		*priorityTestClaim("a-equal", 10, created.Add(2*time.Minute)),
	}

	placementPriorityFirst(claims)

	assert.Equal(t, []string{"newer-high", "a-equal", "z-equal", "older-low"}, claimNames(claims))
}

func TestPlacementPriorityFirstKeepsZeroPriorityFIFO(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	claims := []modelv1alpha1.ModelClaim{
		*priorityTestClaim("z-oldest", 0, created),
		*priorityTestClaim("a-oldest", 0, created),
		*priorityTestClaim("newest", 0, created.Add(time.Minute)),
	}

	placementPriorityFirst(claims)

	assert.Equal(t, []string{"a-oldest", "z-oldest", "newest"}, claimNames(claims))
}

func TestPriorityEligibleRejectsIneligibleClaims(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	zero := int32(0)
	deletedAt := metav1.NewTime(time.Unix(1_700_000_000, 0))

	tests := []struct {
		name   string
		mutate func(*modelv1alpha1.ModelClaim)
	}{
		{
			name: "deleting",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.DeletionTimestamp = &deletedAt
			},
		},
		{
			name: "has an existing instance",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: pod.Name}}
			},
		},
		{
			name: "activation failed",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Status.Phase = modelv1alpha1.ModelClaimFailed
			},
		},
		{
			name: "desired replicas already satisfied",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Spec.Replicas = &zero
			},
		},
		{
			name: "selects another pool",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Spec.PodSelector.MatchLabels = map[string]string{
					constants.ModelPoolLabelName: "other-pool",
				}
			},
		},
		{
			name: "invalid per GPU declaration",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Spec.PerGPU = nil
			},
		},
		{
			name: "invalid parallelism",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
					"--tensor-parallel-size": "0",
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := priorityTestClaim("claim", 0, time.Unix(1_700_000_000, 0))
			tt.mutate(claim)

			assert.False(t, priorityEligible(claim, []corev1.Pod{*pod}))
		})
	}
}

func TestPriorityEligibleAcceptsUnplacedValidClaim(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	claim := priorityTestClaim("claim", 0, time.Unix(1_700_000_000, 0))

	assert.True(t, priorityEligible(claim, []corev1.Pod{*pod}))
}

func priorityTestClaim(name string, priority int32, created time.Time) *modelv1alpha1.ModelClaim {
	claim := claimWithCost(300, 100)
	claim.Name = name
	claim.CreationTimestamp = metav1.NewTime(created)
	claim.Spec.Priority = priority
	return claim
}

func claimNames(claims []modelv1alpha1.ModelClaim) []string {
	names := make([]string, 0, len(claims))
	for i := range claims {
		names = append(names, claims[i].Name)
	}
	return names
}
