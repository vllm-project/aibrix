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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestWaitingClaimsPriorityOrdersOnlyEligibleUnplacedClaims(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	low := priorityTestClaim("older-low", 1, created)
	high := priorityTestClaim("newer-high", 10, created.Add(time.Minute))
	invalid := priorityTestClaim("invalid-highest", 100, created.Add(2*time.Minute))
	invalid.Spec.PerGPU = nil
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	r, _ := newReconciler(t, low, high, invalid)

	waiting, claims := r.waitingClaims(context.Background(), low, []corev1.Pod{*pod})

	require.NotNil(t, claims)
	assert.Equal(t, []string{"newer-high", "older-low"}, claimNames(waiting))
}

func TestFirstToMakeRoomForPriorityHonorsUnexpiredReservation(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	low := priorityTestClaim("older-low", 1, created)
	high := priorityTestClaim("newer-high", 10, created.Add(time.Minute))
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	r, _ := newReconciler(t, low, high)
	card := types.NamespacedName{Namespace: testNamespace, Name: pod.Name}
	require.True(t, r.reservations().hold(card, high.Name, 400, r.now()))
	key := client.ObjectKeyFromObject(high)
	r.Backoff.refused(key, high.Generation, roomSignatureOf([]corev1.Pod{*pod}, nil))
	r.Backoff.statusWritten(key)

	assert.False(t, r.firstToMakeRoomFor(context.Background(), low, []corev1.Pod{*pod}, nil, newRuntimeReadings(nil)))
}

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

func newPrioritySleepClaim(t *testing.T, r *ModelClaimReconciler, name string, priority int32, created time.Time) {
	t.Helper()
	claim := withFinalizer(claimOnPod(name, "", modelv1alpha1.ModelClaimActive, 300, 100))
	claim.Status.Instances = nil
	claim.CreationTimestamp = metav1.NewTime(created)
	claim.Spec.Priority = priority
	claim.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionSleepLowerPriority
	require.NoError(t, r.Create(context.Background(), claim))
}

func setClaimPriority(t *testing.T, r *ModelClaimReconciler, name string, priority int32) {
	t.Helper()
	claim := getModel(t, r, name)
	claim.Spec.Priority = priority
	require.NoError(t, r.Update(context.Background(), claim))
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

func TestReconcileNeverUsesTheExistingMakeRoomPath(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	claim := withFinalizer(claimOnPod("never", "", modelv1alpha1.ModelClaimActive, 300, 100))
	claim.Status.Instances = nil
	claim.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
	require.NoError(t, r.Create(context.Background(), claim))

	reconcileOnce(t, r, claim.Name)

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
	assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, claim.Name).Reason)
}

func TestFirstToMakeRoomForNeverIgnoresDirectlyPlaceablePredecessor(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 2,
		serving{"resident", "warm-1", 300, 100, at0758})
	warm1 := podNamed(t, r, "warm-1")
	warm2 := podNamed(t, r, "warm-2")
	warm1.Labels["placement-test"] = "current"
	warm2.Labels["placement-test"] = "predecessor"
	require.NoError(t, r.Update(context.Background(), warm1))
	require.NoError(t, r.Update(context.Background(), warm2))

	predecessor := withFinalizer(claimOnPod("predecessor", "", modelv1alpha1.ModelClaimActive, 50, 50))
	predecessor.Status.Instances = nil
	predecessor.Spec.Priority = 10
	predecessor.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
	predecessor.Spec.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{
		"placement-test": "predecessor",
	}}
	predecessor.CreationTimestamp = metav1.NewTime(at0758)
	require.NoError(t, r.Create(context.Background(), predecessor))

	current := withFinalizer(claimOnPod("current", "", modelv1alpha1.ModelClaimActive, 800, 100))
	current.Status.Instances = nil
	current.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
	current.Spec.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{
		"placement-test": "current",
	}}
	current.CreationTimestamp = metav1.NewTime(at0759)
	require.NoError(t, r.Create(context.Background(), current))

	ctx := context.Background()
	pods := []corev1.Pod{*warm1, *warm2}
	readings := newRuntimeReadings(runtime)
	claims, err := r.listClaimsForAccount(ctx, testNamespace)
	require.NoError(t, err)
	ledgers := podLedgersFrom(claims, nil, pods, readings.ofPods(ctx, pods),
		r.podsWithoutWakeReserve(ctx, pods))

	assert.True(t, r.firstToMakeRoomFor(ctx, current, pods, ledgers, readings),
		"Never keeps the historical rule that only a predecessor able to make room holds the turn")
}

func TestReconcileSleepLowerPriorityProtectsEqualAndHigherPriorityClaims(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"equal", "warm-1", 300, 100, at0758}, serving{"higher", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "equal", 10)
	setClaimPriority(t, r, "higher", 11)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)

	reconcileOnce(t, r, "preemptor")

	assert.Empty(t, runtime.sleepCalls)
	assert.Equal(t, "NoMatchingPods", scheduledOf(t, r, "preemptor").Reason)
}

func TestReconcileSleepLowerPrioritySleepsNewlyIdleVictim(t *testing.T) {
	recent := time.Date(2026, time.October, 1, 7, 59, 50, 0, time.UTC)
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"low", "warm-1", 300, 100, recent}, serving{"busy", "warm-1", 300, 100, at0758})
	setClaimPriority(t, r, "low", 1)
	setClaimPriority(t, r, "busy", 1)
	runtime.snapshots[podNamed(t, r, "warm-1").Status.PodIP].Models[1].RequestsRunning = 1
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)

	reconcileOnce(t, r, "preemptor")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "low", runtime.sleepCalls[0].ModelName)
	assert.Empty(t, runtime.activateCalls)
	condition := scheduledOf(t, r, "preemptor")
	assert.Equal(t, scheduledReasonMakingRoom, condition.Reason)
	assert.Contains(t, condition.Message, "lower-priority model low")
}

func TestPrioritySleepAcquiresReservationBeforeRuntimeSleep(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	changed := runtime.onSleep
	heldBeforeSleep := false
	runtime.onSleep = func(req *SleepRequest) {
		_, heldBeforeSleep = r.reservations().heldFor(testNamespace, "preemptor", r.now())
		changed(req)
	}

	reconcileOnce(t, r, "preemptor")

	assert.True(t, heldBeforeSleep)
	assert.Len(t, runtime.sleepCalls, 1)
	assert.Empty(t, runtime.activateCalls)
}

func TestPrioritySleepExecutesOneKnownVictimPerPass(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 280, 50, at0758},
		serving{"b", "warm-1", 280, 50, at0759},
		serving{"c", "warm-1", 280, 50, at0759})
	for _, name := range []string{"a", "b", "c"} {
		setClaimPriority(t, r, name, 1)
		r.footprints().note(getModel(t, r, name), 60)
	}
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)

	reconcileOnce(t, r, "preemptor")
	assert.Len(t, runtime.sleepCalls, 1)
	assert.Empty(t, runtime.activateCalls)

	reconcileOnce(t, r, "preemptor")
	assert.Len(t, runtime.sleepCalls, 2)
	assert.Empty(t, runtime.activateCalls)

	reconcileOnce(t, r, "preemptor")
	require.Len(t, runtime.activateCalls, 1)
	require.Len(t, getModel(t, r, "preemptor").Status.Instances, 1)
}

func TestPrioritySleepUnknownFootprintRefreshesAndReplans(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 280, 50, at0758},
		serving{"b", "warm-1", 280, 50, at0759},
		serving{"c", "warm-1", 280, 50, at0759})
	for _, name := range []string{"a", "b", "c"} {
		setClaimPriority(t, r, name, 1)
	}
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)

	reconcileOnce(t, r, "preemptor")

	assert.Len(t, runtime.sleepCalls, 1, "an unknown plan measures one victim")
	assert.GreaterOrEqual(t, runtime.snapshotCalls, 3, "planning, quiet confirmation, and post-Sleep measurement")
	assert.Empty(t, runtime.activateCalls)
	_, known := r.footprints().seenAsleep(getModel(t, r, "a"))
	assert.True(t, known)

	reconcileOnce(t, r, "preemptor")
	assert.Len(t, runtime.sleepCalls, 2, "the next pass replans from measured room")
}

func TestPreemptionReservationSurvivesVictimWakeRequest(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	reconcileOnce(t, r, "preemptor")
	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
	askWake(t, r, "warm-1", "a", "2026-10-01T08:00:06Z")

	reconcileOnce(t, r, "preemptor")

	require.Len(t, runtime.activateCalls, 1, "the victim's wake request cannot cancel room reserved for the preemptor")
	assert.Equal(t, "preemptor", runtime.activateCalls[0].ModelName)
	_, held := r.reservations().heldFor(testNamespace, "preemptor", r.now())
	assert.False(t, held, "successful activation releases the reservation")
}

func TestHigherPriorityWakeKeepsItsReserveWhenPreemptorReconcilesFirst(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"low", "warm-1", 300, 100, at0758},
		serving{"busy", "warm-1", 300, 100, at0759},
		serving{"high", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "low", 1)
	setClaimPriority(t, r, "busy", 10)
	setClaimPriority(t, r, "high", 50)
	high := getModel(t, r, "high")
	high.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
	high.Status.Instances[0].KVLimitBytes = 20
	require.NoError(t, r.Status().Update(context.Background(), high))
	snapshot := runtime.snapshots[podNamed(t, r, "warm-1").Status.PodIP]
	for i := range snapshot.Models {
		if snapshot.Models[i].ModelName == "high" {
			snapshot.Models[i].Phase = runtimePhaseSleeping
			snapshot.Models[i].Ready = false
			snapshot.Models[i].KVUsedBytes = 20
			snapshot.Models[i].KVCapacityBytes = 20
			snapshot.Models[i].SleepingFootprintBytes = bytesOf(60)
		}
	}
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)

	reconcileOnce(t, r, "preemptor")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "low", runtime.sleepCalls[0].ModelName)
	_, held := r.reservations().heldFor(testNamespace, "preemptor", r.now())
	require.True(t, held)
	askWake(t, r, "warm-1", "high", "2026-10-01T08:00:06Z")

	reconcileOnce(t, r, "preemptor")

	assert.Empty(t, runtime.activateCalls, "the preemptor must account for the higher-priority wake reserve")
	_, held = r.reservations().heldFor(testNamespace, "preemptor", r.now())
	assert.False(t, held, "a hold that cannot reclaim more lower-priority room is released")

	reconcileOnce(t, r, "high")

	require.Len(t, runtime.wakeCalls, 1)
	assert.Equal(t, "high", runtime.wakeCalls[0].ModelName)
	assert.Empty(t, getModel(t, r, "preemptor").Status.Instances)
}

func TestPrioritySleepRestartReplansWithoutDurableOwnership(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 280, 50, at0758},
		serving{"b", "warm-1", 280, 50, at0759},
		serving{"c", "warm-1", 280, 50, at0759})
	for _, name := range []string{"a", "b", "c"} {
		setClaimPriority(t, r, name, 1)
	}
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	reconcileOnce(t, r, "preemptor")
	require.Len(t, runtime.sleepCalls, 1)

	fresh := &ModelClaimReconciler{
		Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder, Runtime: runtime,
		PoolPolicy: r.PoolPolicy, Divisions: newCardDivisionState(r.now), Backoff: newPlacementBackoff(r.now), Now: r.Now,
	}
	reconcileOnce(t, fresh, "preemptor")

	assert.Len(t, runtime.sleepCalls, 2)
	assert.Empty(t, runtime.activateCalls)
	_, held := fresh.reservations().heldFor(testNamespace, "preemptor", fresh.now())
	assert.True(t, held)
}

func TestPreemptionSleepFailureReleasesReservationAndBacksOffOnce(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	runtime.sleepErr = &runtimeRefusal{"sleep failed: boom"}
	r.Backoff = newPlacementBackoff(r.now)

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, "preemptor"))
	require.Len(t, runtime.sleepCalls, 1)
	_, held := r.reservations().heldFor(testNamespace, "preemptor", r.now())
	assert.False(t, held)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "a").Status.Instances[0].Phase)
	key := client.ObjectKey{Namespace: testNamespace, Name: "preemptor"}
	attempt, waiting := r.Backoff.attempts[key]
	require.True(t, waiting)
	assert.Equal(t, 1, attempt.refusals)

	reconcileFor(t, r, "preemptor")
	assert.Len(t, runtime.sleepCalls, 1, "backoff prevents another victim in the next pass")
	assert.Equal(t, attempt, r.Backoff.attempts[key])
}

func TestPreemptionSleepFailureLetsLaterFeasibleClaimProceed(t *testing.T) {
	recent := time.Date(2026, time.October, 1, 7, 59, 50, 0, time.UTC)
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, recent}, serving{"b", "warm-1", 300, 100, recent})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "high", 10, at0758)
	low := withFinalizer(claimOnPod("low", "", modelv1alpha1.ModelClaimActive, 50, 50))
	low.Status.Instances = nil
	low.CreationTimestamp = metav1.NewTime(at0759)
	require.NoError(t, r.Create(context.Background(), low))
	runtime.sleepErr = &runtimeRefusal{"sleep failed: boom"}
	r.Backoff = newPlacementBackoff(r.now)
	reconcileOnce(t, r, "high")
	require.Len(t, runtime.sleepCalls, 1)

	reconcileOnce(t, r, "low")

	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, "low", runtime.activateCalls[0].ModelName)
	assert.Empty(t, getModel(t, r, "high").Status.Instances)
}

func TestInfeasiblePrioritySleepPredecessorDoesNotBlockNeverMakeRoom(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"resident-a", "warm-1", 300, 100, at0758}, serving{"resident-b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "resident-a", 20)
	setClaimPriority(t, r, "resident-b", 20)
	high := priorityTestClaim("high", 10, at0758)
	high.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionSleepLowerPriority
	low := priorityTestClaim("low", 0, at0759)
	low.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
	require.NoError(t, r.Create(context.Background(), withFinalizer(high)))
	require.NoError(t, r.Create(context.Background(), withFinalizer(low)))

	reconcileOnce(t, r, low.Name)

	require.Len(t, runtime.sleepCalls, 1, "the infeasible policy-specific predecessor must not keep the ordinary claimant's turn")
	assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, low.Name).Reason)
}

func TestPreemptionActivationFailureReleasesReservationForVictimWake(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	reconcileOnce(t, r, "preemptor")
	require.Len(t, runtime.sleepCalls, 1)
	runtime.failActivate = true

	reconcileOnce(t, r, "preemptor")

	_, held := r.reservations().heldFor(testNamespace, "preemptor", r.now())
	assert.False(t, held, "a definitive activation failure no longer owns reclaimed room")
	askWake(t, r, "warm-1", "a", "2026-10-01T08:00:06Z")
	reconcileOnce(t, r, "a")
	assert.Len(t, runtime.wakeCalls, 1, "normal wake admission resumes after activation failure")
}

func TestPrioritySleepMakingRoomDoesNotCreateRefusalBackoff(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "preemptor", 10, at0759)
	key := client.ObjectKey{Namespace: testNamespace, Name: "preemptor"}
	preemptor := getModel(t, r, "preemptor")
	r.Backoff.refused(key, preemptor.Generation, nil)
	attempt := r.Backoff.attempts[key]
	attempt.readyAt = r.now().Add(-time.Second)
	r.Backoff.attempts[key] = attempt

	reconcileOnce(t, r, "preemptor")

	assert.Len(t, runtime.sleepCalls, 1)
	_, waiting := r.Backoff.attempts[key]
	assert.False(t, waiting)
}

func TestPrioritySleepPreemptorKeepsPlacementTurn(t *testing.T) {
	recent := time.Date(2026, time.October, 1, 7, 59, 50, 0, time.UTC)
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, recent}, serving{"b", "warm-1", 300, 100, recent})
	setClaimPriority(t, r, "a", 1)
	setClaimPriority(t, r, "b", 1)
	newPrioritySleepClaim(t, r, "high", 10, at0758)
	low := withFinalizer(claimOnPod("low", "", modelv1alpha1.ModelClaimActive, 50, 50))
	low.Status.Instances = nil
	low.CreationTimestamp = metav1.NewTime(at0759)
	require.NoError(t, r.Create(context.Background(), low))

	reconcileOnce(t, r, "low")

	assert.Empty(t, runtime.activateCalls)
	assert.Equal(t, scheduledReasonWaitingForPlacementTurn, scheduledOf(t, r, "low").Reason)
	assert.Contains(t, scheduledOf(t, r, "low").Message, "high")
}

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

func TestReconcileMakesRoomForLowerPriorityClaimWhenHighPriorityClaimIsInfeasible(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	// "huge" wins priority order, but needs more than the whole card.
	high := withFinalizer(claimOnPod("huge", "", modelv1alpha1.ModelClaimActive, 1000, 100))
	high.Status.Instances = nil
	high.CreationTimestamp = metav1.NewTime(at0759)
	high.Spec.Priority = 10
	require.NoError(t, r.Create(context.Background(), high))
	lower := withFinalizer(claimOnPod("lower", "", modelv1alpha1.ModelClaimActive, 300, 100))
	lower.Status.Instances = nil
	lower.CreationTimestamp = metav1.NewTime(at0758)
	lower.Spec.Priority = 1
	require.NoError(t, r.Create(context.Background(), lower))

	reconcileOnce(t, r, lower.Name)

	require.Len(t, runtime.sleepCalls, 1, "the infeasible higher-priority claim does not block lower")
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
	assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, lower.Name).Reason)
}

func TestReconcileUnsuitableParallelismDoesNotStallPlacement(t *testing.T) {
	for _, priority := range []int32{0, 10} {
		t.Run(fmt.Sprintf("predecessor priority %d", priority), func(t *testing.T) {
			r, runtime := servingPool(t, keepNoWakeReserve, 1,
				serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
			pod := podNamed(t, r, "warm-1")
			pod.Spec.Containers = warmPodWithGPUs("warm-1", "b300-pool-a", 1).Spec.Containers
			require.NoError(t, r.Update(context.Background(), pod))
			predecessor := priorityTestClaim("tp-two", priority, at0758)
			predecessor.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
				"--tensor-parallel-size": "2",
			}}
			require.NoError(t, r.Create(context.Background(), withFinalizer(predecessor)))
			newClaim(t, r, "needs-room", at0759)
			small := priorityTestClaim("small", 0, at0759.Add(time.Second))
			small.Spec.PerGPU = claimWithCost(50, 50).Spec.PerGPU
			require.NoError(t, r.Create(context.Background(), withFinalizer(small)))

			// The later small claim fits directly but preserves the earlier
			// feasible claim's turn, even though that claim needs a sleep first.
			reconcileOnce(t, r, "small")
			assert.Equal(t, scheduledReasonWaitingForPlacementTurn, scheduledOf(t, r, "small").Reason)
			assert.Contains(t, scheduledOf(t, r, "small").Message, "needs-room")
			assert.Empty(t, runtime.activateCalls)
			assert.Empty(t, runtime.sleepCalls)

			reconcileOnce(t, r, "needs-room")
			require.Len(t, runtime.sleepCalls, 1, "the TP=2 predecessor cannot use this one-GPU pod")
			assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
			assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, "needs-room").Reason)
			card, held := r.reservations().heldFor(testNamespace, "needs-room", r.now())
			require.True(t, held)
			assert.Equal(t, "warm-1", card.Name)

			reconcileOnce(t, r, "needs-room")
			require.Len(t, getModel(t, r, "needs-room").Status.Instances, 1)
			assert.Equal(t, "warm-1", getModel(t, r, "needs-room").Status.Instances[0].Pod)
			reconcileOnce(t, r, "small")
			require.Len(t, getModel(t, r, "small").Status.Instances, 1)
			assert.Equal(t, "warm-1", getModel(t, r, "small").Status.Instances[0].Pod)
			assert.Empty(t, getModel(t, r, "tp-two").Status.Instances)
			assert.Len(t, runtime.sleepCalls, 1)
		})
	}
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

func TestReconcilePrioritySleepFailureBackoffLetsLaterClaimProceed(t *testing.T) {
	for _, priority := range []int32{0, 100} {
		t.Run(fmt.Sprintf("predecessor priority %d", priority), func(t *testing.T) {
			r, runtime := servingPool(t, keepNoWakeReserve, 1,
				serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
			high := priorityTestClaim("needs-sleep", priority, at0758)
			high.Spec.ModelName = nil
			low := priorityTestClaim("small", 0, at0759)
			low.Spec.ModelName = nil
			low.Spec.PerGPU = claimWithCost(50, 50).Spec.PerGPU
			require.NoError(t, r.Create(context.Background(), withFinalizer(high)))
			require.NoError(t, r.Create(context.Background(), withFinalizer(low)))
			runtime.sleepErr = &runtimeRefusal{"sleep failed: boom"}
			r.Backoff = newPlacementBackoff(func() time.Time { return at0759 })

			assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, high.Name))
			require.Len(t, runtime.sleepCalls, 1)
			assert.Empty(t, runtime.activateCalls)
			assert.Equal(t, "NoMatchingPods", scheduledOf(t, r, high.Name).Reason)
			_, held := r.reservations().heldFor(testNamespace, high.Name, r.now())
			assert.False(t, held)
			key := client.ObjectKeyFromObject(high)
			before, waiting := r.Backoff.attempts[key]
			require.True(t, waiting)
			assert.True(t, before.written)
			assert.Equal(t, at0759.Add(DefaultRequeueDuration), before.readyAt)

			reconcileFor(t, r, low.Name)
			require.Len(t, getModel(t, r, low.Name).Status.Instances, 1, "the small claim fits without putting a neighbour to sleep")
			assert.Equal(t, "warm-1", getModel(t, r, low.Name).Status.Instances[0].Pod)
			assert.Empty(t, getModel(t, r, high.Name).Status.Instances)
			assert.Equal(t, before, r.Backoff.attempts[key])
			assert.Len(t, runtime.sleepCalls, 1)
		})
	}
}

func TestReconcilePrioritySleepFailureBackoffLetsLaterClaimMakeRoom(t *testing.T) {
	for _, priority := range []int32{0, 100} {
		t.Run(fmt.Sprintf("predecessor priority %d", priority), func(t *testing.T) {
			r, runtime := servingPool(t, keepNoWakeReserve, 1,
				serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
			high := priorityTestClaim("first", priority, at0758)
			low := priorityTestClaim("later", 0, at0759)
			require.NoError(t, r.Create(context.Background(), withFinalizer(high)))
			require.NoError(t, r.Create(context.Background(), withFinalizer(low)))
			runtime.sleepErr = &runtimeRefusal{"sleep failed: boom"}
			r.Backoff = newPlacementBackoff(func() time.Time { return at0759 })
			assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, high.Name))
			assert.Empty(t, runtime.activateCalls)
			assert.Equal(t, "NoMatchingPods", scheduledOf(t, r, high.Name).Reason)
			key := client.ObjectKeyFromObject(high)
			before, waiting := r.Backoff.attempts[key]
			require.True(t, waiting)
			require.True(t, before.written)
			_, held := r.reservations().heldFor(testNamespace, high.Name, r.now())
			require.False(t, held)

			// The sleep endpoint recovers without a generation or room change.
			// The first claim still waits, while the next may try to make room.
			runtime.sleepErr = nil
			reconcileFor(t, r, low.Name)
			require.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, low.Name).Reason)
			card, held := r.reservations().heldFor(testNamespace, low.Name, r.now())
			require.True(t, held)
			assert.Equal(t, "warm-1", card.Name)
			assert.Equal(t, before, r.Backoff.attempts[key])
			reconcileFor(t, r, low.Name)
			require.Len(t, getModel(t, r, low.Name).Status.Instances, 1)
			assert.Equal(t, "warm-1", getModel(t, r, low.Name).Status.Instances[0].Pod)
			assert.Empty(t, getModel(t, r, high.Name).Status.Instances)
		})
	}
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
			condition := scheduledOf(t, r, "x")
			if tc.older {
				assert.Equal(t, "WaitingForPlacementTurn", condition.Reason)
				assert.Equal(t, "waiting behind ModelClaim older with priority 0 that can use the same pool", condition.Message)
			} else {
				assert.Equal(t, "NoMatchingPods", condition.Reason)
			}
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

func TestReconcileFailedClaimMakesRoomOnlyWhenBackoffDue(t *testing.T) {
	for _, tt := range []struct {
		name    string
		waiting bool
		due     bool
		makes   bool
	}{
		{name: "active backoff", waiting: true},
		{name: "due retry", waiting: true, due: true, makes: true},
		{name: "controller restart", makes: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, runtime := servingPool(t, keepNoWakeReserve, 1,
				serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
			newClaim(t, r, "x", at0759)
			claim := getModel(t, r, "x")
			claim.Status.Phase = modelv1alpha1.ModelClaimFailed
			require.NoError(t, r.Status().Update(context.Background(), claim))
			now := at0759
			r.Backoff = newPlacementBackoff(func() time.Time { return now })
			if tt.waiting {
				candidates, err := r.listCandidateWarmPods(context.Background(), claim)
				require.NoError(t, err)
				claims, err := r.listClaimsForAccount(context.Background(), testNamespace)
				require.NoError(t, err)
				r.Backoff.failedToStart(client.ObjectKeyFromObject(claim), claim.Generation,
					roomSignatureOf(candidates, claims))
			}
			if tt.due {
				now = now.Add(DefaultRequeueDuration)
			}

			reconcileOnce(t, r, claim.Name)

			if tt.makes {
				require.Len(t, runtime.sleepCalls, 1)
				assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
				assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, claim.Name).Reason)
			} else {
				assert.Empty(t, runtime.sleepCalls)
			}
		})
	}
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
