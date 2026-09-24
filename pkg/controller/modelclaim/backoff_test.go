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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	due, left = backoff.due(claim, 1, nil)
	assert.False(t, due)
	assert.Equal(t, DefaultRequeueDuration, left)

	now = now.Add(DefaultRequeueDuration)
	due, _ = backoff.due(claim, 1, nil)
	assert.True(t, due)
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
	before := roomSignature{"warm-1/u1": {instances: 3, undeclared: 1, promisedBytes: 800}}
	cases := []struct {
		name       string
		generation int64
		room       roomSignature
		due        bool
	}{
		{"the pool as it was", 1, before, false},
		{"more promised on a card", 1, roomSignature{"warm-1/u1": {instances: 3, undeclared: 1, promisedBytes: 900}}, false},
		{"a pod gone", 1, roomSignature{}, false},
		{"an instance gone", 1, roomSignature{"warm-1/u1": {instances: 2, undeclared: 1, promisedBytes: 400}}, true},
		{"less promised on a card", 1, roomSignature{"warm-1/u1": {instances: 3, undeclared: 1, promisedBytes: 700}}, true},
		{"a hole closed", 1, roomSignature{"warm-1/u1": {instances: 3, promisedBytes: 1200}}, true},
		{"a pod joined", 1, roomSignature{"warm-1/u1": {instances: 3, undeclared: 1, promisedBytes: 800}, "warm-2/u2": {}}, true},
		{"the claim's own spec changed", 2, before, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backoff := newPlacementBackoff(func() time.Time { return now })
			backoff.refused(claim, 1, before)
			backoff.refused(claim, 1, before)

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
	declared := claimOnPod("declared", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	failed := claimOnPod("failed", "warm-1", modelv1alpha1.ModelClaimFailed, 300, 100)
	legacy := claimOnPod("legacy", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
	legacy.Spec.PerGPU = nil
	elsewhere := claimOnPod("elsewhere", "warm-9", modelv1alpha1.ModelClaimActive, 300, 100)
	claims := &modelv1alpha1.ModelClaimList{Items: []modelv1alpha1.ModelClaim{*declared, *failed, *legacy, *elsewhere}}

	room := roomSignatureOf([]corev1.Pod{*pod, *empty}, claims)

	assert.Equal(t, roomSignature{
		"warm-1/uid-1": {instances: 2, undeclared: 1, promisedBytes: 400},
		"warm-2/uid-2": {},
	}, room)
	assert.Nil(t, roomSignatureOf([]corev1.Pod{*pod}, nil), "with no listing there is nothing to compare")
}

func TestFreesRoomPassesTheChangesThatCanFreeACard(t *testing.T) {
	base := claimOnPod("neighbour", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100)
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
		{"an instance added", func(c *modelv1alpha1.ModelClaim) {
			c.Status.Instances = append(c.Status.Instances, modelv1alpha1.ModelClaimInstance{Pod: "warm-2"})
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
	before := roomSignature{"warm-1/u1": {instances: 2, promisedBytes: 800}}
	backoff.refusedAsTooLarge(claim, 1, before)

	due, _ := backoff.due(claim, 1, roomSignature{"warm-1/u1": {instances: 1, promisedBytes: 400}})
	assert.False(t, due, "a neighbour leaving does not make a card large enough")
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
