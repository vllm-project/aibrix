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

// aCardOfTwoEngines is an 80 GiB card with two claims on it, "busy" and
// "idle", both active. Each has a 20 GiB footprint and a 4 GiB floor, so the
// card has 32 GiB to share out. The clock the divisions read is the one
// returned.
func aCardOfTwoEngines(
	t *testing.T,
	busy, idle RuntimeSnapshotModel,
) (*ModelClaimReconciler, *fakeRuntime, *RuntimeSnapshot, *time.Time) {
	t.Helper()
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	claims := make([]client.Object, 0, 3)
	for _, engine := range []RuntimeSnapshotModel{busy, idle} {
		claim := withFinalizer(claimOnPod(engine.ModelName, pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
		claim.Status.Instances[0].Port = engine.Port
		claim.Status.Instances[0].KVLimitBytes = engine.KVCapacityBytes
		claims = append(claims, claim)
	}
	snapshot.Models = []RuntimeSnapshotModel{busy, idle}
	r, runtime := newReconciler(t, append(claims, pod)...)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	now := time.Unix(1_700_000_000, 0)
	r.Divisions = newCardDivisionState(func() time.Time { return now })
	return r, runtime, snapshot, &now
}

// nextRound moves the clock to the card's next round and reconciles a claim.
func nextRound(t *testing.T, r *ModelClaimReconciler, clock *time.Time, claim string) {
	t.Helper()
	*clock = clock.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, claim)
}

func TestReconcileLeavesACardAloneWhileNoEngineIsShortOfKV(t *testing.T) {
	r, runtime, snapshot, clock := aCardOfTwoEngines(t,
		engineHolding("busy", 4<<30, 20<<30), engineHolding("idle", 4<<30, 20<<30))
	// The card's first round finds it divided evenly, and notes it.
	reconcileOnce(t, r, "idle")
	require.Empty(t, runtime.kvLimitCalls)

	// Requests in flight, with most of the engine's limit still unmapped.
	snapshot.Models[0].RequestsRunning = 4
	nextRound(t, r, clock, "idle")

	// A limit is a ceiling. Moving it while nobody is near it would only cost
	// the writes, round after round, as the requests in flight come and go.
	assert.Empty(t, runtime.kvLimitCalls)
	assert.Equal(t, int64(20)<<30, getModel(t, r, "busy").Status.Instances[0].KVLimitBytes)
}

func TestReconcileDividesACardWhateverItsLoadWhenItFirstSeesIt(t *testing.T) {
	// A controller that has just started finds a card as an engine's sleep
	// left it, with that engine awake again. Both engines serve, both hold
	// their records, and neither is near its limit.
	busy := engineHolding("busy", 4<<30, 36<<30)
	busy.RequestsRunning = 1
	woken := engineHolding("idle", 1<<30, 4<<30)
	woken.RequestsRunning = 1
	r, runtime, _, _ := aCardOfTwoEngines(t, busy, woken)

	reconcileOnce(t, r, "idle")

	// Nothing is known of what the card was last divided for, so the change
	// cannot be seen. The first round divides the card all the same.
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, int64(20)<<30, getModel(t, r, "busy").Status.Instances[0].KVLimitBytes)
	assert.Equal(t, int64(20)<<30, getModel(t, r, "idle").Status.Instances[0].KVLimitBytes)
}

func TestReconcileFinishesADivisionThatWasLeftUnfinished(t *testing.T) {
	r, runtime, snapshot, clock := aCardOfTwoEngines(t,
		engineHolding("busy", 4<<30, 20<<30), engineHolding("idle", 4<<30, 20<<30))
	reconcileOnce(t, r, "idle")
	require.Empty(t, runtime.kvLimitCalls)

	// A grow is recorded before it is written. This one did not take, so the
	// engine is held to less than its record. Both engines serve, and neither
	// is near its limit.
	snapshot.Models[0].KVCapacityBytes = 10 << 30
	snapshot.Models[0].RequestsRunning = 1
	snapshot.Models[1].RequestsRunning = 1
	nextRound(t, r, clock, "idle")

	// The health loop does not grow an engine that is routed. Only a division
	// does, so the round carries one out.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, "busy", runtime.kvLimitCalls[0].ModelName)
	assert.Equal(t, int64(20)<<30, runtime.kvLimitCalls[0].LimitBytes)
}

func TestReconcileGivesAnEngineItsShareBackOnceTheCardIsAtRest(t *testing.T) {
	// A burst on "busy" has the card divided five parts to one.
	spare := int64(32) << 30
	serving := engineHolding("busy", 4<<30, int64(4)<<30+spare*5/6+1)
	serving.RequestsRunning = 4
	r, runtime, snapshot, clock := aCardOfTwoEngines(t,
		serving, engineHolding("idle", 4<<30, int64(4)<<30+spare/6))
	reconcileOnce(t, r, "idle")
	require.Empty(t, runtime.kvLimitCalls, "the card is divided as its load asks")

	// The burst is over, and nothing is in flight on the card.
	snapshot.Models[0].RequestsRunning = 0
	nextRound(t, r, clock, "idle")

	// Left as it was, "idle" would start its own burst on a sixth of the
	// spare room, with most of the card held by an engine that serves nothing.
	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, int64(20)<<30, getModel(t, r, "busy").Status.Instances[0].KVLimitBytes)
	assert.Equal(t, int64(20)<<30, getModel(t, r, "idle").Status.Instances[0].KVLimitBytes)
}

// divisionsIn counts the rounds, of the ones played, in which a limit was
// written. Before each round, load sets what the runtime reports.
func divisionsIn(
	t *testing.T,
	r *ModelClaimReconciler,
	runtime *fakeRuntime,
	clock *time.Time,
	rounds int,
	load func(round int),
) int {
	t.Helper()
	divisions := 0
	for round := 0; round < rounds; round++ {
		load(round)
		before := len(runtime.kvLimitCalls)
		nextRound(t, r, clock, "idle")
		if len(runtime.kvLimitCalls) > before {
			divisions++
		}
	}
	return divisions
}

func TestReconcileDividesACardOnceForAnEngineThatStaysShort(t *testing.T) {
	r, runtime, snapshot, clock := aCardOfTwoEngines(t,
		engineHolding("busy", 4<<30, 20<<30), engineHolding("idle", 1<<30, 20<<30))
	reconcileOnce(t, r, "idle")
	require.Empty(t, runtime.kvLimitCalls)

	// "busy" keeps 20 GiB mapped, which is more than half of any limit it is
	// given. "idle" has mapped little, and has a request in flight at every
	// other reading.
	snapshot.Models[0].KVUsedBytes = 20 << 30
	snapshot.Models[0].RequestsRunning = 4
	divisions := divisionsIn(t, r, runtime, clock, 12, func(round int) {
		snapshot.Models[1].RequestsRunning = int64(round % 2)
	})

	// The first round gives the engine that is short its share. After that,
	// a plan that moves with the other engine's requests gives it nothing
	// more, and is not carried out.
	assert.Equal(t, 1, divisions)
}

func TestReconcileDividesACardOnceForAnEngineWhoseLoadIsNeverRead(t *testing.T) {
	// The runtime reads the request metrics of one kind of engine. An engine
	// of another kind serves with its load unread at every reading.
	unread := engineHolding("busy", 4<<30, 20<<30)
	unread.RequestMetricsObserved = false
	r, runtime, snapshot, clock := aCardOfTwoEngines(t, unread, engineHolding("idle", 4<<30, 20<<30))

	divisions := divisionsIn(t, r, runtime, clock, 12, func(round int) {
		snapshot.Models[1].RequestsRunning = int64(4 * (round % 2))
	})

	assert.Equal(t, 1, divisions, "the engine is given a busy engine's share once, and the card is then left alone")
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

// When a grow is written, the instance already records the limit it grows
// into. A shrink is written before any record moves.
func TestReconcileWritesAGrowOnlyOnceItIsRecorded(t *testing.T) {
	serving := engineHolding("busy", 4<<30, 10<<30)
	serving.RequestsRunning = 4
	r, runtime, _, _ := aCardOfTwoEngines(t, serving, engineHolding("idle", 4<<30, 30<<30))
	recordedAtWrite := map[string]int64{}
	runtime.onKVLimit = func() {
		call := runtime.kvLimitCalls[len(runtime.kvLimitCalls)-1]
		recordedAtWrite[call.ModelName] = getModel(t, r, call.ModelName).Status.Instances[0].KVLimitBytes
	}

	reconcileOnce(t, r, "idle")

	require.Len(t, runtime.kvLimitCalls, 2)
	shrink, grow := runtime.kvLimitCalls[0], runtime.kvLimitCalls[1]
	require.Equal(t, "idle", shrink.ModelName)
	require.Equal(t, "busy", grow.ModelName)
	assert.Equal(t, int64(30)<<30, recordedAtWrite["idle"])
	assert.Equal(t, grow.LimitBytes, recordedAtWrite["busy"])
}

// A placement shrinks one neighbour and grows another. The reading that
// should confirm the grow is lost. The room is made and every limit is
// recorded by then, so the placement keeps the model.
func TestReconcileKeepsAModelPlacedWhenANeighbourCouldNotBeGrown(t *testing.T) {
	pm := claimWithCost(100, 50)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	idle := claimOnPod("idle", pod.Name, modelv1alpha1.ModelClaimActive, 100, 50)
	idle.Status.Instances[0].KVLimitBytes = 600
	busy := claimOnPod("busy", pod.Name, modelv1alpha1.ModelClaimActive, 100, 50)
	busy.Status.Instances[0].KVLimitBytes = 100
	serving := engineHolding("busy", 100, 100)
	serving.RequestsRunning = 4
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("idle", 50, 600), serving}
	r, runtime := newReconciler(t, pm, pod, idle, busy)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	writes := 0
	runtime.onKVLimit = func() {
		writes++
		if writes == 2 {
			runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}
		}
	}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1, "the placement keeps the model")
	assert.Len(t, runtime.activateCalls, 1)
	records := got.Status.Instances[0].KVLimitBytes +
		getModel(t, r, "idle").Status.Instances[0].KVLimitBytes +
		getModel(t, r, "busy").Status.Instances[0].KVLimitBytes
	assert.Equal(t, int64(1000-300), records, "the records spend the card, and no more")
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

func TestReconcileWarnsAgainWhileACardStaysStuck(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	recordedEvents(t, r)
	snapshot := runtime.snapshots[pod.Status.PodIP]
	snapshot.Models[0].RequestsRunning = 3
	snapshot.Models[0].RequestsWaiting = 1
	runtime.deafToKVLimits = true

	var warnedAt []int
	for try := 1; try <= 40; try++ {
		*clock = clock.Add(DefaultRequeueDuration)
		reconcileOnce(t, r, "stays")
		for _, event := range recordedEvents(t, r) {
			if strings.Contains(event, "KVLimitFailed") && strings.Contains(event, "model stays ") {
				warnedAt = append(warnedAt, try)
			}
		}
	}

	// An Event expires, so a card that stays stuck says so again, five
	// minutes of rounds after it said so first.
	assert.Equal(t, []int{3, 33}, warnedAt)
}

// twoCardsToDivide is two cards with one engine on each, "one" and "two". Each
// engine is held to less than its share, so both cards are divided when they
// are first seen.
func twoCardsToDivide(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, []corev1.Pod) {
	t.Helper()
	first, firstSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	second, secondSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 80<<30)
	claims := []client.Object{first, second}
	for _, on := range []struct {
		claim    string
		pod      *corev1.Pod
		snapshot *RuntimeSnapshot
	}{{"one", first, firstSnapshot}, {"two", second, secondSnapshot}} {
		claim := withFinalizer(claimOnPod(on.claim, on.pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30))
		claim.Status.Instances[0].Port = 9001
		claim.Status.Instances[0].KVLimitBytes = 10 << 30
		claims = append(claims, claim)
		on.snapshot.Models = []RuntimeSnapshotModel{engineHolding(on.claim, 4<<30, 10<<30)}
	}
	r, runtime := newReconciler(t, claims...)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		first.Status.PodIP: firstSnapshot, second.Status.PodIP: secondSnapshot,
	}
	return r, runtime, []corev1.Pod{*first, *second}
}

func TestReconcileReadsACardJustBeforeItIsDivided(t *testing.T) {
	r, runtime, pods := twoCardsToDivide(t)
	// What the controller had read of the second card when the first was written.
	readOfSecond := -1
	runtime.onKVLimit = func() {
		if readOfSecond < 0 {
			readOfSecond = runtime.snapshotCallsTo[pods[1].Status.PodIP]
		}
	}

	reconcileOnce(t, r, "one")

	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Zero(t, readOfSecond, "a card is read once the card before it is divided, not before")
}

// A pass that places a model reads every card first, to rank them. A reading
// from then is as old as every change the pass has made since.
func TestDivideCardsReadsACardAgainOnceTheCardBeforeItWasDivided(t *testing.T) {
	r, runtime, pods := twoCardsToDivide(t)
	readings := newRuntimeReadings(r.Runtime)
	readings.ofPods(context.Background(), pods)
	var readOfSecond []int
	runtime.onKVLimit = func() {
		readOfSecond = append(readOfSecond, runtime.snapshotCallsTo[pods[1].Status.PodIP])
	}

	r.divideCards(context.Background(), pods, readings)

	// The first card is divided from the reading the pass has, since nothing
	// was changed after it. The second is read again before its own write.
	assert.Equal(t, []int{1, 2}, readOfSecond)
	assert.Equal(t, 2, runtime.snapshotCallsTo[pods[0].Status.PodIP],
		"the first card is read once for the pass, and once to confirm its division")
}

// unreadablePods fails the snapshot reads of the pods listed, by IP.
type unreadablePods struct {
	*fakeRuntime
	pods map[string]bool
}

func (u *unreadablePods) Snapshot(ctx context.Context, podIP string, port int) (*RuntimeSnapshot, error) {
	if u.pods[podIP] {
		u.snapshotCalls++
		if u.snapshotCallsTo == nil {
			u.snapshotCallsTo = map[string]int{}
		}
		u.snapshotCallsTo[podIP]++
		return nil, errors.New("runtime did not answer")
	}
	return u.fakeRuntime.Snapshot(ctx, podIP, port)
}

// A runtime that did not answer costs the worker a whole timeout, so a pass
// does not ask it twice.
func TestDivideCardsDoesNotAskARuntimeAgainThatDidNotAnswer(t *testing.T) {
	r, runtime, pods := twoCardsToDivide(t)
	r.Runtime = &unreadablePods{fakeRuntime: runtime, pods: map[string]bool{pods[1].Status.PodIP: true}}
	readings := newRuntimeReadings(r.Runtime)
	readings.ofPods(context.Background(), pods)

	r.divideCards(context.Background(), pods, readings)

	require.Len(t, runtime.kvLimitCalls, 1, "the first card is divided")
	assert.Equal(t, 1, runtime.snapshotCallsTo[pods[1].Status.PodIP])
}

func TestReconcileDoesNotTakeACardThatHasBarelyDriftedForOneThatWasDivided(t *testing.T) {
	r, runtime, snapshot, clock := aCardOfTwoEngines(t,
		engineHolding("busy", 4<<30, 20<<30), engineHolding("idle", 4<<30, 20<<30))
	// No write reaches a segment, so no division is ever confirmed. Requests
	// wait on "busy" in the first two rounds and in the fourth. In the third,
	// the card is at rest, and it is divided as its plan wants it.
	runtime.deafToKVLimits = true
	warnings := 0
	for round := 1; round <= 4; round++ {
		snapshot.Models[0].RequestsRunning = 3
		snapshot.Models[0].RequestsWaiting = 1
		if round == 3 {
			snapshot.Models[0].RequestsRunning = 0
			snapshot.Models[0].RequestsWaiting = 0
		}
		before := len(runtime.kvLimitCalls)
		nextRound(t, r, clock, "idle")
		assert.Equal(t, round != 3, len(runtime.kvLimitCalls) > before, "round %d", round)
		for _, event := range recordedEvents(t, r) {
			if strings.Contains(event, "KVLimitFailed") && strings.Contains(event, "model busy ") {
				warnings++
			}
		}
	}

	assert.Equal(t, 1, warnings, "three divisions failed in a row, and the round between them had nothing to write")
}

func TestCardDivisionStateNotesACardThatWasLeftAlone(t *testing.T) {
	divisions := newCardDivisionState(nil)
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	require.False(t, divisions.noted(card))
	require.Equal(t, 2, divisions.failedAgain(card)+divisions.failedAgain(card)-1)

	divisions.leftAlone(card, "a")

	assert.True(t, divisions.noted(card), "a change of its engines can be seen from now on")
	_, changed := divisions.due(card, "b")
	assert.True(t, changed)
	assert.Equal(t, 3, divisions.failedAgain(card), "nothing was tried, so the run of failures is not over")
}

// Failures that lie far apart are no run. Most rounds leave a card alone, so
// a division that works is rare, and only that would end a run.
func TestCardDivisionStateStartsARunOfFailuresAgainAfterFiveQuietMinutes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}

	require.Equal(t, 1, divisions.failedAgain(card))
	now = now.Add(5 * time.Minute)
	require.Equal(t, 2, divisions.failedAgain(card), "five minutes apart is still one run")
	now = now.Add(5*time.Minute + time.Nanosecond)
	assert.Equal(t, 1, divisions.failedAgain(card))
	now = now.Add(time.Hour)
	assert.Equal(t, 1, divisions.failedAgain(card))
}

// A card is forgotten when nothing has asked about it for five minutes. The
// card that is asked about is there, however long ago its last round was.
func TestCardDivisionStateKeepsTheCardThatIsAskedAbout(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	divisions := newCardDivisionState(func() time.Time { return now })
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	gone := types.NamespacedName{Namespace: testNamespace, Name: "deleted"}
	for _, each := range []types.NamespacedName{card, gone} {
		divisions.due(each, "a")
		divisions.divided(each, "a")
		divisions.failedAgain(each)
	}

	now = now.Add(time.Hour)
	divide, changed := divisions.due(card, "a")

	assert.True(t, divide)
	assert.False(t, changed)
	assert.True(t, divisions.noted(card))
	assert.False(t, divisions.noted(gone))
}

func TestCardDivisionStateSaysOnceWhyACardIsLeftUndivided(t *testing.T) {
	divisions := newCardDivisionState(nil)
	card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
	other := types.NamespacedName{Namespace: testNamespace, Name: "warm-2"}

	assert.True(t, divisions.leftUndivided(card, "its runtime did not answer"))
	assert.False(t, divisions.leftUndivided(card, "its runtime did not answer"), "it is said once")
	assert.True(t, divisions.leftUndivided(other, "its runtime did not answer"), "each card says its own")
	assert.True(t, divisions.leftUndivided(card, "an engine there answers to no claim"), "another reason is news")

	divisions.accountedFor(card)
	assert.True(t, divisions.leftUndivided(card, "an engine there answers to no claim"),
		"a card that was accounted for in between says it again")
}

func TestReconcileNotesWhyACardIsLeftUndivided(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	snapshot := runtime.snapshots[pod.Status.PodIP]
	// An engine that answers to no claim makes the card one nobody can
	// account for.
	snapshot.Models = append(snapshot.Models, engineHolding("stranger", 1<<30, 10<<30))

	reconcileOnce(t, r, "stays")

	require.Empty(t, runtime.kvLimitCalls)
	assert.Contains(t, r.Divisions.undividedFor[cardOf(pod)], "answers to no claim")

	snapshot.Models = snapshot.Models[:2]
	nextRound(t, r, clock, "stays")
	assert.NotContains(t, r.Divisions.undividedFor, cardOf(pod))
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

	// Requests wait on "stays", so each round has more to give it, and no
	// write reaches a segment, so no division is ever confirmed.
	snapshot := runtime.snapshots[pod.Status.PodIP]
	snapshot.Models[0].RequestsRunning = 3
	snapshot.Models[0].RequestsWaiting = 1
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

func TestReconcileDoesNotTakeACardLeftAloneForOneThatWasDivided(t *testing.T) {
	r, runtime, pod, clock := twoEnginesSharingACard(t)
	reconcileOnce(t, r, "stays")
	recordedEvents(t, r)

	// No write reaches a segment, so no division is ever confirmed. Requests
	// wait on "stays" in the first two rounds and in the fourth. In the third
	// they are all being served, so nothing is tried.
	snapshot := runtime.snapshots[pod.Status.PodIP]
	runtime.deafToKVLimits = true
	warnings := 0
	for round := 1; round <= 4; round++ {
		snapshot.Models[0].RequestsRunning = 3
		snapshot.Models[0].RequestsWaiting = 1
		if round == 3 {
			snapshot.Models[0].RequestsRunning = 4
			snapshot.Models[0].RequestsWaiting = 0
		}
		before := len(runtime.kvLimitCalls)
		nextRound(t, r, clock, "stays")
		assert.Equal(t, round != 3, len(runtime.kvLimitCalls) > before, "round %d", round)
		for _, event := range recordedEvents(t, r) {
			if strings.Contains(event, "KVLimitFailed") && strings.Contains(event, "model stays ") {
				warnings++
			}
		}
	}

	assert.Equal(t, 1, warnings, "three divisions failed in a row, and the round between them tried none")
}
