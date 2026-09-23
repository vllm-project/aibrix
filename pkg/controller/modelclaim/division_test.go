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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestCardDivisionStateDividesACardOncePerRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	other := types.NamespacedName{Namespace: testNamespace, Name: "warm-2"}
	due := func(card types.NamespacedName) bool {
		divide, _ := divisions.due(card, "unchanged")
		return divide
	}

	assert.True(t, due(card))
	assert.False(t, due(card))
	assert.True(t, due(other), "each card has a round of its own")

	now = now.Add(DefaultRequeueDuration)
	assert.True(t, due(card))
}

func TestCardDivisionStateForgetsCardsLongGone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	gone := types.NamespacedName{Namespace: testNamespace, Name: "deleted"}
	divide, _ := divisions.due(gone, "a")
	require.True(t, divide)

	now = now.Add(30 * DefaultRequeueDuration)
	divisions.due(types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, "b")

	_, remembered := divisions.lastRound[gone]
	assert.False(t, remembered)
	_, remembered = divisions.compositions[gone]
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

func TestCardDivisionStateDividesACardWhoseEnginesChangedAtOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}

	divide, changed := divisions.due(card, "a")
	assert.True(t, divide, "a card seen for the first time is divided by the round")
	assert.False(t, changed, "a card seen for the first time is not taken as changed")

	divide, _ = divisions.due(card, "a")
	assert.False(t, divide)

	divide, changed = divisions.due(card, "a,b")
	assert.True(t, divide, "a card whose engines changed does not wait for the round")
	assert.True(t, changed)

	divide, _ = divisions.due(card, "a,b")
	assert.False(t, divide, "what the card was divided for is remembered")
}

func TestCardDivisionStateCountsAPlacementAsTheCardsDivision(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}

	divisions.divided(card, "a,b")

	divide, _ := divisions.due(card, "a,b")
	assert.False(t, divide)
	divide, changed := divisions.due(card, "a")
	assert.True(t, divide)
	assert.True(t, changed)
}

func TestCardCompositionDescribesTheEnginesOnOneCard(t *testing.T) {
	first := claimOnPod("first", "warm-1", modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	second := claimOnPod("second", "warm-1", modelv1alpha1.ModelClaimSleeping, 20<<30, 4<<30)
	elsewhere := claimOnPod("elsewhere", "warm-2", modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*second, *elsewhere, *first}}
	reordered := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*first, *second}}

	composition := cardComposition(claims, "warm-1")

	assert.Equal(t, composition, cardComposition(reordered, "warm-1"), "order does not matter")
	assert.NotContains(t, composition, "elsewhere")
	assert.Contains(t, composition, "second/asleep")

	second.Status.Instances[0].Phase = modelv1alpha1.ModelClaimActive
	assert.NotEqual(t, composition, cardComposition(
		&modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*first, *second}}, "warm-1"),
		"a wake changes the card")

	second.Spec.PerGPU.KVFloor = *resource.NewQuantity(8<<30, resource.BinarySI)
	second.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
	assert.NotEqual(t, composition, cardComposition(
		&modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*first, *second}}, "warm-1"),
		"a new declaration changes the card")
}

// twoEnginesSharingACard is an 80 GiB card divided evenly between two idle
// claims, "stays" and "leaves", each held to 20 GiB with its 4 GiB floor
// mapped. The clock the divisions read is the one returned.
func twoEnginesSharingACard(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *corev1.Pod, *time.Time) {
	t.Helper()
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	stays := withFinalizer(claimOnPod("stays", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	stays.Status.Instances[0].Port = 9001
	stays.Status.Instances[0].KVLimitBytes = 20 << 30
	leaves := claimOnPod("leaves", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	leaves.Status.Instances[0].KVLimitBytes = 20 << 30
	snapshot.Models = []RuntimeSnapshotModel{
		engineHolding("stays", 4<<30, 20<<30),
		engineHolding("leaves", 4<<30, 20<<30),
	}
	r, runtime := newReconciler(t, stays, leaves, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })
	return r, runtime, pod, &now
}

// leave deletes the claim "leaves" and stops its engine.
func leave(t *testing.T, r *ModelClaimReconciler, runtime *fakeRuntime, pod *corev1.Pod) {
	t.Helper()
	require.NoError(t, r.Delete(context.Background(), getModel(t, r, "leaves")))
	snapshot := runtime.snapshots[pod.Status.PodIP]
	snapshot.Models = snapshot.Models[:1]
}

func TestReconcileDividesACardAgainAtOnceWhenAnEngineLeaves(t *testing.T) {
	r, runtime, pod, _ := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	require.Empty(t, runtime.kvLimitCalls, "an even split needs no write")

	leave(t, r, runtime, pod)
	reconcileOnce(t, r, "stays")

	// Within the same round, the engine left gets the whole card less its own
	// footprint, and its claim is told.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, "stays", runtime.kvLimitCalls[0].ModelName)
	assert.Equal(t, int64(60)<<30, runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, int64(60)<<30, getModel(t, r, "stays").Status.Instances[0].KVLimitBytes)
	told := false
	for _, event := range recordedEvents(t, r) {
		if strings.Contains(event, "KVLimitSet") && strings.Contains(event, "stays") {
			told = true
		}
	}
	assert.True(t, told, "a division after the engines change is raised on the claims it moves")
}

func TestReconcileTriesAFailedDivisionAgainOnlyByTheRound(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	leave(t, r, runtime, pod)
	// The write reaches no segment, so the reading that should confirm it does
	// not, and the division fails.
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, "stays")
	require.Len(t, runtime.kvLimitCalls, 1)
	reconcileOnce(t, r, "stays")
	assert.Len(t, runtime.kvLimitCalls, 1, "a failed division waits for the round rather than every pass")

	*clock = clock.Add(DefaultRequeueDuration)
	runtime.deafToKVLimits = false
	reconcileOnce(t, r, "stays")
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, int64(60)<<30, runtime.kvLimitCalls[1].LimitBytes)
}

func TestReconcileDividesACardAgainAtOnceWhenAnEngineWakes(t *testing.T) {
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
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })
	reconcileOnce(t, r, "awake")
	require.Len(t, runtime.kvLimitCalls, 2)

	// The engine wakes, and its own claim's health loop marks it active.
	snapshot.Models[1].Phase = runtimePhaseActive
	snapshot.Models[1].Ready = true
	woken := getModel(t, r, "asleep")
	woken.Status.Instances[0].Phase = modelv1alpha1.ModelClaimActive
	require.NoError(t, r.Status().Update(context.Background(), woken))
	runtime.kvLimitCalls = nil
	reconcileOnce(t, r, "awake")

	// Within the same round, the 32 GiB spare is shared between the two again.
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, "awake", runtime.kvLimitCalls[0].ModelName)
	assert.Equal(t, int64(20)<<30, runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, "asleep", runtime.kvLimitCalls[1].ModelName)
	assert.Equal(t, int64(20)<<30, runtime.kvLimitCalls[1].LimitBytes)
}

func TestReconcileGivesTheRoomBackWhenTheEnginePlacedCannotStart(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	runtime.failActivate = true

	reconcileOnce(t, r, pm.Name)

	// The card was divided to make room for the model, and its engine then did
	// not start. The card is divided again in the same pass, and the neighbour,
	// alone on it, gets the whole card less its footprint.
	require.Len(t, runtime.activateCalls, 1)
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, int64(200), runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, int64(700), runtime.kvLimitCalls[1].LimitBytes)
	assert.Equal(t, int64(700), getModel(t, r, "neighbour").Status.Instances[0].KVLimitBytes)
	assert.Empty(t, getModel(t, r, pm.Name).Status.Instances)
}

func TestReconcileDividesACardOnceWhenAModelIsPlacedOnIt(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })

	reconcileOnce(t, r, pm.Name)
	reconcileOnce(t, r, pm.Name)

	// Placement divided the card for the engines now on it, so neither pass
	// takes the new model for a change to divide the card for again.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(200), runtime.kvLimitCalls[0].LimitBytes)
}
