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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestPreemptionPolicyOfDefaultsEmptyToNever(t *testing.T) {
	claim := &modelv1alpha1.ModelClaim{}
	assert.Equal(t, modelv1alpha1.ModelClaimPreemptionNever, preemptionPolicyOf(claim))
	assert.Equal(t, modelv1alpha1.ModelClaimPreemptionNever, preemptionPolicyOf(nil))

	claim.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionSleepLowerPriority
	assert.Equal(t, modelv1alpha1.ModelClaimPreemptionSleepLowerPriority, preemptionPolicyOf(claim))
}

type preemptionVictimFixture struct {
	name              string
	pod               string
	priority          int32
	footprint         int64
	floor             int64
	idleSince         time.Time
	sleepingFootprint int64
	footprintKnown    bool
}

type preemptionPlannerFixture struct {
	r         *ModelClaimReconciler
	preemptor *modelv1alpha1.ModelClaim
	pods      []corev1.Pod
	claims    *modelv1alpha1.ModelClaimList
	ledgers   map[string]podLedger
	states    map[string]PodPlacementState
	readings  *runtimeReadings
}

func newPreemptionPlannerFixture(t *testing.T, cards int, victims ...preemptionVictimFixture) preemptionPlannerFixture {
	t.Helper()
	servingClaims := make([]serving, 0, len(victims))
	for _, victim := range victims {
		servingClaims = append(servingClaims, serving{
			name: victim.name, pod: victim.pod, footprint: victim.footprint,
			floor: victim.floor, idleSince: victim.idleSince,
		})
	}
	r, runtime := servingPool(t, keepNoWakeReserve, cards, servingClaims...)
	claims, err := r.listClaimsForAccount(context.Background(), testNamespace)
	require.NoError(t, err)
	byName := make(map[string]preemptionVictimFixture, len(victims))
	for _, victim := range victims {
		byName[victim.name] = victim
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		victim, found := byName[claim.Name]
		if !found {
			continue
		}
		claim.Spec.Priority = victim.priority
		if victim.footprintKnown {
			r.footprints().note(claim, victim.sleepingFootprint)
		}
	}

	pods := []corev1.Pod{*podNamed(t, r, "warm-1")}
	if cards == 2 {
		pods = append(pods, *podNamed(t, r, "warm-2"))
	}
	preemptor := claimOnPod("preemptor", "", modelv1alpha1.ModelClaimActive, 300, 100)
	preemptor.Status.Instances = nil
	preemptor.Spec.Priority = 100
	preemptor.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionSleepLowerPriority
	readings := newRuntimeReadings(runtime)
	snapshots := readings.ofPods(context.Background(), pods)
	ledgers := podLedgersFrom(claims, nil, pods, snapshots, r.podsWithoutWakeReserve(context.Background(), pods))
	states := placementStatesFrom(snapshots, pods, preemptor.Spec.ArtifactURL, 1)
	return preemptionPlannerFixture{
		r: r, preemptor: preemptor, pods: pods, claims: claims,
		ledgers: ledgers, states: states, readings: readings,
	}
}

func (f preemptionPlannerFixture) claim(name string) *modelv1alpha1.ModelClaim {
	for i := range f.claims.Items {
		if f.claims.Items[i].Name == name {
			return &f.claims.Items[i]
		}
	}
	return nil
}

func (f preemptionPlannerFixture) model(podName, name string) *RuntimeSnapshotModel {
	reading := f.readings.byPod[podName]
	if reading.snapshot == nil {
		return nil
	}
	for i := range reading.snapshot.Models {
		if reading.snapshot.Models[i].ModelName == name {
			return &reading.snapshot.Models[i]
		}
	}
	return nil
}

func (f preemptionPlannerFixture) plan(t *testing.T) *sleepPreemptionPlan {
	t.Helper()
	plan, err := f.r.planSleepPreemption(context.Background(), f.preemptor, f.pods,
		f.claims, f.ledgers, f.states, f.readings)
	require.NoError(t, err)
	return plan
}

func TestPlanSleepPreemptionEligibility(t *testing.T) {
	recent := time.Date(2026, time.October, 1, 8, 0, 1, 0, time.UTC)
	for name, mutate := range map[string]func(preemptionPlannerFixture){
		"equal priority": func(f preemptionPlannerFixture) {
			f.claim("candidate").Spec.Priority = f.preemptor.Spec.Priority
		},
		"higher priority": func(f preemptionPlannerFixture) {
			f.claim("candidate").Spec.Priority = f.preemptor.Spec.Priority + 1
		},
		"deleting claim": func(f preemptionPlannerFixture) {
			deleted := metav1.NewTime(f.r.now())
			f.claim("candidate").DeletionTimestamp = &deleted
		},
		"non-vLLM claim": func(f preemptionPlannerFixture) {
			f.claim("candidate").Spec.Engine = "sglang"
		},
		"non-Active instance": func(f preemptionPlannerFixture) {
			f.claim("candidate").Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
		},
		"non-Active runtime": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").Phase = runtimePhaseSleeping
		},
		"not alive": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").Alive = false
		},
		"not ready": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").Ready = false
		},
		"missing request metrics": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").RequestMetricsObserved = false
		},
		"running request": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").RequestsRunning = 1
		},
		"waiting request": func(f preemptionPlannerFixture) {
			f.model("warm-1", "candidate").RequestsWaiting = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPreemptionPlannerFixture(t, 1,
				preemptionVictimFixture{"candidate", "warm-1", 1, 300, 100, recent, 60, true},
				preemptionVictimFixture{"busy", "warm-1", 1, 300, 100, at0758, 60, true},
			)
			f.model("warm-1", "busy").RequestsRunning = 1
			mutate(f)

			assert.Nil(t, f.plan(t))
		})
	}

	t.Run("newly idle lower priority", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"candidate", "warm-1", 1, 300, 100, recent, 60, true},
			preemptionVictimFixture{"busy", "warm-1", 1, 300, 100, at0758, 60, true},
		)
		f.model("warm-1", "busy").RequestsRunning = 1

		plan := f.plan(t)

		require.NotNil(t, plan)
		require.Len(t, plan.victims, 1)
		assert.Equal(t, "candidate", plan.victims[0].claim.Name)
		assert.WithinDuration(t, recent, plan.victims[0].idleSince, 0)
	})
}

func TestPlanSleepPreemptionProtectsPriorityBeforeVictimCount(t *testing.T) {
	f := newPreemptionPlannerFixture(t, 2,
		preemptionVictimFixture{"low-a", "warm-1", 1, 280, 50, at0758, 200, true},
		preemptionVictimFixture{"low-b", "warm-1", 1, 280, 50, at0758, 200, true},
		preemptionVictimFixture{"low-c", "warm-1", 1, 280, 50, at0758, 200, true},
		preemptionVictimFixture{"high", "warm-2", 90, 700, 100, at0759, 200, true},
	)

	plan := f.plan(t)

	require.NotNil(t, plan)
	assert.Equal(t, "warm-1", plan.pod.Name)
	require.Len(t, plan.victims, 3)
	assert.Equal(t, []string{"low-a", "low-b", "low-c"}, []string{
		plan.victims[0].claim.Name, plan.victims[1].claim.Name, plan.victims[2].claim.Name,
	})
}

func TestPlanSleepPreemptionOrdersVictimsByPriorityWithinCard(t *testing.T) {
	f := newPreemptionPlannerFixture(t, 1,
		preemptionVictimFixture{"priority-five", "warm-1", 5, 440, 100, at0758, 60, true},
		preemptionVictimFixture{"priority-one", "warm-1", 1, 440, 100, at0759, 60, true},
	)

	plan := f.plan(t)

	require.NotNil(t, plan)
	require.Len(t, plan.victims, 1)
	assert.Equal(t, "priority-one", plan.victims[0].claim.Name)
}

func comparisonPlan(pod string, known, cached bool, victims ...struct {
	name     string
	priority int32
	idle     time.Time
}) sleepPreemptionPlan {
	plan := sleepPreemptionPlan{
		pod:            &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod}},
		known:          known,
		artifactCached: cached,
	}
	for _, victim := range victims {
		plan.victims = append(plan.victims, sleepPreemptionVictim{
			claim:     &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: victim.name}},
			priority:  victim.priority,
			idleSince: victim.idle,
		})
	}
	return plan
}

func TestBetterSleepPreemptionPlanTieBreakers(t *testing.T) {
	v := func(name string, priority int32, idle time.Time) struct {
		name     string
		priority int32
		idle     time.Time
	} {
		return struct {
			name     string
			priority int32
			idle     time.Time
		}{name, priority, idle}
	}
	for name, plans := range map[string][2]sleepPreemptionPlan{
		"maximum victim priority": {
			comparisonPlan("pod", true, false, v("a", 1, at0758), v("b", 2, at0759)),
			comparisonPlan("pod", true, false, v("a", 1, at0758), v("b", 3, at0759)),
		},
		"int64 priority sum": {
			comparisonPlan("pod", true, false, v("a", math.MaxInt32-1, at0758), v("b", math.MaxInt32, at0759)),
			comparisonPlan("pod", true, false, v("a", math.MaxInt32, at0758), v("b", math.MaxInt32, at0759)),
		},
		"known before unknown": {
			comparisonPlan("pod", true, false, v("a", 1, at0758)),
			comparisonPlan("pod", false, false, v("a", 1, at0758)),
		},
		"victim count": {
			comparisonPlan("pod", true, false, v("a", 1, at0758)),
			comparisonPlan("pod", true, false, v("a", 1, at0758), v("b", 1, at0759)),
		},
		"idle time": {
			comparisonPlan("pod", true, false, v("a", 1, at0758)),
			comparisonPlan("pod", true, false, v("a", 1, at0759)),
		},
		"artifact cache": {
			comparisonPlan("pod", true, true, v("a", 1, at0758)),
			comparisonPlan("pod", true, false, v("a", 1, at0758)),
		},
		"pod name": {
			comparisonPlan("a-pod", true, false, v("victim", 1, at0758)),
			comparisonPlan("b-pod", true, false, v("victim", 1, at0758)),
		},
		"claim name": {
			comparisonPlan("pod", true, false, v("a", 1, at0758)),
			comparisonPlan("pod", true, false, v("b", 1, at0758)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, betterSleepPreemptionPlan(plans[0], plans[1]))
			assert.False(t, betterSleepPreemptionPlan(plans[1], plans[0]))
		})
	}
}

func TestPlanSleepPreemptionCapacitySimulation(t *testing.T) {
	t.Run("known sleeping footprint is subtracted", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 60, true},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 60, true},
		)
		plan := f.plan(t)
		require.NotNil(t, plan)
		assert.True(t, plan.known)
		require.Len(t, plan.victims, 1)
		assert.Equal(t, "a", plan.victims[0].claim.Name)
	})

	t.Run("unknown sleeping footprint selects one victim", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 0, false},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 60, true},
		)
		plan := f.plan(t)
		require.NotNil(t, plan)
		assert.False(t, plan.known)
		require.Len(t, plan.victims, 1)
		assert.Equal(t, "a", plan.victims[0].claim.Name)
	})

	t.Run("insufficient total reclaimable room", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 350, true},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 350, true},
		)
		assert.Nil(t, f.plan(t))
	})

	t.Run("topology mismatch", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 60, true},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 60, true},
		)
		f.preemptor.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
			"--tensor-parallel-size": "2",
		}}
		assert.Nil(t, f.plan(t))
	})

	t.Run("selector mismatch", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 60, true},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 60, true},
		)
		f.preemptor.Spec.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{
			constants.ModelPoolLabelName: "another-pool",
		}}
		assert.Nil(t, f.plan(t))
	})

	t.Run("preemptor too large for every card", func(t *testing.T) {
		f := newPreemptionPlannerFixture(t, 1,
			preemptionVictimFixture{"a", "warm-1", 1, 300, 100, at0758, 60, true},
			preemptionVictimFixture{"b", "warm-1", 1, 300, 100, at0759, 60, true},
		)
		f.preemptor.Spec.PerGPU = claimOnPod("huge", "", modelv1alpha1.ModelClaimActive, 1000, 100).Spec.PerGPU
		assert.Nil(t, f.plan(t))
	})
}
