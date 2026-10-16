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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// serving is a claim whose engine serves on a card, idle since a time.
type serving struct {
	name, pod        string
	footprint, floor int64
	idleSince        time.Time
}

// servingPool is a pool with the given policy and 1000-byte cards on pods
// warm-1 and, with two cards, warm-2. The claims given serve on them, idle,
// each mapping 50 bytes of KV, and the pool policy last saw each one busy at
// its idleSince. The clocks read 08:00:05. A sleep leaves an engine holding 60
// bytes.
func servingPool(t *testing.T, policy string, cards int, claims ...serving) (*ModelClaimReconciler, *fakeRuntime) {
	t.Helper()
	deployment, replicaSet, first := warmPoolObjects(policy)
	first.UID = types.UID("warm-1-uid")
	pods := []*corev1.Pod{first}
	if cards == 2 {
		second := first.DeepCopy()
		second.Name, second.UID, second.Status.PodIP = "warm-2", types.UID("warm-2-uid"), testPeerIP
		pods = append(pods, second)
	}
	objects := []client.Object{deployment, replicaSet}
	snapshots := map[string]*RuntimeSnapshot{}
	byName := map[string]*corev1.Pod{}
	for _, pod := range pods {
		objects = append(objects, pod)
		byName[pod.Name] = pod
		snapshots[pod.Status.PodIP] = &RuntimeSnapshot{
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMUsableBytes: 1000}},
		}
	}
	type seen struct {
		pod   *corev1.Pod
		model RuntimeSnapshotModel
		at    time.Time
	}
	var observations []seen
	for i, s := range claims {
		claim := withFinalizer(claimOnPod(s.name, s.pod, modelv1alpha1.ModelClaimActive, s.footprint, s.floor))
		claim.UID = types.UID(s.name + "-uid")
		claim.Status.Instances[0].Port = int32(9001 + i)
		claim.Status.Instances[0].KVLimitBytes = 100
		objects = append(objects, claim)
		engine := engineHolding(s.name, 50, 100)
		engine.Port = int32(9001 + i)
		engine.ClaimRef = &ModelClaimRef{Namespace: testNamespace, Name: s.name, UID: string(claim.UID)}
		total := int64(10)
		engine.RequestSuccessTotal = &total
		snapshot := snapshots[byName[s.pod].Status.PodIP]
		snapshot.Models = append(snapshot.Models, engine)
		observations = append(observations, seen{pod: byName[s.pod], model: engine, at: s.idleSince})
	}
	r, runtime := newReconciler(t, objects...)
	runtime.snapshots = snapshots
	now := time.Date(2026, time.October, 1, 7, 0, 0, 0, time.UTC)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	for _, o := range observations {
		now = o.at
		busy := o.model
		busy.RequestsRunning = 1
		_, observed := r.PoolPolicy.observeSnapshot(o.pod, &RuntimeSnapshot{Models: []RuntimeSnapshotModel{busy}})
		require.True(t, observed)
	}
	now = time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC)
	r.Now = func() time.Time { return now }
	runtime.onSleep = func(req *SleepRequest) {
		for _, snapshot := range runtime.snapshots {
			for i := range snapshot.Models {
				if snapshot.Models[i].ModelName == req.ModelName {
					snapshot.Models[i].Phase = runtimePhaseSleeping
					snapshot.Models[i].Ready = false
					snapshot.Models[i].SleepingFootprintBytes = bytesOf(60)
				}
			}
		}
	}
	return r, runtime
}

// newClaim is a claim, not placed yet, that declares 300+100 and selects the
// pool, created at the given time.
func newClaim(t *testing.T, r *ModelClaimReconciler, name string, created time.Time) {
	t.Helper()
	claim := withFinalizer(claimOnPod(name, "", modelv1alpha1.ModelClaimActive, 300, 100))
	claim.Status.Instances = nil
	claim.CreationTimestamp = metav1.NewTime(created)
	require.NoError(t, r.Create(context.Background(), claim))
}

func scheduledOf(t *testing.T, r *ModelClaimReconciler, name string) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(getModel(t, r, name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, condition)
	return condition
}

var (
	at0758 = time.Date(2026, time.October, 1, 7, 58, 0, 0, time.UTC)
	at0759 = time.Date(2026, time.October, 1, 7, 59, 0, 0, time.UTC)
)

func TestReconcileMakesRoomForANewClaimByPuttingTheIdlestToSleep(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName, "the one idle longest")
	assert.Empty(t, getModel(t, r, "x").Status.Instances)
	scheduled := scheduledOf(t, r, "x")
	assert.Equal(t, scheduledReasonMakingRoom, scheduled.Reason)
	assert.Contains(t, scheduled.Message, "model a was put to sleep on pod warm-1 to make room")
	card, held := r.reservations().heldFor(testNamespace, "x", r.now())
	require.True(t, held)
	assert.Equal(t, "warm-1", card.Name)
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "SleptToMakeRoom model a")
	assert.Contains(t, events, "Normal MakingRoom")

	reconcileOnce(t, r, "x")

	got := getModel(t, r, "x")
	require.Len(t, got.Status.Instances, 1, "a asleep holds 60 bytes, so x fits")
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Len(t, runtime.sleepCalls, 1)
	_, held = r.reservations().heldFor(testNamespace, "x", r.now())
	assert.False(t, held, "the card is let go once the claim is placed")
}

func TestReconcileMakesRoomForANewerClaimWhenNoneCanBeMadeForTheOlder(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	// "huge" has waited longest, but needs more than the whole card.
	huge := withFinalizer(claimOnPod("huge", "", modelv1alpha1.ModelClaimActive, 1000, 100))
	huge.Status.Instances = nil
	huge.CreationTimestamp = metav1.NewTime(at0758)
	require.NoError(t, r.Create(context.Background(), huge))
	newClaim(t, r, "x", at0759)

	reconcileOnce(t, r, "huge")

	assert.Empty(t, runtime.sleepCalls, "no sleep would give huge room")

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1, "huge does not hold x up")
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
}

func TestReconcileLeavesACardToARequestToWakeAnEngine(t *testing.T) {
	r, runtime := queueCard(t, 1000,
		queued{name: "a", footprint: 300, floor: 100, idleSince: at0758},
		queued{name: "waker", footprint: 300, floor: 100, asleep: true, askedAt: "2026-10-01T08:00:00Z"})
	newClaim(t, r, "x", at0759)

	reconcileOnce(t, r, "x")

	assert.Empty(t, runtime.sleepCalls, "the card has room for waker, and the room is its")
	assert.Empty(t, getModel(t, r, "x").Status.Instances)
}

func TestReconcileLetsTheCardGoWhenTheSleepThatMakesRoomFails(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	runtime.sleepErr = &runtimeRefusal{"sleep failed: boom"}

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1)
	_, held := r.reservations().heldFor(testNamespace, "x", r.now())
	assert.False(t, held, "no room was made, so the card is not kept from the other claims")
	assert.NotEqual(t, scheduledReasonMakingRoom, scheduledOf(t, r, "x").Reason)
}

func TestReconcileKeepsTheRoomMadeForAClaimFromOthers(t *testing.T) {
	r, _ := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	reconcileOnce(t, r, "x")
	newClaim(t, r, "y", time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC))

	reconcileOnce(t, r, "y")

	assert.Empty(t, getModel(t, r, "y").Status.Instances, "the room a's sleep gave back is x's")
	refused := scheduledOf(t, r, "y")
	assert.Equal(t, "NoMatchingPods", refused.Reason, "y has not waited longest")
	assert.Contains(t, refused.Message, "is held for another model that room is being made for")

	reconcileOnce(t, r, "x")

	require.Len(t, getModel(t, r, "x").Status.Instances, 1)
}

func TestReconcileMakesNoRoomWhereItShouldNot(t *testing.T) {
	recent := time.Date(2026, time.October, 1, 7, 59, 50, 0, time.UTC)
	for name, tc := range map[string]struct {
		policy string
		idle   time.Time
		older  bool
	}{
		"a pool that keeps the wake reserve":      {policy: `{"lifecycle":{"sleepAfterSeconds":600}}`, idle: at0758},
		"engines that have not idled long enough": {policy: keepNoWakeReserve, idle: recent},
		"a claim that has not waited longest":     {policy: keepNoWakeReserve, idle: at0758, older: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, runtime := servingPool(t, tc.policy, 1,
				serving{"a", "warm-1", 300, 100, tc.idle}, serving{"b", "warm-1", 300, 100, tc.idle})
			newClaim(t, r, "x", at0759)
			if tc.older {
				newClaim(t, r, "older", at0758)
			}

			reconcileOnce(t, r, "x")

			assert.Empty(t, runtime.sleepCalls)
			assert.Equal(t, "NoMatchingPods", scheduledOf(t, r, "x").Reason)
			_, held := r.reservations().heldFor(testNamespace, "x", r.now())
			assert.False(t, held)
		})
	}
}

func TestDivisionLendsNoRoomHeldForAClaim(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	reconcileOnce(t, r, "x")
	runtime.kvLimitCalls = nil
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "warm-1"}, pod))
	r.divisions().divided(cardOf(pod), "")

	r.divideCardsAsListed(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))

	limits := map[string]int64{}
	for _, call := range runtime.kvLimitCalls {
		limits[call.ModelName] = call.LimitBytes
	}
	assert.Equal(t, int64(100+140), limits["b"], "b shares 1000 less a's 60, its own 400 and x's 400")
}

func TestReconcileMakesRoomWhereTheFewestEnginesHaveToSleep(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 2,
		// warm-1 has 10 bytes left and needs two of its engines asleep for x
		// to fit. warm-2 has 200 left and needs one.
		serving{"c", "warm-1", 280, 50, at0758}, serving{"d", "warm-1", 280, 50, at0758},
		serving{"e", "warm-1", 280, 50, at0758},
		serving{"a", "warm-2", 300, 100, at0759}, serving{"b", "warm-2", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	// Each engine was seen asleep before, holding 60 bytes.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		r.footprints().note(getModel(t, r, name), 60)
	}

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName, "warm-2 needs the fewest sleeps, though warm-1's engines idled longer")
	card, held := r.reservations().heldFor(testNamespace, "x", r.now())
	require.True(t, held)
	assert.Equal(t, "warm-2", card.Name)
}

// A claim that is gone leaves nothing behind in memory: not the card held for
// it, nor what its engine held asleep. That holds whether this controller saw
// No engine on either pod has been seen asleep, so no plan is known. Room is
// made first where the card is nearest to fitting the claim, though the
// engines on the other pod idled longer.
func TestReconcileMakesRoomFirstNearestToFittingWhenNoFigureIsKnown(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 2,
		// warm-1 has 300 bytes left. warm-2 has 200, and its engines idled
		// longer. One sleep on either would do, if a sleep gave everything back.
		serving{"p", "warm-1", 400, 100, at0759}, serving{"q", "warm-1", 150, 50, at0759},
		serving{"a", "warm-2", 300, 100, at0758}, serving{"b", "warm-2", 300, 100, at0758})
	newClaim(t, r, "x", at0759)

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "p", runtime.sleepCalls[0].ModelName)
	card, held := r.reservations().heldFor(testNamespace, "x", r.now())
	require.True(t, held)
	assert.Equal(t, "warm-1", card.Name)
}

// Another model was seen holding more asleep than this pod's idle engine is
// charged. That says nothing of this engine, so room is still made here.
func TestReconcileMakesRoomThoughAnotherModelHeldMoreAsleep(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	large := withFinalizer(claimOnPod("large", "", modelv1alpha1.ModelClaimActive, 900, 100))
	large.UID = "large-uid"
	r.footprints().note(large, 900)
	newClaim(t, r, "x", at0759)

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
}

// listHook is a reader that changes what it lists before returning it.
type listHook struct {
	client.Reader
	after func(client.ObjectList)
}

func (l *listHook) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := l.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	l.after(list)
	return nil
}

// The cache still shows the older claim waiting. The API server already has
// its instance, so x is the claim that has waited longest.
func TestReconcileMakesRoomByTheClaimsAsTheAccountListsThem(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "older", at0758)
	newClaim(t, r, "x", at0759)
	r.APIReader = &listHook{Reader: r.Client, after: func(list client.ObjectList) {
		claims, ok := list.(*modelv1alpha1.ModelClaimList)
		if !ok {
			return
		}
		for i := range claims.Items {
			if claims.Items[i].Name == "older" {
				claims.Items[i].Status.Instances = []modelv1alpha1.ModelClaimInstance{{
					Pod: "warm-9", Phase: modelv1alpha1.ModelClaimActivating,
				}}
			}
		}
	}}

	reconcileOnce(t, r, "x")

	require.Len(t, runtime.sleepCalls, 1, "older no longer waits, so room is made for x")
}

// The cache and the account can disagree for a moment about which claim an
// engine serves. An idle engine that the account does not know by name is
// passed over, and the engine beside it is still planned to sleep.
func TestPlanRoomPassesOverAnIdleEngineTheAccountDoesNotKnow(t *testing.T) {
	r, _ := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	ctx := context.Background()
	pod := podNamed(t, r, "warm-1")
	pods := []corev1.Pod{*pod}
	readings := newRuntimeReadings(r.Runtime)
	claims, err := r.listClaimsForAccount(ctx, testNamespace)
	require.NoError(t, err)
	ledger := podLedgersFrom(claims, nil, pods, readings.ofPods(ctx, pods), r.podsWithoutWakeReserve(ctx, pods))[pod.Name]
	require.True(t, ledger.judgeable)
	engines := make([]engineOnPod, 0, len(ledger.engines))
	for _, engine := range ledger.engines {
		if engine.claimName == "a" {
			// a, idle longest, is still charged, under a name the cache does
			// not use.
			engine.claimName = "a-as-the-account-saw-it"
		}
		engines = append(engines, engine)
	}
	ledger.engines = engines

	plan, found := r.planRoom(ctx, getModel(t, r, "x"), pod, ledger, 400, readings)

	require.True(t, found, "the pod is still planned on")
	require.Len(t, plan.sleeps, 1)
	assert.Equal(t, "b", plan.sleeps[0].claim.Name)
}

func TestReconcileMakesNoRoomForAClaimWhoseLastStartFailed(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	newClaim(t, r, "x", at0759)
	claim := getModel(t, r, "x")
	claim.Status.Phase = modelv1alpha1.ModelClaimFailed
	require.NoError(t, r.Status().Update(context.Background(), claim))

	reconcileOnce(t, r, "x")

	assert.Empty(t, runtime.sleepCalls, "room does not help a claim whose start failed")
}

// A claim that is gone leaves nothing behind in memory: not the card held for
// it, nor what its engine held asleep. That holds whether this controller saw
// it go, or someone else removed its finalizer.
func TestReconcileForgetsWhatItKeptForAClaimThatIsGone(t *testing.T) {
	for name, finalizerRemovedElsewhere := range map[string]bool{
		"deleted":                     false,
		"finalizer removed elsewhere": true,
	} {
		t.Run(name, func(t *testing.T) {
			pm := claimWithCost(100, 100)
			r, _ := newReconciler(t, pm)
			claim := getModel(t, r, pm.Name)
			card := types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}
			require.True(t, r.reservations().hold(card, claim.Name, 200, r.now()))
			r.footprints().note(claim, 300)

			if finalizerRemovedElsewhere {
				claim.Finalizers = nil
				require.NoError(t, r.Update(context.Background(), claim))
			}
			require.NoError(t, r.Delete(context.Background(), claim))
			reconcileOnce(t, r, pm.Name)

			_, held := r.reservations().heldFor(testNamespace, pm.Name, r.now())
			assert.False(t, held, "the card held for the claim is let go")
			assert.Empty(t, r.footprints().byClaim, "what its engine held asleep is forgotten")
		})
	}
}

// A claim's reading is its own. A later claim of the same name has none, until
// its own engine sleeps.
func TestSleepingFootprintsTellAClaimFromALaterOneOfTheSameName(t *testing.T) {
	footprints := newSleepingFootprints()
	first := sampleModelClaim()
	first.UID = "first"
	footprints.note(first, 300)
	other := sampleModelClaim()
	other.Name, other.UID = "other", "other"
	footprints.note(other, 500)

	seen, known := footprints.seenAsleep(first)
	require.True(t, known)
	assert.Equal(t, int64(300), seen)
	later := first.DeepCopy()
	later.UID = "later"
	_, known = footprints.seenAsleep(later)
	assert.False(t, known, "another engine's figure is no stand-in")

	footprints.forget(client.ObjectKeyFromObject(first))
	_, known = footprints.seenAsleep(first)
	assert.False(t, known)
	assert.Len(t, footprints.byClaim, 1)
}

// A pod given its card by a resource claim requests no GPU, and its runtime
// reports one card. A claim that runs on two cannot run there, however much
// room is made, so no engine is put to sleep for it.
func TestReconcileMakesNoRoomOnAPodWhoseCardsDoNotSuitTheClaim(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	ctx := context.Background()
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	for i := range pod.Spec.Containers {
		delete(pod.Spec.Containers[i].Resources.Limits, nvidiaGPUResourceName)
		delete(pod.Spec.Containers[i].Resources.Requests, nvidiaGPUResourceName)
	}
	require.NoError(t, r.Update(ctx, pod))
	runtime.snapshots[pod.Status.PodIP].Accelerators[0].HBMTotalBytes = 1000
	claim := withFinalizer(claimOnPod("x", "", modelv1alpha1.ModelClaimActive, 300, 100))
	claim.Status.Instances = nil
	claim.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{"--tensor-parallel-size": "2"}}
	claim.CreationTimestamp = metav1.NewTime(at0759)
	require.NoError(t, r.Create(ctx, claim))

	reconcileOnce(t, r, "x")

	assert.Empty(t, runtime.sleepCalls)
	assert.Empty(t, runtime.activateCalls)
	scheduled := scheduledOf(t, r, "x")
	assert.Equal(t, "NoMatchingPods", scheduled.Reason)
	assert.Contains(t, scheduled.Message, "warm-1 reports 1 GPU(s), and the model runs on 2")
	_, held := r.reservations().heldFor(testNamespace, "x", r.now())
	assert.False(t, held)
}
