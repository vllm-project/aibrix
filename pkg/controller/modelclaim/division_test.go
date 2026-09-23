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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestCardDivisionStateDividesACardOncePerRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	other := types.NamespacedName{Namespace: testNamespace, Name: "warm-2"}

	assert.True(t, divisions.due(card))
	assert.False(t, divisions.due(card))
	assert.True(t, divisions.due(other), "each card has a round of its own")

	now = now.Add(DefaultRequeueDuration)
	assert.True(t, divisions.due(card))
}

func TestCardDivisionStateForgetsCardsLongGone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	gone := types.NamespacedName{Namespace: testNamespace, Name: "deleted"}
	require.True(t, divisions.due(gone))

	now = now.Add(30 * DefaultRequeueDuration)
	divisions.due(types.NamespacedName{Namespace: testNamespace, Name: "warm-1"})

	_, remembered := divisions.lastRound[gone]
	assert.False(t, remembered)
}

// aCardAndOneEngineOnIt is a realistically sized card carrying one claim whose
// engine is already held to limitBytes and has mapped usedBytes.
func aCardAndOneEngineOnIt(t *testing.T, limitBytes, usedBytes int64) (*ModelClaimReconciler, *fakeRuntime, *corev1.Pod) {
	t.Helper()
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	solo := withFinalizer(claimOnPod("solo", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	solo.Status.Instances[0].Port = 9001
	solo.Status.Instances[0].KVLimitBytes = limitBytes
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("solo", usedBytes, limitBytes)}
	r, runtime := newReconciler(t, solo, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pod
}

func TestReconcileGivesACardsSpareRoomToTheEngineOnIt(t *testing.T) {
	r, runtime, _ := aCardAndOneEngineOnIt(t, 10<<30, 4<<30)

	reconcileOnce(t, r, "solo")

	// 80 GiB less a 20 GiB footprint leaves 60 GiB, all of it this engine's.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(60)<<30, runtime.kvLimitCalls[0].LimitBytes)
	got := getModel(t, r, "solo")
	assert.Equal(t, int64(60)<<30, got.Status.Instances[0].KVLimitBytes)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
	// Following load is routine, so it is logged rather than raised on the
	// claim every round.
	for _, event := range recordedEvents(t, r) {
		assert.NotContains(t, event, "KVLimitSet")
	}
}

func TestReconcileLeavesACardAloneWhenItHasBarelyDrifted(t *testing.T) {
	// A hundred mebibytes short of its share, well inside one page bundle.
	current := int64(60)<<30 - 100<<20
	r, runtime, _ := aCardAndOneEngineOnIt(t, current, 4<<30)

	reconcileOnce(t, r, "solo")

	assert.Empty(t, runtime.kvLimitCalls)
	got := getModel(t, r, "solo")
	assert.Equal(t, current, got.Status.Instances[0].KVLimitBytes)
}

func TestReconcileGivesTheBusierEngineMoreOfTheCard(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	idle := withFinalizer(claimOnPod("idle", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	idle.Status.Instances[0].KVLimitBytes = 20 << 30
	busy := claimOnPod("busy", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	busy.Status.Instances[0].KVLimitBytes = 20 << 30
	serving := engineHolding("busy", 4<<30, 20<<30)
	serving.RequestsRunning = 4
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("idle", 4<<30, 20<<30), serving}
	r, runtime := newReconciler(t, idle, busy, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, "idle")

	// Two 20 GiB footprints and two 4 GiB floors leave 32 GiB to share. The
	// busy engine weighs five to the idle one's one, and the byte left over by
	// the division goes to the first claim by name.
	spare := int64(32) << 30
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, "idle", runtime.kvLimitCalls[0].ModelName, "the shrink comes first")
	assert.Equal(t, int64(4)<<30+spare/6, runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, "busy", runtime.kvLimitCalls[1].ModelName)
	assert.Equal(t, int64(4)<<30+spare*5/6+1, runtime.kvLimitCalls[1].LimitBytes)
	assert.Equal(t, int64(4)<<30+spare*5/6+1, getModel(t, r, "busy").Status.Instances[0].KVLimitBytes)
}

func TestReconcileHoldsASleepingEngineToWhatItHolds(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	awake := withFinalizer(claimOnPod("awake", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	awake.Status.Instances[0].Port = 9001
	awake.Status.Instances[0].KVLimitBytes = 20 << 30
	asleep := claimOnPod("asleep", pod.Name, modelv1alpha1.ModelClaimSleeping, 20<<30, 4<<30)
	asleep.Status.Instances[0].KVLimitBytes = 20 << 30
	sleeping := engineHolding("asleep", 0, 20<<30)
	sleeping.Phase = runtimePhaseSleeping
	sleeping.Ready = false
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("awake", 4<<30, 20<<30), sleeping}
	r, runtime := newReconciler(t, awake, asleep, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, "awake")

	// The sleeping engine serves nothing, so it keeps only its 4 GiB floor, and
	// all 32 GiB the card has spare go to the engine that is awake.
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, "asleep", runtime.kvLimitCalls[0].ModelName)
	assert.Equal(t, int64(4)<<30, runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, "awake", runtime.kvLimitCalls[1].ModelName)
	assert.Equal(t, int64(36)<<30, runtime.kvLimitCalls[1].LimitBytes)
	assert.Equal(t, int64(4)<<30, getModel(t, r, "asleep").Status.Instances[0].KVLimitBytes)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "awake").Status.Instances[0].Phase)
}

func TestReconcileDividesACardOnlyOncePerRound(t *testing.T) {
	r, runtime, pod := aCardAndOneEngineOnIt(t, 10<<30, 4<<30)
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })

	reconcileOnce(t, r, "solo")
	require.Len(t, runtime.kvLimitCalls, 1)

	// The engine is held to less than its share again, which keeps its route
	// and is the division's to correct, not the health loop's.
	runtime.snapshots[pod.Status.PodIP].Models[0].KVCapacityBytes = 30 << 30
	reconcileOnce(t, r, "solo")
	assert.Len(t, runtime.kvLimitCalls, 1, "a card is divided at most once per round")

	now = now.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, "solo")
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, int64(60)<<30, runtime.kvLimitCalls[1].LimitBytes)
}
