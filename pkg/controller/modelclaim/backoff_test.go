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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestPlacementBackoffWaitsLongerAfterEachRefusal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim, 1, nil))
	assert.Equal(t, 2*DefaultRequeueDuration, backoff.refused(claim, 1, nil))
	assert.Equal(t, 4*DefaultRequeueDuration, backoff.refused(claim, 1, nil))
}

func TestPlacementBackoffStopsDoublingAtItsCeiling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	wait := time.Duration(0)
	for i := 0; i < 40; i++ {
		wait = backoff.refused(claim, 1, nil)
	}

	assert.Equal(t, maximumPlacementBackoff, wait)
}

func TestPlacementBackoffHoldsAClaimUntilItsTurn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	due, left := backoff.due(claim, 1, nil)
	assert.True(t, due)
	assert.Zero(t, left)

	backoff.refused(claim, 1, nil)
	backoff.statusWritten(claim)
	due, left = backoff.due(claim, 1, nil)
	assert.False(t, due)
	assert.Equal(t, DefaultRequeueDuration, left)

	now = now.Add(DefaultRequeueDuration)
	due, _ = backoff.due(claim, 1, nil)
	assert.True(t, due)
}

// A claim waits only once its status says why. Until then it is tried again,
// and the try that is repeated counts once.
func TestPlacementBackoffTriesAgainWhileARefusalIsNotWritten(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	require.Equal(t, DefaultRequeueDuration, backoff.refused(claim, 1, nil))
	due, _ := backoff.due(claim, 1, nil)
	assert.True(t, due, "the refusal was not written")
	due, _ = backoff.due(claim, 1, nil)
	assert.True(t, due, "and still is not")

	assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim, 1, nil), "the same try, made again")
	backoff.statusWritten(claim)
	due, _ = backoff.due(claim, 1, nil)
	assert.False(t, due)

	// A start that failed is written before its wait is recorded.
	other := types.NamespacedName{Namespace: testNamespace, Name: "other"}
	backoff.failedToStart(other, 1, nil)
	due, _ = backoff.due(other, 1, nil)
	assert.False(t, due)
}

func TestPlacementBackoffStartsOverOnceAClaimIsPlaced(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	backoff.refused(claim, 1, nil)
	backoff.refused(claim, 1, nil)
	backoff.placed(claim)

	due, _ := backoff.due(claim, 1, nil)
	assert.True(t, due)
	assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim, 1, nil))
}

// reconcileFor reconciles a claim once and returns how soon it asked to be
// reconciled again.
func reconcileFor(t *testing.T, r *ModelClaimReconciler, name string) time.Duration {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	require.NoError(t, err)
	return result.RequeueAfter
}

// aClaimWaitingForRoom is a claim that needs 800 of a 1000 card on which a
// neighbour is already promised 400, so it can never fit until the neighbour
// goes. The clock the backoff reads is the one returned.
func aClaimWaitingForRoom(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *time.Time) {
	t.Helper()
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, &now
}

func TestReconcileBacksOffAClaimNoCardCanHold(t *testing.T) {
	r, runtime, pm, clock := aClaimWaitingForRoom(t)

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	activations := len(runtime.activateCalls)
	asked := runtime.snapshotCalls
	require.NotZero(t, asked)

	// Inside the wait, the pool is not asked about the claim again, and the
	// claim comes back when its wait is up.
	*clock = clock.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration/2, reconcileFor(t, r, pm.Name))
	assert.Equal(t, asked, runtime.snapshotCalls)

	// Each try that is refused doubles the wait, up to a minute.
	*clock = clock.Add(DefaultRequeueDuration / 2)
	for _, want := range []time.Duration{20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
		assert.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}
	assert.Len(t, runtime.activateCalls, activations, "nothing could hold it, so nothing was started")
}

func TestReconcileForgetsTheWaitOnceTheModelIsPlaced(t *testing.T) {
	r, runtime, pm, pod := aClaimWaitingForHeldRoom(t)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}
	reconcileOnce(t, r, pm.Name)
	require.Contains(t, r.Backoff.attempts, claim)

	// The neighbour gives back KV it had mapped. Nothing says so, and the
	// claim finds the room when its wait is up. Nothing woke it, so only
	// being placed can have made it forget its wait.
	runtime.snapshots[pod.Status.PodIP].Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 700)}
	now = now.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	assert.NotContains(t, r.Backoff.attempts, claim, "a placed claim that waits again starts from the shortest wait")
}

func TestReconcileChecksAPartlyPlacedClaimEveryRound(t *testing.T) {
	pm := claimWithCost(300, 100)
	two := int32(2)
	pm.Spec.Replicas = &two
	// One card, which takes one instance of the model and cannot take a
	// second, since an engine runs at most once on a pod.
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	// While the first engine comes up, the claim is looked at every couple of
	// seconds.
	assert.Equal(t, ActivatingRequeueDuration, reconcileFor(t, r, pm.Name))
	placed := getModel(t, r, pm.Name).Status.Instances
	require.Len(t, placed, 1)

	// The second replica waits. The first engine is up and routed, and it is
	// still checked every round, so the claim does not sleep through the wait.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(placed[0].KVLimitBytes)}
	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
}

func TestPlacementBackoffStartsOverWhenRoomMayHaveAppeared(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}
	card := func(room podRoom) roomSignature { return roomSignature{"warm-1/u1": room} }
	// Three instances, which are promised 1200 together.
	whole := podRoom{instances: 3, awake: 3, promisedBytes: 1200}
	// Three instances, one of whose claim declares nothing.
	withAHole := podRoom{instances: 3, awake: 3, undeclared: 1, promisedBytes: 800}
	cases := []struct {
		name       string
		generation int64
		before     roomSignature
		room       roomSignature
		due        bool
	}{
		{"the pool as it was", 1, card(whole), card(whole), false},
		{"more promised on a card", 1, card(whole), card(podRoom{instances: 3, awake: 3, promisedBytes: 1300}), false},
		{"a pod gone", 1, card(whole), roomSignature{}, false},
		{"an instance gone", 1, card(whole), card(podRoom{instances: 2, awake: 2, promisedBytes: 800}), true},
		{"less promised on a card", 1, card(whole), card(podRoom{instances: 3, awake: 3, promisedBytes: 1100}), true},
		{"a pod joined", 1, card(whole), roomSignature{"warm-1/u1": whole, "warm-2/u2": {}}, true},
		{"an engine woken", 1, card(podRoom{instances: 3, awake: 2, promisedBytes: 1200}), card(whole), false},
		{"an engine gone to sleep", 1, card(whole), card(podRoom{instances: 3, awake: 2, promisedBytes: 1200}), true},
		{"a pod turned ready", 1, card(whole), card(podRoom{instances: 3, awake: 3, promisedBytes: 1200, ready: true}), true},
		{"a pod turned not ready", 1, card(podRoom{instances: 3, awake: 3, promisedBytes: 1200, ready: true}), card(whole), false},
		{"the claim's own spec changed", 2, card(whole), card(whole), true},
		// A card with a hole is turned away, whatever else happens on it. So
		// nothing on it counts as room until its last hole has closed.
		{"a hole closed", 1, card(withAHole), card(whole), true},
		{"a hole opened", 1, card(whole), card(withAHole), false},
		{"an instance gone beside a hole", 1, card(withAHole),
			card(podRoom{instances: 2, awake: 2, undeclared: 1, promisedBytes: 400}), false},
		{"less promised beside a hole", 1, card(withAHole),
			card(podRoom{instances: 3, awake: 3, undeclared: 1, promisedBytes: 700}), false},
		{"an engine gone to sleep beside a hole", 1, card(withAHole),
			card(podRoom{instances: 3, awake: 2, undeclared: 1, promisedBytes: 800}), false},
		{"one of two holes closed", 1, card(podRoom{instances: 3, awake: 3, undeclared: 2, promisedBytes: 400}),
			card(withAHole), false},
		// A pod without a card is not judged by its account, so it can take
		// a model once its runtime answers.
		{"a pod turned ready beside a hole", 1, card(withAHole),
			card(podRoom{instances: 3, awake: 3, undeclared: 1, promisedBytes: 800, ready: true}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backoff := newPlacementBackoff(func() time.Time { return now })
			backoff.refused(claim, 1, c.before)
			backoff.refused(claim, 1, c.before)
			backoff.statusWritten(claim)

			due, _ := backoff.due(claim, c.generation, c.room)

			assert.Equal(t, c.due, due)
			if c.due {
				assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim, c.generation, c.room),
					"a claim woken by possible room starts over from the shortest wait")
			}
		})
	}
}

func TestRoomSignatureCountsWhatEachCandidateCarries(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.UID = "uid-1"
	empty := warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning)
	empty.UID = "uid-2"
	empty.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	declared := claimOnPod("declared", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	asleep := claimOnPod("asleep", "warm-1", modelv1alpha1.ModelClaimSleeping, 200, 100)
	failed := claimOnPod("failed", "warm-1", modelv1alpha1.ModelClaimFailed, 300, 100)
	legacy := claimOnPod("legacy", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	legacy.Spec.PerGPU = nil
	elsewhere := claimOnPod("elsewhere", "warm-9", modelv1alpha1.ModelClaimActive, 300, 100)
	for port, claim := range []*modelv1alpha1.ModelClaim{declared, asleep, failed, legacy, elsewhere} {
		claim.Status.Instances[0].Port = int32(9001 + port)
	}
	// An instance is recorded before its engine is started, and has no port
	// until the engine is. Such a record is not counted.
	recorded := claimOnPod("recorded", "warm-1", modelv1alpha1.ModelClaimActivating, 300, 100)
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{
		*declared, *asleep, *failed, *legacy, *recorded, *elsewhere,
	}}

	room := roomSignatureOf([]corev1.Pod{*pod, *empty}, claims)

	assert.Equal(t, roomSignature{
		"warm-1/uid-1": {instances: 3, awake: 2, undeclared: 1, promisedBytes: 700},
		"warm-2/uid-2": {ready: true},
	}, room)
	assert.Nil(t, roomSignatureOf([]corev1.Pod{*pod}, nil), "with no listing there is nothing to compare")
}

func TestFreesRoomPassesTheChangesThatCanFreeACard(t *testing.T) {
	base := claimOnPod("neighbour", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	base.Status.Instances[0].Port = 9001
	cases := []struct {
		name   string
		change func(*modelv1alpha1.ModelClaim)
		frees  bool
	}{
		{"an instance removed", func(c *modelv1alpha1.ModelClaim) { c.Status.Instances = nil }, true},
		{"an instance failed", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances[0].Phase = modelv1alpha1.ModelClaimFailed
		}, true},
		{"a smaller declaration", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.PerGPU.KVFloor = *resource.NewQuantity(50, resource.BinarySI)
		}, true},
		{"a larger declaration", func(c *modelv1alpha1.ModelClaim) {
			c.Spec.PerGPU.KVFloor = *resource.NewQuantity(500, resource.BinarySI)
		}, false},
		{"an instance gone to sleep", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
		}, true},
		{"an instance added", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = append(c.Status.Instances, modelv1alpha1.ModelClaimInstance{Pod: "warm-2"})
		}, false},
		{"an engine started", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances[0].Port = 9002
		}, false},
		{"a condition written", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: "Ready"})
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := base.DeepCopy()
			c.change(after)
			assert.Equal(t, c.frees, freesRoom(base, after))
		})
	}

	// A record taken back before its engine was started: no engine left a card.
	recorded := base.DeepCopy()
	recorded.Status.Instances[0].Port = 0
	takenBack := recorded.DeepCopy()
	takenBack.Status.Instances = nil
	assert.False(t, freesRoom(recorded, takenBack))

	asleep := base.DeepCopy()
	asleep.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
	assert.False(t, freesRoom(asleep, base), "an engine that wakes takes room, and frees none")

	undeclared := base.DeepCopy()
	undeclared.Spec.PerGPU = nil
	assert.True(t, freesRoom(undeclared, base), "a claim that comes to declare its cost closes a hole")
	assert.False(t, freesRoom(base, undeclared), "a claim that stops declaring opens one")

	watched := roomMayHaveFreed()
	assert.True(t, watched.Delete(event.DeleteEvent{Object: base}), "a deleted claim frees its cards")
	assert.False(t, watched.Create(event.CreateEvent{Object: base}))
}

func TestEnqueueWaitingClaimsWakesOnlyTheClaimsThatWait(t *testing.T) {
	leaving := claimOnPod("leaving", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	waiting := claimWithCost(300, 100)
	placed := claimOnPod("placed", "warm-2", modelv1alpha1.ModelClaimActive, 300, 100)
	r, _ := newReconciler(t, leaving, waiting, placed)

	requests := enqueueWaitingClaims(r.Client)(context.Background(), leaving)

	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: testNamespace, Name: waiting.Name,
	}}}, requests)
}

func TestReconcileTriesAWaitingClaimAgainWhenANeighbourLeaves(t *testing.T) {
	r, runtime, pm, _ := aClaimWaitingForRoom(t)
	reconcileOnce(t, r, pm.Name)
	require.Empty(t, runtime.activateCalls)

	// The neighbour goes, and so does its engine. The clock has not moved, so
	// only the room it freed can explain another try.
	require.NoError(t, r.Delete(context.Background(), getModel(t, r, "neighbour")))
	runtime.snapshots["10.0.0.1"].Models = nil
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1, "the room the neighbour freed is tried at once")
}

// aClaimWaitingForHeldRoom is a claim that needs 400 of a 1000 card. The card
// could hold it beside its neighbour, which is promised 400. It does not
// today: the neighbour has mapped 500 of KV, so 200 is free.
func aClaimWaitingForHeldRoom(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *corev1.Pod) {
	t.Helper()
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 700
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 500, 700)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, pod
}

// An engine that goes to sleep gives back the KV it had mapped. So a claim that
// was refused on what the engines hold is tried again at once.
func TestReconcileTriesAWaitingClaimAgainWhenANeighbourGoesToSleep(t *testing.T) {
	r, runtime, pm, pod := aClaimWaitingForHeldRoom(t)
	reconcileOnce(t, r, pm.Name)
	require.Empty(t, runtime.activateCalls)
	require.Contains(t, scheduled(t, r, pm.Name).Message, "held by the engines already on it")

	asleep := engineHolding("neighbour", 0, 700)
	asleep.Phase = runtimePhaseSleeping
	asleep.Ready = false
	runtime.snapshots[pod.Status.PodIP].Models = []RuntimeSnapshotModel{asleep}
	neighbour := getModel(t, r, "neighbour")
	before := neighbour.DeepCopy()
	neighbour.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
	require.NoError(t, r.Status().Update(context.Background(), neighbour))

	// The change is one the watch on claims passes on, and the claim it wakes
	// is the one that waits.
	require.True(t, roomMayHaveFreed().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: neighbour}))
	woken := enqueueWaitingClaims(r.Client)(context.Background(), neighbour)
	require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: testNamespace, Name: pm.Name,
	}}}, woken)

	// The clock has not moved, so only the sleep can explain another try.
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1, "the room the sleep freed is tried at once")
}

// A pod is a candidate as soon as it runs, and its runtime may need longer to
// answer. The pod turning ready is the sign that it does.
func TestReconcileTriesAWaitingClaimAgainWhenAPodTurnsReady(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}

	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Empty(t, runtime.activateCalls)

	// Its runtime answers now, and the kubelet has seen it.
	runtime.nilSnapshots = nil
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	ready := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pod), ready))
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, r.Status().Update(context.Background(), ready))

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1, "the pod is tried as soon as it is ready")
}

func TestReconcileTriesAWaitingClaimAgainWhenItsOwnSpecChanges(t *testing.T) {
	r, runtime, pm, _ := aClaimWaitingForRoom(t)
	reconcileOnce(t, r, pm.Name)
	require.Empty(t, runtime.activateCalls)

	// The claim now declares a smaller footprint, and fits beside its
	// neighbour. The fake client does not count generations, so the test does.
	claim := getModel(t, r, pm.Name)
	claim.Spec.PerGPU.MaximumFootprint = *resource.NewQuantity(300, resource.BinarySI)
	claim.Generation++
	require.NoError(t, r.Update(context.Background(), claim))
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
}

func TestReconcileKeepsAClaimWaitingWhenOnlyMoreIsPromised(t *testing.T) {
	r, _, pm, clock := aClaimWaitingForRoom(t)
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}
	reconcileOnce(t, r, pm.Name)
	require.Equal(t, 1, r.Backoff.attempts[claim].refusals)
	refusedAt := r.Backoff.attempts[claim].readyAt

	// Another model is recorded on the card, which only takes room away.
	other := claimOnPod("other", "warm-1", modelv1alpha1.ModelClaimActivating, 100, 100)
	recorded := other.Status
	require.NoError(t, r.Create(context.Background(), other))
	other.Status = recorded
	require.NoError(t, r.Status().Update(context.Background(), other))
	*clock = clock.Add(time.Second)
	reconcileOnce(t, r, pm.Name)

	// A try would have been refused, and its wait would run from a second
	// later.
	assert.Equal(t, refusedAt, r.Backoff.attempts[claim].readyAt, "the claim was not tried again early")
	assert.Equal(t, 1, r.Backoff.attempts[claim].refusals)
}

// scheduled is the claim's Scheduled condition, which must be there.
func scheduled(t *testing.T, r *ModelClaimReconciler, name string) metav1.Condition {
	t.Helper()
	for _, condition := range getModel(t, r, name).Status.Conditions {
		if condition.Type == string(modelv1alpha1.ModelClaimConditionTypeScheduled) {
			return condition
		}
	}
	require.FailNow(t, "no Scheduled condition")
	return metav1.Condition{}
}

func TestReconcileSaysWhenNoCardCouldEverHoldAClaim(t *testing.T) {
	pm := claimWithCost(50<<30, 10<<30)
	small, smallSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 40<<30)
	larger, largerSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 48<<30)
	r, runtime := newReconciler(t, pm, small, larger)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		small.Status.PodIP:  smallSnapshot,
		larger.Status.PodIP: largerSnapshot,
	}
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}

	reconcileOnce(t, r, pm.Name)

	condition := scheduled(t, r, pm.Name)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "TooLargeForAnyCard", condition.Reason)
	assert.Contains(t, condition.Message, "needs 60.0 GiB on a card")
	assert.Contains(t, condition.Message, "the best of them offers 48.0 GiB on a card")
	told := 0
	for _, event := range recordedEvents(t, r) {
		if strings.Contains(event, "TooLargeForAnyCard") {
			told++
		}
	}
	assert.Equal(t, 1, told)
	assert.Equal(t, 1, r.Backoff.attempts[claim].refusals, "a claim no card could ever hold still backs off")
	assert.Empty(t, runtime.activateCalls)
}

func TestReconcileDoesNotCallAClaimTooLargeWhileACardCannotBeMeasured(t *testing.T) {
	pm := claimWithCost(50<<30, 10<<30)
	small, smallSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 40<<30)
	unanswered, _ := sizedWarmPod("warm-2", "10.0.0.2", 0)
	r, runtime := newReconciler(t, pm, small, unanswered)
	runtime.snapshots = map[string]*RuntimeSnapshot{small.Status.PodIP: smallSnapshot}
	runtime.nilSnapshots = map[string]bool{unanswered.Status.PodIP: true}

	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, "NoMatchingPods", scheduled(t, r, pm.Name).Reason,
		"a card nobody measured might hold the model")
}

func TestReconcileDoesNotCallAClaimTooLargeWhenAnEmptyCardCouldHoldIt(t *testing.T) {
	r, _, pm, _ := aClaimWaitingForRoom(t)

	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, "NoMatchingPods", scheduled(t, r, pm.Name).Reason,
		"the card holds the model once its neighbour goes")
}

// The account is built from the claims as the API server has them, and the
// room a claim was refused on is remembered the same way. A cache a moment
// behind would otherwise miss an instance recorded just before, and its
// leaving would not wake the claim.
func TestReconcileRemembersTheRoomAClaimWasRefusedOnAsTheAPIServerHasIt(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	// The cache has not seen the neighbour yet; the API server has.
	r, runtime := newReconciler(t, pm, pod)
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(pm.DeepCopy(), pod.DeepCopy(), neighbour).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))

	// The neighbour leaves. Seen through the cache, the card carries nothing,
	// which is less than it carried when the claim was refused.
	key := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	due, _ := r.Backoff.due(key, pm.Generation, roomSignatureOf([]corev1.Pod{*pod}, &modelv1alpha1.ModelClaimList{}))
	assert.True(t, due, "the claim is woken by the neighbour leaving")
}

// A try can straddle a neighbour's leaving. The API server has the neighbour
// gone already, and its engine is still exiting, so the try is refused. The
// event of the leaving comes afterwards. It finds the pool as the refusal
// remembered it, and wakes nobody. So such a try starts the claim over from
// the shortest wait.
func TestReconcileStartsAClaimOverWhenThePoolChangedUnderItsTry(t *testing.T) {
	r, runtime, pm, clock := aClaimWaitingForRoom(t)
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}

	// The cache still lists the neighbour, and the API server does not.
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(getModel(t, r, pm.Name), pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()
	exiting := engineHolding("neighbour", 100, 600)
	exiting.Phase = runtimePhaseStopping
	exiting.Ready = false
	runtime.snapshots[pod.Status.PodIP].Models = []RuntimeSnapshotModel{exiting}

	wait := reconcileFor(t, r, pm.Name)

	require.Empty(t, runtime.activateCalls, "a card whose engine is still exiting is not handed out")
	assert.Equal(t, DefaultRequeueDuration, wait)
}

// A try that is refused on the pool as it was when the try began goes on
// doubling its wait.
func TestReconcileGoesOnWaitingLongerWhenThePoolStoodStillUnderItsTry(t *testing.T) {
	r, _, pm, clock := aClaimWaitingForRoom(t)
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	// The API server and the cache list the same claims.
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).
		WithObjects(getModel(t, r, pm.Name), getModel(t, r, "neighbour"), pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()

	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}
}

// A claim no card could ever hold is helped only by a pod joining the pool, or
// by its own spec changing. Room freed on a card too small for it changes
// nothing, so it does not wake the claim.
func TestPlacementBackoffWakesATooLargeClaimOnlyForANewPod(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "huge"}
	before := roomSignature{"warm-1/u1": {instances: 2, awake: 2, promisedBytes: 800}}
	backoff.refusedAsTooLarge(claim, 1, before)
	backoff.statusWritten(claim)

	due, _ := backoff.due(claim, 1, roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400}})
	assert.False(t, due, "a neighbour leaving does not make a card large enough")
	due, _ = backoff.due(claim, 1, roomSignature{"warm-1/u1": {instances: 2, awake: 1, promisedBytes: 800, ready: true}})
	assert.False(t, due, "nor does a neighbour asleep, or a pod that was measured turning ready")
	due, _ = backoff.due(claim, 1, roomSignature{"warm-1/u1": before["warm-1/u1"], "warm-2/u2": {}})
	assert.True(t, due, "a pod joining may bring a larger card")
}

// Through Reconcile as well: a claim no card could ever hold is not tried
// again for room that was freed.
func TestReconcileDoesNotTryATooLargeClaimAgainForFreedRoom(t *testing.T) {
	pm := claimWithCost(1500, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 700
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 700)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Equal(t, "TooLargeForAnyCard", scheduled(t, r, pm.Name).Reason)
	asked := runtime.snapshotCalls

	require.NoError(t, r.Delete(context.Background(), getModel(t, r, "neighbour")))
	runtime.snapshots[pod.Status.PodIP].Models = nil

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	assert.Equal(t, asked, runtime.snapshotCalls, "the card is no larger with its neighbour gone")
}

// The room a claim remembers is the room of its last refusal. Compared with
// an older one, a pool that has not changed since would look changed.
func TestPlacementBackoffRemembersTheRoomOfTheLastRefusal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}
	crowded := roomSignature{"warm-1/u1": {instances: 2, awake: 2, promisedBytes: 800}}
	emptier := roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400}}

	backoff.refused(claim, 1, crowded)
	backoff.refused(claim, 1, emptier)
	backoff.statusWritten(claim)

	due, _ := backoff.due(claim, 1, emptier)
	assert.False(t, due)
}

func TestTooLargeForEveryCardTakesACardOfExactlyTheSizeForLargeEnough(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	ledgers := podLedgersFrom(&modelv1alpha1.ModelClaimList{}, nil, []corev1.Pod{*pod},
		map[string]*RuntimeSnapshot{pod.Name: snapshot})

	_, never := tooLargeForEveryCard([]corev1.Pod{*pod}, ledgers, 1000)
	assert.False(t, never)
	largest, never := tooLargeForEveryCard([]corev1.Pod{*pod}, ledgers, 1001)
	assert.True(t, never)
	assert.Equal(t, int64(1000), largest)
}

// A pod with several cards is judged by its smallest, since the device plugin
// decides which card an engine lands on. The refusal says what the pod
// offers, not what its largest card holds.
func TestReconcileSaysWhatAPodWithSeveralCardsOffers(t *testing.T) {
	pm := claimWithCost(50<<30, 10<<30)
	pm.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{"--tensor-parallel-size": "2"}}
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 2)
	pod.Status.PodIP = "10.0.0.1"
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: {
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 40 << 30, HBMUsableBytes: 40 << 30},
			{ID: "GPU-1", HBMFreeBytes: 80 << 30, HBMUsableBytes: 80 << 30},
		},
	}}

	reconcileOnce(t, r, pm.Name)

	condition := scheduled(t, r, pm.Name)
	assert.Equal(t, "TooLargeForAnyCard", condition.Reason)
	assert.Contains(t, condition.Message, "the best of them offers 40.0 GiB on a card")
}

// A claim's wait is forgotten once nothing is left for it to wait for: when it
// has all its instances, or when it is gone.
func TestReconcileForgetsTheWaitOfAClaimThatNoLongerWaits(t *testing.T) {
	r, _, pm, _ := aClaimWaitingForRoom(t)
	key := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Contains(t, r.Backoff.attempts, key)

	// Its instance was recorded some other way.
	placed := getModel(t, r, pm.Name)
	placed.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "warm-1", Phase: modelv1alpha1.ModelClaimActivating}}
	require.NoError(t, r.Status().Update(context.Background(), placed))
	reconcileFor(t, r, pm.Name)
	assert.NotContains(t, r.Backoff.attempts, key)

	// Refused again, and then deleted by someone else.
	r2, _, pm2, _ := aClaimWaitingForRoom(t)
	key2 := types.NamespacedName{Namespace: pm2.Namespace, Name: pm2.Name}
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r2, pm2.Name))
	gone := getModel(t, r2, pm2.Name)
	gone.Finalizers = nil
	require.NoError(t, r2.Update(context.Background(), gone))
	require.NoError(t, r2.Delete(context.Background(), gone))
	reconcileFor(t, r2, pm2.Name)
	assert.NotContains(t, r2.Backoff.attempts, key2)

	// Refused, and then deleted while this controller watches.
	r3, _, pm3, _ := aClaimWaitingForRoom(t)
	key3 := types.NamespacedName{Namespace: pm3.Namespace, Name: pm3.Name}
	reconcileFor(t, r3, pm3.Name)
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r3, pm3.Name))
	require.Contains(t, r3.Backoff.attempts, key3)
	require.NoError(t, r3.Delete(context.Background(), getModel(t, r3, pm3.Name)))
	reconcileFor(t, r3, pm3.Name)
	assert.NotContains(t, r3.Backoff.attempts, key3)

	// Refused, and then found with more instances than it asks for.
	r4, _, pm4, _ := aClaimWaitingForRoom(t)
	key4 := types.NamespacedName{Namespace: pm4.Namespace, Name: pm4.Name}
	reconcileFor(t, r4, pm4.Name)
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r4, pm4.Name))
	surplus := getModel(t, r4, pm4.Name)
	surplus.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Phase: modelv1alpha1.ModelClaimActivating},
		{Pod: "warm-1", Phase: modelv1alpha1.ModelClaimActivating},
	}
	require.NoError(t, r4.Status().Update(context.Background(), surplus))
	r4.Backoff.refused(key4, surplus.Generation, nil)
	reconcileFor(t, r4, pm4.Name)
	assert.NotContains(t, r4.Backoff.attempts, key4)
}

// A refusal is what tells a claim why it waits. When it could not be written,
// the claim is tried again on its next pass, and the refusal is written then.
func TestReconcileTriesAgainWhenARefusalCouldNotBeWritten(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	scheme := testScheme(t)
	conflicts := 1
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pm, pod, neighbour).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if obj.GetName() == pm.Name && conflicts > 0 {
					conflicts--
					return apierrors.NewConflict(schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
						obj.GetName(), fmt.Errorf("the object has been modified"))
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	runtime := &fakeRuntime{snapshots: map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}}
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	r := &ModelClaimReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32), Runtime: runtime,
		PoolPolicy: newPoolPolicyManager(clock),
		Divisions:  newCardDivisionState(clock),
		Backoff:    newPlacementBackoff(clock),
	}
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claim})
	require.NoError(t, err)
	require.True(t, result.Requeue, "the write met a conflict")
	require.Nil(t, meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled)))
	asked := runtime.snapshotCalls

	// The clock has not moved, so the claim is still inside its wait.
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name), "the try counts once")

	assert.Greater(t, runtime.snapshotCalls, asked, "the claim was tried again")
	assert.Equal(t, "NoMatchingPods", scheduled(t, r, pm.Name).Reason)
	assert.Equal(t, 1, r.Backoff.attempts[claim].refusals)

	// Once the refusal is written, the claim waits.
	asked = runtime.snapshotCalls
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	assert.Equal(t, asked, runtime.snapshotCalls)
}

// With one worker and a queue that is first in, first out, the order the
// claims are added in is the order they are tried in.
func TestEnqueueWaitingClaimsWakesTheOldestFirst(t *testing.T) {
	leaving := claimOnPod("leaving", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	created := time.Unix(1_700_000_000, 0)
	leaving.CreationTimestamp = metav1.NewTime(created.Add(30 * time.Second))
	var objects []client.Object
	// The names run against the ages, so a listing by name gives the wrong
	// order.
	for i, name := range []string{"d-oldest", "c-older", "b-newer", "a-newest"} {
		waiting := claimWithCost(300, 100)
		waiting.Name = name
		waiting.CreationTimestamp = metav1.NewTime(created.Add(time.Duration(i) * time.Minute))
		objects = append(objects, waiting)
	}
	twin := claimWithCost(300, 100)
	twin.Name = "a-twin-of-the-oldest"
	twin.CreationTimestamp = metav1.NewTime(created)
	r, _ := newReconciler(t, append(objects, leaving, twin)...)

	var woken []string
	for _, request := range enqueueWaitingClaims(r.Client)(context.Background(), leaving) {
		woken = append(woken, request.Name)
	}
	assert.Equal(t, []string{"a-twin-of-the-oldest", "d-oldest", "c-older", "b-newer", "a-newest"}, woken)

	woken = nil
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	for _, request := range enqueueModelClaimsForPod(r.Client)(context.Background(), pod) {
		woken = append(woken, request.Name)
	}
	assert.Equal(t, []string{"a-twin-of-the-oldest", "d-oldest", "leaving", "c-older", "b-newer", "a-newest"}, woken,
		"a pod that changes reaches every claim, the oldest first")
}

// Room does not help a claim that failed: its engine config is not valid, or
// its engine could not be started. Such a claim is not waiting for a card.
func TestEnqueueWaitingClaimsLeavesOutAClaimThatFailed(t *testing.T) {
	leaving := claimOnPod("leaving", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	failed := claimWithCost(300, 100)
	failed.Name = "failed"
	failed.Status.Phase = modelv1alpha1.ModelClaimFailed
	going := claimWithCost(300, 100)
	going.Name = "going"
	going.Finalizers = []string{ModelClaimFinalizer}
	deleted := metav1.NewTime(time.Unix(1_700_000_000, 0))
	going.DeletionTimestamp = &deleted
	r, _ := newReconciler(t, leaving, failed, going)

	assert.Empty(t, enqueueWaitingClaims(r.Client)(context.Background(), leaving),
		"neither the claim that caused the event, nor one that failed, nor one that is going")

	// A claim that waits is not woken by its own change.
	waiting := claimWithCost(300, 100)
	waiting.Name = "waiting"
	require.NoError(t, r.Create(context.Background(), waiting))
	assert.Empty(t, enqueueWaitingClaims(r.Client)(context.Background(), waiting))
	assert.Len(t, enqueueWaitingClaims(r.Client)(context.Background(), leaving), 1)
}

func TestRoomMayHaveFreedPassesOnlyTheUpdatesThatFreeRoom(t *testing.T) {
	before := claimOnPod("neighbour", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	before.Status.Instances[0].Port = 9001
	written := before.DeepCopy()
	written.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	gone := before.DeepCopy()
	gone.Status.Instances = nil
	watched := roomMayHaveFreed()

	assert.False(t, watched.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: written}))
	assert.True(t, watched.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: gone}))
	assert.False(t, watched.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: &corev1.Pod{}}),
		"what is no claim frees no card")
	assert.False(t, watched.Generic(event.GenericEvent{Object: before}))
}

// wakeLoop stands in for the manager's queue: first in, first out, one entry
// for each claim. It is fed as the watch on claims feeds the queue. Every
// status write is shown to roomMayHaveFreed as the update the informer would
// deliver, and what passes is mapped by enqueueWaitingClaims. Timers are left
// out, so whatever runs here runs with no wait at all.
type wakeLoop struct {
	queue  []types.NamespacedName
	queued map[types.NamespacedName]bool
	wake   func(ctx context.Context, obj client.Object) []ctrl.Request
}

func (l *wakeLoop) add(name types.NamespacedName) {
	if l.queued == nil {
		l.queued = map[types.NamespacedName]bool{}
	}
	if !l.queued[name] {
		l.queued[name] = true
		l.queue = append(l.queue, name)
	}
}

func (l *wakeLoop) pop() (types.NamespacedName, bool) {
	if len(l.queue) == 0 {
		return types.NamespacedName{}, false
	}
	name := l.queue[0]
	l.queue = l.queue[1:]
	delete(l.queued, name)
	return name, true
}

func (l *wakeLoop) watch() interceptor.Funcs {
	watched := roomMayHaveFreed()
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			before := &modelv1alpha1.ModelClaim{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), before); err != nil {
				return err
			}
			if err := cl.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			if watched.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: obj}) {
				for _, request := range l.wake(ctx, obj) {
					l.add(request.NamespacedName)
				}
			}
			return nil
		},
	}
}

// twoClaimsThatCannotStart is a card with room for two claims whose engines
// the runtime refuses to start, beside a neighbour that serves.
func twoClaimsThatCannotStart(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *wakeLoop) {
	t.Helper()
	first := claimWithCost(100, 100)
	first.Name = "first"
	second := claimWithCost(100, 100)
	second.Name = "second"
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 700
	neighbour.Status.Instances[0].Port = 9001
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 700)}
	scheme := testScheme(t)
	loop := &wakeLoop{}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(first, second, pod, neighbour).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(loop.watch()).Build()
	loop.wake = enqueueWaitingClaims(c)
	runtime := &fakeRuntime{failActivate: true, snapshots: map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}}
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	return &ModelClaimReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(1024), Runtime: runtime,
		PoolPolicy: newPoolPolicyManager(clock),
		Divisions:  newCardDivisionState(clock),
		Backoff:    newPlacementBackoff(clock),
	}, runtime, loop
}

func TestReconcileDoesNotLetClaimsThatCannotStartWakeEachOther(t *testing.T) {
	r, runtime, loop := twoClaimsThatCannotStart(t)

	loop.add(types.NamespacedName{Namespace: testNamespace, Name: "first"})
	loop.add(types.NamespacedName{Namespace: testNamespace, Name: "second"})
	passes := 0
	for ; passes < 50; passes++ {
		name, more := loop.pop()
		if !more {
			break
		}
		reconcileFor(t, r, name.Name)
	}

	// A record taken back after a start that failed frees no card, so it wakes
	// nobody. Each claim is tried once, and comes back when its wait is up.
	assert.Equal(t, 2, passes)
	assert.Equal(t, 2, len(runtime.activateCalls))
}

func TestReconcileBacksOffAClaimWhoseEngineCannotBeStarted(t *testing.T) {
	r, runtime, _ := twoClaimsThatCannotStart(t)

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, "first"))
	require.Equal(t, 1, len(runtime.activateCalls))
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, "first").Status.Phase)

	// A pass inside the wait, as after a pod event, starts nothing. The claim
	// goes on saying that its start failed.
	wait := reconcileFor(t, r, "first")
	assert.Equal(t, DefaultRequeueDuration, wait)
	assert.Equal(t, 1, len(runtime.activateCalls))
	got := getModel(t, r, "first")
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, "ActivateFailed", ready.Reason)
}

// A pod that joins the pool may be able to start the engine that another pod
// could not, so it wakes the claim. Room freed on a card does not: the claim
// had found a card.
func TestReconcileTriesAClaimThatCouldNotStartAgainWhenAPodJoins(t *testing.T) {
	first := claimWithCost(100, 100)
	first.Name = "first"
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	stays := claimOnPod("stays", pod.Name, modelv1alpha1.ModelClaimActive, 100, 100)
	stays.Status.Instances[0].Port = 9001
	stays.Status.Instances[0].KVLimitBytes = 300
	leaves := claimOnPod("leaves", pod.Name, modelv1alpha1.ModelClaimActive, 100, 100)
	leaves.Status.Instances[0].Port = 9002
	leaves.Status.Instances[0].KVLimitBytes = 300
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("stays", 100, 300), engineHolding("leaves", 100, 300)}
	r, runtime := newReconciler(t, first, pod, stays, leaves)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The runtime of this pod refuses every start.
	runtime.failActivateOn = map[string]bool{pod.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, "first"))
	require.Equal(t, []string{pod.Status.PodIP}, runtime.activatedOn)

	// A neighbour leaves. The clock has not moved.
	require.NoError(t, r.Delete(context.Background(), getModel(t, r, "leaves")))
	runtime.snapshots[pod.Status.PodIP].Models = snapshot.Models[:1]
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, "first"))
	assert.Equal(t, []string{pod.Status.PodIP}, runtime.activatedOn, "room was not what the claim lacked")

	// A pod joins. It is empty, so it ranks before the pod that refused.
	joined, joinedSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
	require.NoError(t, r.Create(context.Background(), joined))
	runtime.snapshots[joined.Status.PodIP] = joinedSnapshot
	reconcileOnce(t, r, "first")

	assert.Equal(t, []string{pod.Status.PodIP, joined.Status.PodIP}, runtime.activatedOn,
		"the claim is tried again at once, on the pod that joined")
	instances := getModel(t, r, "first").Status.Instances
	require.Len(t, instances, 1)
	assert.Equal(t, joined.Name, instances[0].Pod)
}

func TestReconcileWaitsLongerAfterEachStartThatFails(t *testing.T) {
	first := claimWithCost(100, 100)
	first.Name = "first"
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, first, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	runtime.failActivate = true
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })

	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute} {
		wait := reconcileFor(t, r, "first")
		assert.Equal(t, want, wait)
		now = now.Add(wait)
	}
	assert.Equal(t, 4, len(runtime.activateCalls))

	// Once the engine starts, the claim has nothing left to wait for.
	runtime.failActivate = false
	reconcileFor(t, r, "first")
	require.Len(t, getModel(t, r, "first").Status.Instances, 1)
	reconcileFor(t, r, "first")
	r.Backoff.mu.Lock()
	defer r.Backoff.mu.Unlock()
	assert.Empty(t, r.Backoff.attempts)
}

// failedOnFirstPod is a claim whose engine on warm-1 has failed for good, as
// its runtime reports it, with the snapshot that says so.
func failedOnFirstPod(t *testing.T) (*modelv1alpha1.ModelClaim, *corev1.Pod, *RuntimeSnapshot) {
	t.Helper()
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseFailed,
		LastError: "restart budget exhausted",
		ClaimRef:  &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	return pm, pod, snapshot
}

// A claim whose engine failed for good, with no other pod to take it, waits as
// a claim that cannot be placed does. It keeps its failed instance, so it still
// comes back every round, and a pass inside its wait tries nothing.
func TestReconcileBacksOffTheReplacementOfAFailedEngine(t *testing.T) {
	pm, failedPod, failedSnapshot := failedOnFirstPod(t)
	// The only other pod is promised to a neighbour.
	full, fullSnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	neighbour := claimOnPod("neighbour", full.Name, modelv1alpha1.ModelClaimActive, 700, 100)
	neighbour.Status.Instances[0].Port = 9001
	fullSnapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 200)}
	r, runtime := newReconciler(t, pm, neighbour, failedPod, full)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedPod.Status.PodIP: failedSnapshot,
		full.Status.PodIP:      fullSnapshot,
	}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}

	reconcileOnce(t, r, pm.Name)
	now = now.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, pm.Name)
	waiting, found := r.Backoff.attempts[claim]
	require.True(t, found, "the claim waits")

	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name),
		"it keeps its instance, so it comes back every round")
	assert.Equal(t, waiting, r.Backoff.attempts[claim], "a pass inside the wait tries nothing")
	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
}

// A replacement whose start is refused waits as a first start that failed
// does. A pass inside the wait starts nothing, and the failed instance stays.
func TestReconcileBacksOffAReplacementWhoseStartIsRefused(t *testing.T) {
	pm, failedPod, failedSnapshot := failedOnFirstPod(t)
	other, otherSnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	r, runtime := newReconciler(t, pm, failedPod, other)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedPod.Status.PodIP: failedSnapshot,
		other.Status.PodIP:     otherSnapshot,
	}
	runtime.failActivateOn = map[string]bool{other.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Equal(t, []string{other.Status.PodIP}, runtime.activatedOn)

	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	assert.Equal(t, []string{other.Status.PodIP}, runtime.activatedOn, "a pass inside the wait starts nothing")
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
}

// A claim whose engine could not be started had found a card, so room freed
// on a card does not help it. A pod that joined may start the engine, and so
// may one that turned ready: its runtime answers now.
func TestPlacementBackoffWakesAClaimThatCouldNotStartForAPodThatAnswers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	before := roomSignature{"warm-1/u1": {instances: 2, awake: 2, promisedBytes: 800, ready: true}, "warm-2/u2": {}}
	for name, c := range map[string]struct {
		room roomSignature
		due  bool
	}{
		"a neighbour gone": {roomSignature{
			"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400, ready: true}, "warm-2/u2": {}}, false},
		"a neighbour asleep": {roomSignature{
			"warm-1/u1": {instances: 2, awake: 1, promisedBytes: 800, ready: true}, "warm-2/u2": {}}, false},
		"a pod turned ready": {roomSignature{
			"warm-1/u1": before["warm-1/u1"], "warm-2/u2": {ready: true}}, true},
		"a pod turned not ready": {roomSignature{
			"warm-1/u1": {instances: 2, awake: 2, promisedBytes: 800}, "warm-2/u2": {}}, false},
		"a pod joined": {roomSignature{
			"warm-1/u1": before["warm-1/u1"], "warm-2/u2": {}, "warm-3/u3": {}}, true},
	} {
		backoff := newPlacementBackoff(func() time.Time { return now })
		backoff.failedToStart(claim, 1, before)

		due, _ := backoff.due(claim, 1, c.room)

		assert.Equal(t, c.due, due, name)
	}
}

// A refusal is what the claim waits on from then on. A start that failed
// before it is forgotten, so the claim no longer reads as one that could not
// start.
func TestPlacementBackoffForgetsAFailedStartOnceTheClaimIsRefused(t *testing.T) {
	backoff := newPlacementBackoff(nil)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	backoff.failedToStart(claim, 1, nil)
	require.True(t, backoff.waitsAfterAFailedStart(claim))

	backoff.refused(claim, 1, nil)

	assert.False(t, backoff.waitsAfterAFailedStart(claim))
}

// A pod is a candidate once it runs and has an address. Its runtime answers
// later. So the wake that a pod gives by joining can be spent before the pod
// can be used. The pod wakes the claim again when it turns ready.
func TestReconcileTriesAClaimThatCouldNotStartAgainWhenAPodTurnsReady(t *testing.T) {
	first := claimWithCost(100, 100)
	first.Name = "first"
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	stays := claimOnPod("stays", pod.Name, modelv1alpha1.ModelClaimActive, 100, 100)
	stays.Status.Instances[0].Port = 9001
	stays.Status.Instances[0].KVLimitBytes = 900
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("stays", 100, 900)}
	r, runtime := newReconciler(t, first, pod, stays)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The runtime of this pod refuses every start.
	runtime.failActivateOn = map[string]bool{pod.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, "first"))

	// A pod joins, and its runtime does not answer yet. The try goes to the
	// pod that refused before.
	joined, joinedSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
	require.NoError(t, r.Create(context.Background(), joined))
	runtime.nilSnapshots = map[string]bool{joined.Status.PodIP: true}
	reconcileOnce(t, r, "first")
	require.Equal(t, []string{pod.Status.PodIP, pod.Status.PodIP}, runtime.activatedOn)

	// The pod turns ready. The clock has not moved.
	runtime.nilSnapshots = nil
	runtime.snapshots[joined.Status.PodIP] = joinedSnapshot
	ready := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(joined), ready))
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, r.Status().Update(context.Background(), ready))
	reconcileOnce(t, r, "first")

	assert.Equal(t, []string{pod.Status.PodIP, pod.Status.PodIP, joined.Status.PodIP}, runtime.activatedOn)
}

// The guard is for a claim that waits for room. A claim no card could ever
// hold gains nothing from a neighbour that has gone, so it goes on waiting
// longer.
func TestReconcileDoesNotStartATooLargeClaimOverWhenThePoolChangedUnderItsTry(t *testing.T) {
	pm := claimWithCost(1500, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 700
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 700)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		now = now.Add(want)
	}
	require.Equal(t, "TooLargeForAnyCard", scheduled(t, r, pm.Name).Reason)

	// The cache still lists the neighbour, and the API server does not.
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(getModel(t, r, pm.Name), pod.DeepCopy()).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()
	runtime.snapshots[pod.Status.PodIP].Models = nil

	assert.Equal(t, 40*time.Second, reconcileFor(t, r, pm.Name))
}

// A pod of the pool starts a pass of every claim in its namespace when it
// joins and when it changes. A claim that reads Failed is among them: a pod
// that joins is what it waits for after a start that failed.
func TestAPodOfThePoolStartsAPassOfEveryClaim(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	filter := modelPoolPodFilter()
	assert.True(t, filter.Create(event.CreateEvent{Object: pod}))
	assert.True(t, filter.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}))
	stranger := pod.DeepCopy()
	stranger.Labels = map[string]string{"app": "something-else"}
	assert.False(t, filter.Create(event.CreateEvent{Object: stranger}))
	assert.False(t, filter.Update(event.UpdateEvent{ObjectOld: stranger, ObjectNew: stranger}))

	waiting := claimWithCost(300, 100)
	waiting.Name = "waiting"
	failed := claimWithCost(300, 100)
	failed.Name = "failed"
	failed.Status.Phase = modelv1alpha1.ModelClaimFailed
	r, _ := newReconciler(t, waiting, failed, pod)

	var started []string
	for _, request := range enqueueModelClaimsForPod(r.Client)(context.Background(), pod) {
		started = append(started, request.Name)
	}
	assert.ElementsMatch(t, []string{"waiting", "failed"}, started)
}

// A claim with an engine is looked at every round, whatever its other start
// waits for. The pass whose start fails says so on the claim. A pass inside
// the wait tries nothing, and the claim reads as its instances do.
func TestReconcileChecksAClaimWithAnEngineEveryRoundAfterAStartThatFailed(t *testing.T) {
	pm := claimWithCost(100, 100)
	two := int32(2)
	pm.Spec.Replicas = &two
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 900,
	}}
	serves, servesSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	servesSnapshot.Models = []RuntimeSnapshotModel{engineHolding(pm.Name, 100, 900)}
	refuses, refusesSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
	r, runtime := newReconciler(t, pm, serves, refuses)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		serves.Status.PodIP:  servesSnapshot,
		refuses.Status.PodIP: refusesSnapshot,
	}
	runtime.failActivateOn = map[string]bool{refuses.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	ready := func() string {
		condition := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
			string(modelv1alpha1.ModelClaimConditionReady))
		require.NotNil(t, condition)
		return condition.Reason
	}

	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Phase)
	assert.Equal(t, "ActivateFailed", ready())

	now = now.Add(DefaultRequeueDuration / 2)
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Len(t, runtime.activateCalls, 1, "a pass inside the wait starts nothing")
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Phase)
	assert.NotEqual(t, "ActivateFailed", ready())

	// The second start fails as well. The claim would wait 20 seconds for
	// its third, and comes back after 10 for the engine it has.
	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	assert.Len(t, runtime.activateCalls, 2)
}

// A pod wakes a waiting claim once by turning ready. A pod that a try has seen
// ready is remembered as ready. So a pod that keeps turning not ready and
// ready again does not take the wait away.
func TestPlacementBackoffIsWokenOnceByAPodThatTurnsReady(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	notReady := roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400}}
	ready := roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400, ready: true}}
	for name, refuse := range map[string]func(b *placementBackoff, room roomSignature) time.Duration{
		"a claim that waits for room": func(b *placementBackoff, room roomSignature) time.Duration {
			wait := b.refused(claim, 1, room)
			b.statusWritten(claim)
			return wait
		},
		"a claim whose engine could not be started": func(b *placementBackoff, room roomSignature) time.Duration {
			return b.failedToStart(claim, 1, room)
		},
	} {
		backoff := newPlacementBackoff(func() time.Time { return now })
		require.Equal(t, 10*time.Second, refuse(backoff, notReady), name)

		// The pod turns ready, and the claim is tried at once.
		due, _ := backoff.due(claim, 1, ready)
		require.True(t, due, name)
		require.Equal(t, 10*time.Second, refuse(backoff, ready), name)

		// The pod is not ready when the wait is up, and ready a moment later.
		now = now.Add(10 * time.Second)
		due, _ = backoff.due(claim, 1, notReady)
		require.True(t, due, name)
		require.Equal(t, 20*time.Second, refuse(backoff, notReady), name)
		due, _ = backoff.due(claim, 1, ready)
		assert.False(t, due, name)

		// The room that was handed in is left as it was.
		assert.False(t, notReady["warm-1/u1"].ready, name)
	}
}

// Through Reconcile: a claim on the card declares nothing, so the card is
// turned away. A neighbour that declares less beside it frees no room, and
// the waiting claim sits out its wait.
func TestReconcileKeepsAClaimWaitingWhileItsCardHasAHole(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	neighbour.Status.Instances[0].KVLimitBytes = 300
	undeclared := claimOnPod("undeclared", pod.Name, modelv1alpha1.ModelClaimActive, 100, 100)
	undeclared.Status.Instances[0].Port = 9002
	undeclared.Spec.PerGPU = nil
	snapshot.Models = []RuntimeSnapshotModel{
		engineHolding("neighbour", 100, 300), engineHolding("undeclared", 100, 300),
	}
	r, runtime := newReconciler(t, pm, pod, neighbour, undeclared)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		now = now.Add(want)
	}
	require.Equal(t, time.Minute, reconcileFor(t, r, pm.Name))
	require.Contains(t, scheduled(t, r, pm.Name).Message, "could not be judged: undeclared runs there")

	shrunk := getModel(t, r, "neighbour")
	shrunk.Spec.PerGPU.MaximumFootprint = *resource.NewQuantity(200, resource.BinarySI)
	require.NoError(t, r.Update(context.Background(), shrunk))

	assert.Equal(t, time.Minute, reconcileFor(t, r, pm.Name))
}

// A claim that starts over waits from the shortest wait again, and keeps what
// it has seen of the pods. A claim that does not wait has nothing to start
// over.
func TestPlacementBackoffKeepsWhatAClaimHasSeenWhenItStartsOver(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	notReady := roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400}}
	ready := roomSignature{"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400, ready: true}}
	backoff := newPlacementBackoff(func() time.Time { return now })

	backoff.startOver(claim)
	require.Empty(t, backoff.attempts)

	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		require.Equal(t, want, backoff.refused(claim, 1, ready))
	}
	backoff.startOver(claim)
	require.Equal(t, 10*time.Second, backoff.refused(claim, 1, notReady))
	backoff.statusWritten(claim)

	due, left := backoff.due(claim, 1, ready)
	assert.False(t, due)
	assert.Equal(t, 10*time.Second, left)
}

// A refusal without a listing of the claims keeps the room that the claim
// remembered. So a pod that joins still wakes the claim. A claim that has
// never seen the pool has nothing to compare, and sits out its wait.
func TestPlacementBackoffKeepsTheRoomItRemembersAfterARefusalWithoutAListing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	one := roomSignature{"warm-1/u1": {ready: true}}
	two := roomSignature{"warm-1/u1": {ready: true}, "warm-2/u2": {}}

	backoff := newPlacementBackoff(func() time.Time { return now })
	backoff.refused(claim, 1, one)
	backoff.refused(claim, 1, nil)
	backoff.statusWritten(claim)
	due, left := backoff.due(claim, 1, one)
	assert.False(t, due)
	assert.Equal(t, 20*time.Second, left)
	due, _ = backoff.due(claim, 1, two)
	assert.True(t, due, "a pod joined")

	backoff = newPlacementBackoff(func() time.Time { return now })
	backoff.refused(claim, 1, nil)
	backoff.statusWritten(claim)
	due, left = backoff.due(claim, 1, two)
	assert.False(t, due)
	assert.Equal(t, 10*time.Second, left)
}

// Two pods that turn ready in turn wake a waiting claim once each. A claim
// that is woken keeps what it has seen of the pods. So the first pod is still
// remembered as ready when the second one wakes the claim.
func TestPlacementBackoffIsWokenOnceByEachOfTwoPodsThatTurnReady(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}
	pool := func(first, second bool) roomSignature {
		return roomSignature{
			"warm-1/u1": {instances: 1, awake: 1, promisedBytes: 400, ready: first},
			"warm-2/u2": {instances: 1, awake: 1, promisedBytes: 400, ready: second},
		}
	}
	backoff := newPlacementBackoff(func() time.Time { return now })
	backoff.failedToStart(claim, 1, pool(false, false))

	for _, turn := range []struct{ first, second bool }{{true, false}, {false, true}} {
		due, _ := backoff.due(claim, 1, pool(turn.first, turn.second))
		require.True(t, due)
		require.Equal(t, 10*time.Second, backoff.failedToStart(claim, 1, pool(turn.first, turn.second)))
	}

	for i := 0; i < 5; i++ {
		due, left := backoff.due(claim, 1, pool(i%2 == 0, i%2 == 1))
		assert.False(t, due)
		assert.Equal(t, 10*time.Second, left)
	}
}

// The guard starts a claim over, and the claim keeps what it has seen of the
// pods. A pod that an earlier try saw ready does not wake it by turning ready
// again.
func TestReconcileKeepsWhatAClaimHasSeenOfThePodsWhenItStartsOver(t *testing.T) {
	r, runtime, pm, clock := aClaimWaitingForRoom(t)
	setReady := func(ready bool) {
		pod := &corev1.Pod{}
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "warm-1"}, pod))
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		require.NoError(t, r.Status().Update(context.Background(), pod))
	}
	setReady(true)
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}

	// The pod is not ready at the try that the neighbour's leaving straddles.
	setReady(false)
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(getModel(t, r, pm.Name), pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()
	exiting := engineHolding("neighbour", 100, 600)
	exiting.Phase = runtimePhaseStopping
	exiting.Ready = false
	runtime.snapshots[pod.Status.PodIP].Models = []RuntimeSnapshotModel{exiting}
	require.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name), "the claim starts over")

	// The pod turns ready three seconds later.
	*clock = clock.Add(3 * time.Second)
	setReady(true)

	assert.Equal(t, 7*time.Second, reconcileFor(t, r, pm.Name), "the claim sits out the rest of its wait")
}

// Through Reconcile: a pod that flaps for ten minutes costs a claim whose
// engine cannot be started two tries more than a pod that stays as it is. One
// is the try that the pod wakes. The other comes from the waits after it,
// which start from the shortest again.
func TestReconcileKeepsTheWaitOfAClaimWhileAPodFlaps(t *testing.T) {
	setReady := func(r *ModelClaimReconciler, ready bool) {
		pod := &corev1.Pod{}
		require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "warm-1"}, pod))
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		require.NoError(t, r.Status().Update(context.Background(), pod))
	}
	tries := func(flapping bool) int {
		first := claimWithCost(100, 100)
		first.Name = "first"
		pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
		r, runtime := newReconciler(t, first, pod)
		r.Recorder = record.NewFakeRecorder(1 << 12)
		runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
		runtime.failActivate = true
		now := time.Unix(1_700_000_000, 0)
		start := now
		r.Backoff = newPlacementBackoff(func() time.Time { return now })
		setReady(r, false)
		wait := reconcileFor(t, r, "first")
		for now.Sub(start) < 10*time.Minute {
			// The wait is up, and the pod is not ready. It turns ready a
			// second later, and not ready again.
			now = now.Add(wait)
			wait = reconcileFor(t, r, "first")
			if !flapping {
				continue
			}
			now = now.Add(time.Second)
			setReady(r, true)
			if woken := reconcileFor(t, r, "first"); woken < wait {
				wait = woken
			}
			setReady(r, false)
			reconcileFor(t, r, "first")
		}
		return len(runtime.activateCalls)
	}

	steady := tries(false)
	require.Equal(t, 13, steady)
	assert.Equal(t, steady+2, tries(true))
}

// The wait of a claim is for the instance it could not place. A claim that
// loses an instance has another need, so it starts over and is tried at once.
// Here the pod of its engine goes, and nothing else changes.
func TestReconcileTriesAClaimAtOnceThatLostAnInstanceWithItsPod(t *testing.T) {
	pm := claimWithCost(100, 100)
	two := int32(2)
	pm.Spec.Replicas = &two
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 900,
	}}
	serves, servesSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	servesSnapshot.Models = []RuntimeSnapshotModel{engineHolding(pm.Name, 100, 900)}
	refuses, refusesSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
	r, runtime := newReconciler(t, pm, serves, refuses)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		serves.Status.PodIP:  servesSnapshot,
		refuses.Status.PodIP: refusesSnapshot,
	}
	runtime.failActivateOn = map[string]bool{refuses.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	for _, step := range []time.Duration{10 * time.Second, 20 * time.Second} {
		reconcileOnce(t, r, pm.Name)
		now = now.Add(step)
	}
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.activateCalls, 3, "the next start is 40 seconds away")

	require.NoError(t, r.Delete(context.Background(), serves))
	now = now.Add(time.Second)
	wait := reconcileFor(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 4, "the claim has no engine left, and is tried at once")
	assert.Equal(t, DefaultRequeueDuration, wait, "its wait starts from the shortest again")
}

// The same holds for an instance that the health check drops. The runtime
// knows no engine for it, and refuses to start one.
func TestReconcileTriesAClaimAtOnceThatLostAnInstanceInTheHealthCheck(t *testing.T) {
	pm := claimWithCost(100, 100)
	two := int32(2)
	pm.Spec.Replicas = &two
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 900,
	}}
	first, firstSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	firstSnapshot.Models = []RuntimeSnapshotModel{engineHolding(pm.Name, 100, 900)}
	second, secondSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
	r, runtime := newReconciler(t, pm, first, second)
	r.Recorder = record.NewFakeRecorder(1 << 10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		first.Status.PodIP:  firstSnapshot,
		second.Status.PodIP: secondSnapshot,
	}
	runtime.failActivateOn = map[string]bool{second.Status.PodIP: true}
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	for _, step := range []time.Duration{10 * time.Second, 20 * time.Second} {
		reconcileOnce(t, r, pm.Name)
		now = now.Add(step)
	}
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.activateCalls, 3, "the next start is 40 seconds away")

	// The engine goes, and no runtime starts one from now on. Two passes of
	// the health check find that out, and the second drops the instance.
	firstSnapshot.Models = nil
	runtime.failActivate = true
	for i := 0; i < 2; i++ {
		now = now.Add(time.Second)
		reconcileOnce(t, r, pm.Name)
	}
	require.Empty(t, getModel(t, r, pm.Name).Status.Instances)
	started := len(runtime.activateCalls)

	now = now.Add(time.Second)
	wait := reconcileFor(t, r, pm.Name)

	assert.Len(t, runtime.activateCalls, started+1, "the claim has no engine left, and is tried at once")
	assert.Equal(t, DefaultRequeueDuration, wait)
}

// A record that was taken back after a start that failed is no engine that
// left a card. The cache can still show it when the API server does not. A
// try that meets such a moment goes on waiting longer.
func TestReconcileDoesNotStartAClaimOverForARecordThatWasTakenBack(t *testing.T) {
	r, _, pm, clock := aClaimWaitingForRoom(t)
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second} {
		require.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	// The API server lists the claims as they are. The cache also shows the
	// record of another claim, whose start has just failed.
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).
		WithObjects(getModel(t, r, pm.Name), getModel(t, r, "neighbour"), pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).Build()
	other := claimOnPod("other", pod.Name, modelv1alpha1.ModelClaimActivating, 50, 50)
	require.NoError(t, r.Create(context.Background(), other))

	assert.Equal(t, 40*time.Second, reconcileFor(t, r, pm.Name))
}

// A reconciler that is built by hand has no backoff. It gets one on the first
// call, and keeps it, so that a refusal is still known on the next pass.
func TestBackoffKeepsTheWaitsOfAReconcilerBuiltByHand(t *testing.T) {
	r := &ModelClaimReconciler{}
	claim := types.NamespacedName{Namespace: testNamespace, Name: "first"}

	r.backoff().refused(claim, 1, nil)
	r.backoff().statusWritten(claim)

	due, _ := r.backoff().due(claim, 1, nil)
	assert.False(t, due)
}
