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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
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
		{
			name: "parallelism does not match the pod",
			mutate: func(claim *modelv1alpha1.ModelClaim) {
				claim.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
					"--tensor-parallel-size": "2",
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

func TestPlacementTurnBlockerPriorityAndFIFO(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	for _, tt := range []struct {
		name      string
		priority  int32
		created   time.Time
		claimName string
		wantBlock bool
	}{
		{"newer higher priority", 10, created.Add(time.Minute), "competitor", true},
		{"older lower priority", 0, created.Add(-time.Minute), "competitor", false},
		{"equal priority older", 1, created.Add(-time.Minute), "competitor", true},
		{"equal priority newer", 1, created.Add(time.Minute), "competitor", false},
		{"equal timestamp earlier name", 1, created, "a-first", true},
		{"equal timestamp later name", 1, created, "z-last", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := priorityTestClaim("middle", 1, created)
			competitor := priorityTestClaim(tt.claimName, tt.priority, tt.created)
			pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
			r, runtime := newReconciler(t)
			runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
			readings := newRuntimeReadings(runtime)
			candidates := []corev1.Pod{*pod}
			claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*current, *competitor}}
			ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
			blocker := r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings)
			if tt.wantBlock {
				require.NotNil(t, blocker)
				assert.Equal(t, competitor.Name, blocker.Name)
			} else {
				assert.Nil(t, blocker)
			}
			assert.Equal(t, 1, runtime.snapshotCalls)
			assert.Empty(t, runtime.activateCalls)
		})
	}
}

func TestPlacementTurnBlockerSkipsIneligibleCompetitors(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	zero := int32(0)
	deleted := metav1.NewTime(created)
	for _, tt := range []struct {
		name   string
		mutate func(*modelv1alpha1.ModelClaim)
	}{
		{"deleting", func(c *modelv1alpha1.ModelClaim) { c.DeletionTimestamp = &deleted }},
		{"zero replicas", func(c *modelv1alpha1.ModelClaim) { c.Spec.Replicas = &zero }},
		{"zero instance failed", func(c *modelv1alpha1.ModelClaim) { c.Status.Phase = modelv1alpha1.ModelClaimFailed }},
		{"active instance", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "elsewhere", Phase: modelv1alpha1.ModelClaimActive}}
		}},
		{"activating instance", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "elsewhere", Phase: modelv1alpha1.ModelClaimActivating}}
		}},
		{"sleeping instance", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "elsewhere", Phase: modelv1alpha1.ModelClaimSleeping}}
		}},
		{"failed instance", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "elsewhere", Phase: modelv1alpha1.ModelClaimFailed}}
		}},
		{"another namespace", func(c *modelv1alpha1.ModelClaim) { c.Namespace = "other" }},
		{"disjoint pool", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.PodSelector.MatchLabels[constants.ModelPoolLabelName] = "other"
		}},
		{"invalid selector", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "app", Operator: "invalid"}}
		}},
		{"missing per GPU", func(c *modelv1alpha1.ModelClaim) { c.Spec.PerGPU = nil }},
		{"invalid per GPU", func(c *modelv1alpha1.ModelClaim) { c.Spec.PerGPU.KVFloor = resource.MustParse("0") }},
		{"invalid parallelism", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{"--tensor-parallel-size": "0"}}
		}},
		{"too large", func(c *modelv1alpha1.ModelClaim) { c.Spec.PerGPU.MaximumFootprint = resource.MustParse("2000") }},
		{"unsuitable topology", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{"--tensor-parallel-size": "2"}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := priorityTestClaim("current", 0, created)
			high := priorityTestClaim("high", 10, created.Add(time.Minute))
			tt.mutate(high)
			pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
			r, runtime := newReconciler(t)
			runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
			readings := newRuntimeReadings(runtime)
			candidates := []corev1.Pod{*pod}
			claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*high, *current}}
			ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
			assert.Nil(t, r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings))
		})
	}
}

func TestPlacementTurnBlockerPreservesReservations(t *testing.T) {
	for _, currentHolds := range []bool{false, true} {
		t.Run(map[bool]string{false: "competitor charged by ledger", true: "current keeps its turn"}[currentHolds], func(t *testing.T) {
			current := priorityTestClaim("low", 0, at0758)
			current.Spec.PerGPU = claimWithCost(50, 50).Spec.PerGPU
			high := priorityTestClaim("high", 10, at0759)
			high.Spec.PerGPU = claimWithCost(700, 100).Spec.PerGPU
			pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
			r, runtime := newReconciler(t)
			runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
			owner := high.Name
			if currentHolds {
				owner = current.Name
			}
			require.True(t, r.reservations().hold(cardOf(pod), owner, 800, r.now()))
			readings := newRuntimeReadings(runtime)
			candidates := []corev1.Pod{*pod}
			claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*high, *current}}
			ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
			r.reservations().takeFrom(ledgers, testNamespace, current.Name, r.now())
			blocker := r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings)
			if currentHolds {
				assert.Nil(t, blocker)
			} else {
				admissible, _ := admissibleCandidates(candidates, ledgers, 800, 1)
				require.Empty(t, admissible, "the owner's reservation is already charged in this ledger")
				require.NotNil(t, blocker)
				assert.Equal(t, high.Name, blocker.Name)
			}
			card, held := r.reservations().heldFor(testNamespace, owner, r.now())
			assert.True(t, held)
			assert.Equal(t, cardOf(pod), card)
		})
	}
}

func TestPlacementTurnBlockerInfeasibleCurrentKeepsItsRefusal(t *testing.T) {
	current := priorityTestClaim("low", 0, at0758)
	current.Spec.PerGPU = claimWithCost(2000, 100).Spec.PerGPU
	high := priorityTestClaim("high", 10, at0759)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	readings := newRuntimeReadings(runtime)
	candidates := []corev1.Pod{*pod}
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*current, *high}}
	ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
	assert.Nil(t, r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings))
}

func TestPlacementTurnBlockerFailedRetryYieldsToOrdinaryClaims(t *testing.T) {
	current := priorityTestClaim("failed-high", 100, at0758)
	current.Status.Phase = modelv1alpha1.ModelClaimFailed
	ordinary := priorityTestClaim("ordinary-low", 0, at0759)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	readings := newRuntimeReadings(runtime)
	candidates := []corev1.Pod{*pod}
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*current, *ordinary}}
	ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
	blocker := r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings)
	require.NotNil(t, blocker)
	assert.Equal(t, ordinary.Name, blocker.Name)
	claims.Items[1].Spec.PerGPU = claimWithCost(2000, 100).Spec.PerGPU
	assert.Nil(t, r.placementTurnBlocker(context.Background(), current, candidates, claims, ledgers, readings))
}

func TestClaimFeasibleForPlacementFiltersSharedPodsAndUnavailableReadings(t *testing.T) {
	claim := priorityTestClaim("claim", 0, at0758)
	claim.Spec.PodSelector.MatchLabels["selected"] = "yes"
	selected, selectedSnapshot := sizedWarmPod("selected", "10.0.0.1", 1000)
	selected.Labels["selected"] = "yes"
	other, otherSnapshot := sizedWarmPod("other", "10.0.0.2", 10000)
	for _, tt := range []struct {
		name     string
		snapshot *RuntimeSnapshot
		want     bool
	}{
		{"direct fit", selectedSnapshot, true},
		{"runtime unavailable", nil, false},
		{"only unselected pod fits", &RuntimeSnapshot{Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMUsableBytes: 100}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, runtime := newReconciler(t)
			runtime.snapshots = map[string]*RuntimeSnapshot{other.Status.PodIP: otherSnapshot}
			if tt.snapshot == nil {
				runtime.nilSnapshots = map[string]bool{selected.Status.PodIP: true}
			} else {
				runtime.snapshots[selected.Status.PodIP] = tt.snapshot
			}
			readings := newRuntimeReadings(runtime)
			candidates := []corev1.Pod{*selected, *other}
			claims := &modelv1alpha1.ModelClaimList{}
			ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), nil)
			assert.Equal(t, tt.want, r.claimFeasibleForPlacement(context.Background(), claim, candidates, ledgers, readings))
			assert.Equal(t, map[string]int{selected.Status.PodIP: 1, other.Status.PodIP: 1}, runtime.snapshotCallsTo)
		})
	}
}

func TestClaimFeasibleForPlacementMakesRoomWithoutMutating(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	claim := priorityTestClaim("waiting", 10, at0759)
	newClaim(t, r, claim.Name, at0759)
	claim = getModel(t, r, claim.Name)
	candidates, err := r.listCandidateWarmPods(context.Background(), claim)
	require.NoError(t, err)
	claims, err := r.listClaimsForAccount(context.Background(), testNamespace)
	require.NoError(t, err)
	readings := newRuntimeReadings(runtime)
	ledgers := podLedgersFrom(claims, nil, candidates, readings.ofPods(context.Background(), candidates), r.podsWithoutWakeReserve(context.Background(), candidates))
	admissible, _ := admissibleCandidates(candidates, ledgers, 400, 1)
	require.Empty(t, admissible)
	before := claim.DeepCopy()
	assert.True(t, r.claimFeasibleForPlacement(context.Background(), claim, candidates, ledgers, readings))
	assert.Empty(t, runtime.sleepCalls)
	assert.Empty(t, runtime.kvLimitCalls)
	assert.Empty(t, runtime.activateCalls)
	assert.Equal(t, before, getModel(t, r, claim.Name))
	_, held := r.reservations().heldFor(testNamespace, claim.Name, r.now())
	assert.False(t, held)
	assert.Equal(t, 1, runtime.snapshotCalls)
}

func TestPlacementTurnBlockerExistingInstanceBypassesAdmission(t *testing.T) {
	current := priorityTestClaim("replacement", 0, at0758)
	current.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "failed", Phase: modelv1alpha1.ModelClaimFailed}}
	high := priorityTestClaim("high", 10, at0759)
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	r, _ := newReconciler(t)
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*high}}
	assert.Nil(t, r.placementTurnBlocker(context.Background(), current, []corev1.Pod{*pod}, claims, nil, newRuntimeReadings(nil)))
	assert.Empty(t, r.reservations().held)
	assert.NotContains(t, r.backoff().attempts, types.NamespacedName{Namespace: testNamespace, Name: current.Name})
}
