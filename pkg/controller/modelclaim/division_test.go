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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	_, remembered = divisions.attemptedFor[gone]
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

// A scrape of an engine's metrics that timed out says nothing about its load.
// The engine may be too busy to answer, so it is not squeezed as idle.
func TestReconcileWeighsAServingEngineWhoseMetricsWereNotReadAsBusy(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	idle := withFinalizer(claimOnPod("idle", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	idle.Status.Instances[0].KVLimitBytes = 20 << 30
	unread := claimOnPod("unread", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	unread.Status.Instances[0].KVLimitBytes = 20 << 30
	unreadEngine := engineHolding("unread", 4<<30, 20<<30)
	unreadEngine.RequestMetricsObserved = false
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("idle", 4<<30, 20<<30), unreadEngine}
	r, runtime := newReconciler(t, idle, unread, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, "idle")

	// Weighed as busy as an engine counts, it is given five parts of the 32 GiB
	// spare to the idle engine's one.
	spare := int64(32) << 30
	assert.Equal(t, int64(4)<<30+spare*5/6, getModel(t, r, "unread").Status.Instances[0].KVLimitBytes)
	assert.Equal(t, int64(4)<<30+spare/6+1, getModel(t, r, "idle").Status.Instances[0].KVLimitBytes)
}

// The runtime goes on listing an engine it has given up on, dead. Its room is
// back with the card, so the engine left serving is given all of it.
func TestReconcileGivesAFailedEnginesRoomBack(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	awake := withFinalizer(claimOnPod("awake", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	awake.Status.Instances[0].KVLimitBytes = 30 << 30
	failed := claimOnPod("failed", pod.Name, modelv1alpha1.ModelClaimFailed, 20<<30, 4<<30)
	failed.Status.Instances[0].KVLimitBytes = 30 << 30
	dead := engineHolding("failed", 10<<30, 30<<30)
	dead.Phase = runtimePhaseFailed
	dead.Alive = false
	dead.Ready = false
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("awake", 4<<30, 30<<30), dead}
	r, runtime := newReconciler(t, awake, failed, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, "awake")

	// Alone on the card, it is held to the card less its own footprint.
	assert.Equal(t, int64(60)<<30, getModel(t, r, "awake").Status.Instances[0].KVLimitBytes)
}

// A grow is written after every new limit is recorded. So when the reading
// that should confirm the grow is lost, the engine is already recorded at its
// new share, and it keeps its route.
func TestReconcileKeepsAnEngineRoutedWhenItsGrowIsNotConfirmed(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	idle := withFinalizer(claimOnPod("idle", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	idle.Status.Instances[0].KVLimitBytes = 30 << 30
	idle.Status.Instances[0].Port = 9001
	busy := withFinalizer(claimOnPod("busy", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	busy.Status.Instances[0].KVLimitBytes = 10 << 30
	busy.Status.Instances[0].Port = 9001
	serving := engineHolding("busy", 4<<30, 10<<30)
	serving.RequestsRunning = 4
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("idle", 4<<30, 30<<30), serving}
	r, runtime := newReconciler(t, idle, busy, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	writes := 0
	runtime.onKVLimit = func() {
		writes++
		if writes == 2 {
			// The grow reached the engine, and the reading back is lost.
			runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}
		}
	}

	reconcileOnce(t, r, "idle")

	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, "busy", runtime.kvLimitCalls[1].ModelName)
	assert.Equal(t, runtime.kvLimitCalls[1].LimitBytes, getModel(t, r, "busy").Status.Instances[0].KVLimitBytes,
		"the grow is recorded before it is written")

	runtime.nilSnapshots = nil
	runtime.onKVLimit = nil
	drainEvents(t, r)
	reconcileOnce(t, r, "busy")

	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "busy").Status.Instances[0].Phase)
	for _, event := range drainEvents(t, r) {
		assert.NotContains(t, event, "KVLimitNotHeld")
	}
}

// The cache can lag a record that a division in another claim's pass has just
// written. The health loop reads the record fresh before it acts on a limit
// that is not in force, so an engine that was just grown is not pulled back.
func TestReconcileReadsARecordFreshBeforeActingOnIt(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	cached := withFinalizer(claimOnPod("busy", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	cached.Status.Instances[0].Port = 9001
	cached.Status.Instances[0].KVLimitBytes = 10 << 30
	// A division has grown the engine and recorded 30 GiB; the cache still
	// shows the 10 GiB before it.
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("busy", 4<<30, 30<<30)}
	r, runtime := newReconciler(t, cached, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	fresh := cached.DeepCopy()
	fresh.Status.Instances[0].KVLimitBytes = 30 << 30
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(fresh, pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()

	reconcileOnce(t, r, "busy")

	for _, call := range runtime.kvLimitCalls {
		assert.NotEqual(t, int64(10)<<30, call.LimitBytes, "the engine must not be pulled back to a stale record")
	}
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "busy").Status.Instances[0].Phase)
	for _, event := range drainEvents(t, r) {
		assert.NotContains(t, event, "KVLimitNotHeld")
	}
}

// countingReader counts the listings a reconciler makes around the cache.
type countingReader struct {
	client.Reader
	lists int
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Reader.List(ctx, list, opts...)
}

// Whether a card is due is told from the cache. The claims are listed around
// it only when some card is due, so a pass with nothing to divide costs no
// read of the API server.
func TestReconcileListsClaimsFreshOnlyWhenACardIsDue(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	alone := withFinalizer(claimOnPod("alone", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
	alone.Status.Instances[0].KVLimitBytes = 60 << 30
	alone.Status.Instances[0].Port = 9001
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("alone", 4<<30, 60<<30)}
	r, runtime := newReconciler(t, alone, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	reader := &countingReader{Reader: r.Client}
	r.APIReader = reader

	reconcileOnce(t, r, "alone")
	first := reader.lists
	require.Positive(t, first, "the card's first round lists the claims")

	reconcileOnce(t, r, "alone")
	assert.Equal(t, first, reader.lists, "nothing is due, so nothing is listed")
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
	divisions.divided(card, "a")

	divide, _ = divisions.due(card, "a")
	assert.False(t, divide)

	divide, changed = divisions.due(card, "a,b")
	assert.True(t, divide, "a card whose engines changed does not wait for the round")
	assert.True(t, changed)
	divisions.divided(card, "a,b")

	divide, _ = divisions.due(card, "a,b")
	assert.False(t, divide, "what the card was divided for is remembered")
}

func TestCardDivisionStateKeepsAChangePendingUntilTheCardIsDivided(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	divisions.divided(card, "a,b")

	divide, changed := divisions.due(card, "a")
	require.True(t, divide)
	require.True(t, changed)

	// That division could not be carried out, so nothing records it. The
	// change is not tried again on every pass, only by the round, and then
	// still as a change.
	divide, _ = divisions.due(card, "a")
	assert.False(t, divide)
	now = now.Add(DefaultRequeueDuration)
	divide, changed = divisions.due(card, "a")
	assert.True(t, divide)
	assert.True(t, changed, "a change not yet divided for is still a change")

	divisions.divided(card, "a")
	now = now.Add(DefaultRequeueDuration)
	divide, changed = divisions.due(card, "a")
	assert.True(t, divide)
	assert.False(t, changed, "once divided for, the card is back to its rounds")
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

func TestReconcileAnnouncesTheRoomAnEngineLeftOnceItHasExited(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	recordedEvents(t, r)

	// The claim goes, and its engine takes a moment to exit. Until it does,
	// the card runs an engine no claim answers for, and cannot be divided.
	require.NoError(t, r.Delete(context.Background(), getModel(t, r, "leaves")))
	reconcileOnce(t, r, "stays")
	require.Empty(t, runtime.kvLimitCalls)

	snapshot := runtime.snapshots[pod.Status.PodIP]
	snapshot.Models = snapshot.Models[:1]
	*clock = clock.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, "stays")

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(60)<<30, runtime.kvLimitCalls[0].LimitBytes)
	told := false
	for _, event := range recordedEvents(t, r) {
		if strings.Contains(event, "KVLimitSet") && strings.Contains(event, "dividing the card between 1 engine(s)") {
			told = true
		}
	}
	assert.True(t, told, "room freed by a change of engines is announced, even when the card could only be divided a round later")
}

func TestReconcileTriesAFailedDivisionAgainOnlyByTheRound(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	leave(t, r, runtime, pod)
	// The write reaches no segment, so the reading that should confirm it does
	// not, and the division fails.
	runtime.deafToKVLimits = true
	tries := func() int {
		tries := 0
		for _, call := range runtime.kvLimitCalls {
			if call.LimitBytes == int64(60)<<30 {
				tries++
			}
		}
		return tries
	}

	reconcileOnce(t, r, "stays")
	require.Equal(t, 1, tries())
	reconcileOnce(t, r, "stays")
	assert.Equal(t, 1, tries(), "a failed division waits for the round rather than every pass")

	*clock = clock.Add(DefaultRequeueDuration)
	runtime.deafToKVLimits = false
	reconcileOnce(t, r, "stays")
	require.Equal(t, 2, tries())
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

func TestReconcileReadsEachRuntimeOnceAPass(t *testing.T) {
	// The engine already holds its share, so nothing is written.
	r, runtime, _ := aCardAndOneEngineOnIt(t, 60<<30, 4<<30)

	reconcileOnce(t, r, "solo")

	require.Empty(t, runtime.kvLimitCalls)
	assert.Equal(t, 1, runtime.snapshotCalls,
		"the health check and the division should share one reading of the runtime")
}

func TestReconcileReadsARuntimeAgainOnlyAfterChangingIt(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	// One reading for the account and the ranking, one to confirm the
	// neighbour's shrink, and one after the engine was started, which the
	// health check needs to see the new engine at all.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, 3, runtime.snapshotCalls)
	assert.Len(t, runtime.activateCalls, 1, "a reading from before the start would show no engine")
}

func TestReconcileDoesNotReadACardWithNothingOnIt(t *testing.T) {
	r, runtime, _ := aCardAndOneEngineOnIt(t, 60<<30, 4<<30)
	empty, emptySnapshot := sizedWarmPod("warm-2", "10.0.0.2", 80<<30)
	require.NoError(t, r.Create(context.Background(), empty))
	runtime.snapshots[empty.Status.PodIP] = emptySnapshot

	reconcileOnce(t, r, "solo")

	assert.Equal(t, 1, runtime.snapshotCalls, "a card with no instance has nothing to divide")
}

// aCardWithAnEngineThatTakesNoLimit is a card of three engines held to even
// shares. The engine "deaf" reports the limit it had whatever is written to
// it, so every division that moves it fails at the reading.
func aCardWithAnEngineThatTakesNoLimit(
	t *testing.T,
	newcomerPhase modelv1alpha1.ModelClaimPhase,
) (*ModelClaimReconciler, *fakeRuntime, *RuntimeSnapshot, *time.Time) {
	t.Helper()
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	even := int64(4)<<30 + (int64(8)<<30)/3
	claims := []client.Object{pod}
	for i, name := range []string{"deaf", "busy", "newcomer"} {
		phase := modelv1alpha1.ModelClaimActive
		if name == "newcomer" {
			phase = newcomerPhase
		}
		claim := withFinalizer(claimOnPod(name, pod.Name, phase, 20<<30, 4<<30))
		claim.Status.Instances[0].Port = int32(9001 + i)
		claim.Status.Instances[0].KVLimitBytes = even
		claims = append(claims, claim)
		engine := engineHolding(name, 0, even)
		engine.Port = int32(9001 + i)
		snapshot.Models = append(snapshot.Models, engine)
	}
	// The busy engine is to grow, so the other two are to shrink.
	snapshot.Models[1].KVUsedBytes = 4 << 30
	snapshot.Models[1].RequestsRunning = 4
	r, runtime := newReconciler(t, claims...)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	runtime.onKVLimit = func() { snapshot.Models[0].KVCapacityBytes = even }
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })
	return r, runtime, snapshot, &now
}

func TestReconcileRoutesAReadyEngineThoughItsCardCannotBeDivided(t *testing.T) {
	r, _, snapshot, clock := aCardWithAnEngineThatTakesNoLimit(t, modelv1alpha1.ModelClaimActivating)

	// A neighbour's pass comes first, and its division of the card fails.
	reconcileOnce(t, r, "busy")
	*clock = clock.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, "newcomer")

	// The failed division took its shrink of the newcomer back, so the
	// newcomer is held to its record, and is routed.
	got := getModel(t, r, "newcomer")
	assert.Equal(t, got.Status.Instances[0].KVLimitBytes, snapshot.Models[2].KVCapacityBytes)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
}

func TestReconcileLeavesEveryEngineAsItWasHeldWhenACardCannotBeDivided(t *testing.T) {
	r, _, snapshot, clock := aCardWithAnEngineThatTakesNoLimit(t, modelv1alpha1.ModelClaimActive)
	held := snapshot.Models[2].KVCapacityBytes

	for round := 0; round < 3; round++ {
		reconcileOnce(t, r, "busy")
		*clock = clock.Add(DefaultRequeueDuration)
	}

	for i := range snapshot.Models {
		assert.Equal(t, held, snapshot.Models[i].KVCapacityBytes, snapshot.Models[i].ModelName)
	}
}

// failingKVLimits fails the writes of a KV limit whose number is listed,
// counted from one.
type failingKVLimits struct {
	*fakeRuntime
	failing map[int]bool
}

func (f *failingKVLimits) SetKVLimit(
	ctx context.Context,
	podIP string,
	port int,
	req *SetKVLimitRequest,
) (*RuntimeOperationResponse, error) {
	if f.failing[len(f.kvLimitCalls)+1] {
		f.kvLimitCalls = append(f.kvLimitCalls, *req)
		return nil, errors.New("runtime did not take the limit")
	}
	return f.fakeRuntime.SetKVLimit(ctx, podIP, port, req)
}

// Only what a step wrote is taken back. An engine the step did not reach is
// held as before, so there is nothing to write to it.
func TestArrangeCardTakesBackOnlyWhatItWrote(t *testing.T) {
	r, runtime, _, _ := aCardWithAnEngineThatTakesNoLimit(t, modelv1alpha1.ModelClaimActive)
	runtime.onKVLimit = nil
	// The shrinks are for "deaf" and "newcomer". The second write fails.
	r.Runtime = &failingKVLimits{fakeRuntime: runtime, failing: map[int]bool{2: true}}

	reconcileOnce(t, r, "busy")

	var written []string
	for _, call := range runtime.kvLimitCalls {
		written = append(written, call.ModelName+" "+strings.SplitN(call.OperationID, "/", 2)[0])
	}
	assert.Equal(t, []string{"deaf kv-plan", "newcomer kv-plan", "deaf kv-plan-back"}, written)
}

// A runtime that does not take a limit back is not asked for the next one.
func TestArrangeCardStopsTakingBackAfterACallThatFails(t *testing.T) {
	r, runtime, _, _ := aCardWithAnEngineThatTakesNoLimit(t, modelv1alpha1.ModelClaimActive)
	// Both shrinks are written, and the reading does not confirm them. The
	// first call that takes one back fails.
	r.Runtime = &failingKVLimits{fakeRuntime: runtime, failing: map[int]bool{3: true}}

	reconcileOnce(t, r, "busy")

	require.Len(t, runtime.kvLimitCalls, 3)
	assert.True(t, strings.HasPrefix(runtime.kvLimitCalls[2].OperationID, "kv-plan-back/"))
}

func TestCardDivisionStateCountsFailuresUntilADivisionWorks(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}

	assert.Equal(t, 1, divisions.failedAgain(card))
	assert.Equal(t, 2, divisions.failedAgain(card))
	divisions.divided(card, "a")
	assert.Equal(t, 1, divisions.failedAgain(card), "a division that works starts the count again")
}

func TestReconcileWarnsOnceWhenACardKeepsFailingToBeDivided(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	recordedEvents(t, r)

	// "stays" gets busy, so each round would move the card, and no write
	// reaches a segment, so no division is ever confirmed.
	snapshot := runtime.snapshots[pod.Status.PodIP]
	snapshot.Models[0].RequestsRunning = 4
	runtime.deafToKVLimits = true
	warned := map[string]int{}
	countWarnings := func() {
		for _, event := range recordedEvents(t, r) {
			if !strings.Contains(event, "KVLimitFailed") {
				continue
			}
			for _, name := range []string{"stays", "leaves"} {
				if strings.Contains(event, "model "+name+" ") {
					warned[name]++
				}
			}
		}
	}
	for try := 1; try <= 4; try++ {
		*clock = clock.Add(DefaultRequeueDuration)
		reconcileOnce(t, r, "stays")
		countWarnings()
		if try < 3 {
			assert.Empty(t, warned, "no warning after %d failed division(s)", try)
		}
	}
	assert.Equal(t, map[string]int{"stays": 1, "leaves": 1}, warned,
		"each claim on the card is warned once, on the third failure in a row")

	// A division that works ends the episode quietly.
	runtime.deafToKVLimits = false
	*clock = clock.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, "stays")
	countWarnings()
	assert.Equal(t, map[string]int{"stays": 1, "leaves": 1}, warned)
	assert.Equal(t, int64(4)<<30+(32<<30)*5/6, getModel(t, r, "stays").Status.Instances[0].KVLimitBytes)
}
