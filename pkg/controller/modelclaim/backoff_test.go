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
	backoff.failedToStart(other, 1)
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
	pm := claimWithCost(700, 100)
	small, smallSnapshot := sizedWarmPod("warm-small", "10.0.0.1", 500)
	roomy, roomySnapshot := sizedWarmPod("warm-roomy", "10.0.0.2", 2000)
	r, runtime := newReconciler(t, pm, small)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{
		small.Status.PodIP: smallSnapshot,
		roomy.Status.PodIP: roomySnapshot,
	}
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}

	reconcileOnce(t, r, pm.Name)
	_, waiting := r.Backoff.attempts[claim]
	require.True(t, waiting)

	require.NoError(t, r.Create(context.Background(), roomy))
	now = now.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	_, waiting = r.Backoff.attempts[claim]
	assert.False(t, waiting, "a placed claim that waits again starts from the shortest wait")
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

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Len(t, getModel(t, r, pm.Name).Status.Instances, 1)

	// The second replica waits, but the first still has its engine checked
	// every round, so the claim does not sleep through the wait.
	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
}

func TestPlacementBackoffStartsOverWhenRoomMayHaveAppeared(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}
	// Three instances, one of whose claim declares nothing.
	before := roomSignature{"warm-1/u1": {instances: 3, awake: 3, undeclared: 1, promisedBytes: 800}}
	cases := []struct {
		name       string
		generation int64
		room       roomSignature
		due        bool
	}{
		{"the pool as it was", 1, before, false},
		{"more promised on a card", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 3, undeclared: 1, promisedBytes: 900}}, false},
		{"a pod gone", 1, roomSignature{}, false},
		{"an instance gone", 1, roomSignature{"warm-1/u1": {instances: 2, awake: 2, undeclared: 1, promisedBytes: 400}}, true},
		{"less promised on a card", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 3, undeclared: 1, promisedBytes: 700}}, true},
		{"a hole closed", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 3, promisedBytes: 1200}}, true},
		{"a pod joined", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 3, undeclared: 1, promisedBytes: 800}, "warm-2/u2": {}}, true},
		{"an engine woken", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 4, undeclared: 1, promisedBytes: 800}}, false},
		{"an engine gone to sleep", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 2, undeclared: 1, promisedBytes: 800}}, true},
		{"a pod turned ready", 1, roomSignature{"warm-1/u1": {instances: 3, awake: 3, undeclared: 1, promisedBytes: 800, ready: true}}, true},
		{"the claim's own spec changed", 2, before, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backoff := newPlacementBackoff(func() time.Time { return now })
			backoff.refused(claim, 1, before)
			backoff.refused(claim, 1, before)
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
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{
		*declared, *asleep, *failed, *legacy, *elsewhere,
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

// An engine that goes to sleep gives back the KV it had mapped, so a claim
// that was refused on what the engines hold is tried again at once.
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
	r, _, pm, _ := aClaimWaitingForRoom(t)
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}
	reconcileOnce(t, r, pm.Name)
	require.Equal(t, 1, r.Backoff.attempts[claim].refusals)

	// Another model is recorded on the card, which only takes room away.
	other := claimOnPod("other", "warm-1", modelv1alpha1.ModelClaimActivating, 100, 100)
	recorded := other.Status
	require.NoError(t, r.Create(context.Background(), other))
	other.Status = recorded
	require.NoError(t, r.Status().Update(context.Background(), other))
	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, 1, r.Backoff.attempts[claim].refusals, "the claim was not tried again early")
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
	assert.Contains(t, condition.Message, "the largest holds 48.0 GiB")
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
