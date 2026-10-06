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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const keepNoWakeReserve = `{"lifecycle":{"noWakeReserveWhileAsleep":true}}`

// sleeperOnACard is the account of a 1000-byte card in a pool with the given
// policy. A claim that declared 300+100 sleeps on it, and its runtime measured
// it to hold footprint bytes asleep, with 20 bytes of KV mapped. A nil
// footprint is one the runtime could not measure.
func sleeperOnACard(t *testing.T, policy string, footprint *int64, wakeAsked bool) podLedger {
	t.Helper()
	deployment, replicaSet, pod := warmPoolObjects(policy)
	if wakeAsked {
		pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + "sleeper": "2026-10-01T08:00:00Z"}
	}
	sleeper := claimOnPod("sleeper", pod.Name, modelv1alpha1.ModelClaimSleeping, 300, 100)
	engine := engineHolding("sleeper", 20, 100)
	engine.Phase = runtimePhaseSleeping
	engine.SleepingFootprintBytes = footprint
	r, _ := newReconciler(t, deployment, replicaSet, pod, sleeper)
	ledger, found := r.collectPodLedgers(context.Background(), testNamespace, []corev1.Pod{*pod},
		sizedPodSnapshots(pod.Name, 1000, engine), "")[pod.Name]
	require.True(t, found)
	require.True(t, ledger.judgeable, ledger.blocked)
	return ledger
}

func bytesOf(n int64) *int64 { return &n }

func TestLedgerChargesASleeperWithoutAWakeReserveWhatItStillHolds(t *testing.T) {
	ledger := sleeperOnACard(t, keepNoWakeReserve, bytesOf(60), false)

	assert.Equal(t, int64(940), ledger.maximumRoomBytes(), "charged only the 60 bytes it holds asleep")
	assert.Equal(t, int64(940), ledger.heldRoomBytes(), "its mapped KV is part of what it holds")
}

func TestLedgerKeepsAWakeReserveWhenItIsNeeded(t *testing.T) {
	for name, tc := range map[string]struct {
		policy    string
		footprint *int64
		wakeAsked bool
	}{
		"a pool that keeps the reserve":   {`{"lifecycle":{"sleepAfterSeconds":60}}`, bytesOf(60), false},
		"a pool without a policy":         {"", bytesOf(60), false},
		"a memory asleep nobody measured": {keepNoWakeReserve, nil, false},
		"a request that asks to wake it":  {keepNoWakeReserve, bytesOf(60), true},
		"a policy that is not valid":      {`{"lifecycle":{"noWakeReserveWhileAsleep":true,"sleepAfterSeconds":-1}}`, bytesOf(60), false},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := sleeperOnACard(t, tc.policy, tc.footprint, tc.wakeAsked)

			assert.Equal(t, int64(600), ledger.maximumRoomBytes(), "charged its footprint and floor")
			assert.Equal(t, int64(600), ledger.heldRoomBytes())
		})
	}
}

func TestLedgerChargesASleeperOnAPodWithMoreThanOneCardWhatItHolds(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects(keepNoWakeReserve)
	sleeper := claimOnPod("sleeper", pod.Name, modelv1alpha1.ModelClaimSleeping, 300, 100)
	engine := engineHolding("sleeper", 20, 100)
	engine.Phase = runtimePhaseSleeping
	engine.SleepingFootprintBytes = bytesOf(60)
	r, _ := newReconciler(t, deployment, replicaSet, pod, sleeper)
	snapshots := sizedPodSnapshots(pod.Name, 1000, engine)
	snapshots[pod.Name].Accelerators = []RuntimeAcceleratorSnapshot{
		{ID: "GPU-0", HBMTotalBytes: 1100, HBMUsableBytes: 1000},
		{ID: "GPU-1", HBMTotalBytes: 1100, HBMUsableBytes: 1000},
	}

	ledger, found := r.collectPodLedgers(context.Background(), testNamespace, []corev1.Pod{*pod}, snapshots, "")[pod.Name]

	require.True(t, found)
	require.True(t, ledger.judgeable, ledger.blocked)
	assert.Equal(t, int64(940), ledger.maximumRoomBytes(), "charged the 60 bytes its runtime measured on its heaviest card")
}

func TestLedgerChargesASleeperThatHoldsMoreThanItsSeatWhatItHolds(t *testing.T) {
	ledger := sleeperOnACard(t, keepNoWakeReserve, bytesOf(450), false)

	assert.Equal(t, int64(550), ledger.maximumRoomBytes(), "it really holds 450 bytes, more than the 400 it declared")
}

func TestPlanKVLimitsKeepsASleeperWithoutAWakeReserveAtWhatItMaps(t *testing.T) {
	awake := engineOnPod{
		claimName: "awake", modelName: "awake", perGPUBytes: perGPUBytes{maximumFootprintBytes: 300, kvFloorBytes: 100},
		kvUsedBytes: 50, kvCapacityBytes: 100,
	}
	sleeper := engineOnPod{
		claimName: "sleeper", modelName: "sleeper", perGPUBytes: perGPUBytes{maximumFootprintBytes: 300, kvFloorBytes: 100},
		kvUsedBytes: 20, kvCapacityBytes: 100, asleep: true,
		sleepingFootprintBytes: 60, withoutWakeReserve: true,
	}

	limits, err := planKVLimits(1000, []engineOnPod{awake, sleeper})

	require.NoError(t, err)
	require.Len(t, limits, 2)
	assert.Equal(t, int64(20), limits[1].kvLimitBytes, "the sleeper keeps only the KV it maps")
	assert.Equal(t, int64(1000-300-60), limits[0].kvLimitBytes,
		"the engine awake gets every byte the sleeper does not hold")
}

// lentRoomOnACard is a 1000-byte card in a pool that keeps no wake reserve. The
// claim "lender" serves on it and was given the room "waker" left when it went
// to sleep: it is held to 600 bytes of KV and maps 150. The claim "waker"
// sleeps, held to the 20 bytes it maps, and a request asks to wake it at 08:00.
// The controller's clock reads 08:00:05, so the request is still young on
// whatever day the test runs.
func lentRoomOnACard(t *testing.T) (*ModelClaimReconciler, *fakeRuntime) {
	t.Helper()
	deployment, replicaSet, pod := warmPoolObjects(keepNoWakeReserve)
	pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + "waker": "2026-10-01T08:00:00Z"}
	lender := withFinalizer(claimOnPod("lender", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))
	lender.Status.Instances[0].Port = 9001
	lender.Status.Instances[0].KVLimitBytes = 600
	waker := withFinalizer(claimOnPod("waker", pod.Name, modelv1alpha1.ModelClaimSleeping, 300, 100))
	waker.Status.Instances[0].Port = 9002
	waker.Status.Instances[0].KVLimitBytes = 20
	lending := engineHolding("lender", 150, 600)
	asleep := engineHolding("waker", 20, 20)
	asleep.Port = 9002
	asleep.Phase = runtimePhaseSleeping
	asleep.Ready = false
	asleep.SleepingFootprintBytes = bytesOf(60)
	r, runtime := newReconciler(t, deployment, replicaSet, pod, lender, waker)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: sizedPodSnapshots(pod.Name, 1000, lending, asleep)[pod.Name]}
	now := time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC)
	r.Now = func() time.Time { return now }
	return r, runtime
}

func TestReconcileTakesLentRoomBackBeforeAWake(t *testing.T) {
	r, runtime := lentRoomOnACard(t)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "waker"},
	})

	require.NoError(t, err)
	assert.False(t, result.Requeue, "the pass writes its status over the record the division made, without a conflict")
	require.Len(t, runtime.wakeCalls, 1)
	require.GreaterOrEqual(t, len(runtime.kvLimitCalls), 2)
	assert.Equal(t, "lender", runtime.kvLimitCalls[0].ModelName, "the neighbour gives the room back first")
	assert.Equal(t, int64(300), runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, "waker", runtime.kvLimitCalls[1].ModelName, "then the engine is raised to its floor")
	assert.Equal(t, int64(100), runtime.kvLimitCalls[1].LimitBytes)
	assert.Equal(t, int64(300), getModel(t, r, "lender").Status.Instances[0].KVLimitBytes)
	assert.Equal(t, int64(100), getModel(t, r, "waker").Status.Instances[0].KVLimitBytes,
		"the record the division wrote survives the pass")
}

// getHook is a reader whose Get is the given function.
type getHook struct {
	client.Reader
	get func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

func (g *getHook) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return g.get(ctx, key, obj, opts...)
}

func TestReconcileWakesOnTheNextPassWhenTheClaimCannotBeReadBack(t *testing.T) {
	r, runtime := lentRoomOnACard(t)
	// The claim cannot be read back once, after the division recorded its new
	// limit on it.
	failed := false
	r.APIReader = &getHook{Reader: r.Client, get: func(ctx context.Context, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if err := r.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		claim, ok := obj.(*modelv1alpha1.ModelClaim)
		if ok && !failed && claim.Name == "waker" && claim.Status.Instances[0].KVLimitBytes != 20 {
			failed = true
			return fmt.Errorf("the API server did not answer")
		}
		return nil
	}}
	waker := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "waker"}}

	_, err := r.Reconcile(context.Background(), waker)

	require.NoError(t, err)
	require.True(t, failed, "the division recorded the new limit")
	assert.NotEmpty(t, runtime.kvLimitCalls, "the card was divided")
	assert.Empty(t, runtime.wakeCalls, "the engine sleeps until the next pass")

	_, err = r.Reconcile(context.Background(), waker)

	require.NoError(t, err)
	assert.Len(t, runtime.wakeCalls, 1)
}

func TestCardArrangedForWakeWaitsForACardThatCannotBePlanned(t *testing.T) {
	pod, _ := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pod)
	ledger := podLedger{judgeable: true, hbmUsableBytes: 1000, accelerators: 1, engines: []engineOnPod{{
		claimName: "undeclared", modelName: "undeclared", kvCapacityBytes: 100,
	}}}

	arranged := r.cardArrangedForWake(context.Background(), claimWithCost(300, 100), pod, ledger,
		newRuntimeReadings(runtime))

	assert.False(t, arranged)
	assert.Empty(t, runtime.kvLimitCalls)
}

func TestReconcileLeavesAnEngineAsleepWhileItsCardCannotBeDivided(t *testing.T) {
	r, runtime := lentRoomOnACard(t)
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, "waker")

	assert.Empty(t, runtime.wakeCalls, "the neighbour may still grow into the room")
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations, constants.ModelClaimWakeAnnotationPrefix+"waker", "the request is tried again")
}

func TestHeldAsPlannedForWakeLooksAtNeighboursAndTheWakingEngine(t *testing.T) {
	limits := []plannedKVLimit{{claimName: "a", kvLimitBytes: 300}, {claimName: "w", kvLimitBytes: 100}}
	engine := func(name string, capacity int64) engineOnPod {
		return engineOnPod{claimName: name, kvCapacityBytes: capacity}
	}

	assert.True(t, heldAsPlannedForWake([]engineOnPod{engine("a", 300), engine("w", 100)}, limits, "w"))
	assert.True(t, heldAsPlannedForWake([]engineOnPod{engine("a", 200), engine("w", 100)}, limits, "w"),
		"a neighbour held to less is no danger")
	assert.False(t, heldAsPlannedForWake([]engineOnPod{engine("a", 600), engine("w", 100)}, limits, "w"),
		"a neighbour held to more could grow into the room")
	assert.False(t, heldAsPlannedForWake([]engineOnPod{engine("a", 300), engine("w", 20)}, limits, "w"),
		"the waking engine needs its floor")
	assert.True(t, heldAsPlannedForWake([]engineOnPod{engine("a", 300), engine("w", -1)}, limits, "w"),
		"an engine without a segment holds nothing")
}

// idleEngineInAPool is an engine serving a claim alone in a pool with the
// given policy. The clock the pool policy reads is the one returned.
func idleEngineInAPool(t *testing.T, policy string) (*ModelClaimReconciler, *fakeRuntime, *corev1.Pod, *time.Time) {
	t.Helper()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	deployment, replicaSet, pod := warmPoolObjects(policy)
	pod.UID = types.UID("warm-uid")
	claim := sampleModelClaim()
	claim.UID = types.UID("claim-uid")
	claim.Status.Phase = modelv1alpha1.ModelClaimActive
	claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: pod.Name, Port: 20000, Phase: modelv1alpha1.ModelClaimActive,
	}}
	r, runtime := newReconciler(t, deployment, replicaSet, pod, claim)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "qwen2-7b", Port: 20000,
				Phase: runtimePhaseActive, Alive: true, Ready: true,
				RequestMetricsObserved: true, RequestSuccessTotal: &requestSuccessTotal,
				ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
			}},
		},
	}
	return r, runtime, pod, &now
}

// sleepsHolding has a runtime report an engine asleep after a sleep, holding
// footprint bytes, or with what it holds unknown when footprint is nil.
func sleepsHolding(runtime *fakeRuntime, pod *corev1.Pod, footprint *int64) {
	runtime.onSleep = func(*SleepRequest) {
		engine := &runtime.snapshots[pod.Status.PodIP].Models[0]
		engine.Phase = runtimePhaseSleeping
		engine.Ready = false
		engine.SleepingFootprintBytes = footprint
	}
}

func TestReconcilePoolPoliciesSaysWhatAnIdleEngineHoldsAsleep(t *testing.T) {
	r, runtime, pod, now := idleEngineInAPool(t, `{"lifecycle":{"sleepAfterSeconds":60,"noWakeReserveWhileAsleep":true}}`)
	sleepsHolding(runtime, pod, bytesOf(2<<30))

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))
	*now = now.Add(61 * time.Second)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))

	require.Len(t, runtime.sleepCalls, 1)
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "it holds 2.0 GiB asleep")
	assert.NotContains(t, events, "SleepingFootprintUnknown")
}

func TestReconcilePoolPoliciesWarnsOfASleepThatFreesNothing(t *testing.T) {
	for policy, warned := range map[string]bool{
		`{"lifecycle":{"sleepAfterSeconds":60,"noWakeReserveWhileAsleep":true}}`: true,
		`{"lifecycle":{"sleepAfterSeconds":60}}`:                                 false,
	} {
		t.Run(policy, func(t *testing.T) {
			r, runtime, pod, now := idleEngineInAPool(t, policy)
			sleepsHolding(runtime, pod, nil)

			r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))
			*now = now.Add(61 * time.Second)
			r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))

			events := strings.Join(drainEvents(t, r), "\n")
			assert.Contains(t, events, "what it holds asleep could not be measured")
			if warned {
				assert.Contains(t, events, "Warning SleepingFootprintUnknown",
					"the pool keeps no wake reserve, and this engine keeps its own all the same")
			} else {
				assert.NotContains(t, events, "SleepingFootprintUnknown", "every engine keeps its reserve here")
			}
		})
	}
}

func TestReconcileSaysWhatAnEngineFoundAsleepHolds(t *testing.T) {
	for name, footprint := range map[string]*int64{"measured": bytesOf(60), "unmeasured": nil} {
		t.Run(name, func(t *testing.T) {
			deployment, replicaSet, pod := warmPoolObjects(keepNoWakeReserve)
			pm := withFinalizer(claimOnPod("dozing", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))
			pm.Status.Instances[0].Port = 9001
			asleep := engineHolding("dozing", 20, 100)
			asleep.Phase = runtimePhaseSleeping
			asleep.Ready = false
			asleep.SleepingFootprintBytes = footprint
			r, runtime := newReconciler(t, deployment, replicaSet, pod, pm)
			runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: sizedPodSnapshots(pod.Name, 1000, asleep)[pod.Name]}

			reconcileOnce(t, r, "dozing")

			events := strings.Join(drainEvents(t, r), "\n")
			assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, "dozing").Status.Instances[0].Phase)
			if footprint != nil {
				assert.Contains(t, events, "is sleeping on pod warm-1 and marked non-routable; it holds 60 bytes asleep")
				assert.NotContains(t, events, "SleepingFootprintUnknown")
			} else {
				assert.Contains(t, events, "Warning SleepingFootprintUnknown")
			}
		})
	}
}

// crowdedCard is a 1000-byte card in a pool with the given policy. The claims
// "a" and "b" serve on it, idle, and "waker" sleeps on it, held to the 20
// bytes it maps. Each declared 300+100, so the three floors do not fit. A
// request asks to wake "waker" at 08:00. The pool policy saw "a" busy last at
// 07:58 and "b" at 07:59, and its clock reads 08:00:05.
func crowdedCard(t *testing.T, policy string) (*ModelClaimReconciler, *fakeRuntime, *corev1.Pod) {
	t.Helper()
	deployment, replicaSet, pod := warmPoolObjects(policy)
	pod.UID = types.UID("warm-uid")
	pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + "waker": "2026-10-01T08:00:00Z"}
	objects := []client.Object{deployment, replicaSet, pod}
	var engines []RuntimeSnapshotModel
	for i, name := range []string{"a", "b", "waker"} {
		claim := withFinalizer(claimOnPod(name, pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))
		claim.UID = types.UID(name + "-uid")
		claim.Status.Instances[0].Port = int32(9001 + i)
		claim.Status.Instances[0].KVLimitBytes = 100
		engine := engineHolding(name, 50, 100)
		engine.Port = int32(9001 + i)
		engine.ClaimRef = &ModelClaimRef{Namespace: testNamespace, Name: name, UID: string(claim.UID)}
		total := int64(10)
		engine.RequestSuccessTotal = &total
		if name == "waker" {
			claim.Status.Instances[0].Phase = modelv1alpha1.ModelClaimSleeping
			claim.Status.Instances[0].KVLimitBytes = 20
			engine.Phase = runtimePhaseSleeping
			engine.Ready = false
			engine.KVUsedBytes, engine.KVCapacityBytes = 20, 20
			engine.SleepingFootprintBytes = bytesOf(60)
		}
		objects = append(objects, claim)
		engines = append(engines, engine)
	}
	r, runtime := newReconciler(t, objects...)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: sizedPodSnapshots(pod.Name, 1000, engines...)[pod.Name]}
	// The pool policy sees each engine first as the idle timer does, through a
	// reading of the whole pod.
	now := time.Date(2026, time.October, 1, 7, 58, 0, 0, time.UTC)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	_, observed := r.PoolPolicy.observeSnapshot(pod, &RuntimeSnapshot{Models: engines[:1]})
	require.True(t, observed)
	now = now.Add(time.Minute)
	_, observed = r.PoolPolicy.observeSnapshot(pod, &RuntimeSnapshot{Models: engines[1:2]})
	require.True(t, observed)
	now = time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC)
	r.Now = func() time.Time { return now }
	// A sleep leaves the engine holding 60 bytes, as its runtime measures.
	runtime.onSleep = func(req *SleepRequest) {
		models := runtime.snapshots[pod.Status.PodIP].Models
		for i := range models {
			if models[i].ModelName == req.ModelName {
				models[i].Phase = runtimePhaseSleeping
				models[i].Ready = false
				models[i].SleepingFootprintBytes = bytesOf(60)
			}
		}
	}
	return r, runtime, pod
}

func TestReconcilePutsTheNeighbourIdleLongestToSleepToMakeRoomForAWake(t *testing.T) {
	r, runtime, _ := crowdedCard(t, keepNoWakeReserve)

	reconcileOnce(t, r, "waker")

	require.Len(t, runtime.sleepCalls, 1, "one neighbour a pass")
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName, "the one idle longest")
	assert.Empty(t, runtime.wakeCalls, "the next pass sees what it holds asleep")
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, "a").Status.Instances[0].Phase)
	assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, "waker").Status.Instances[0].Reason)
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "SleptToMakeRoom model a idle for 2m5s; put to sleep on pod warm-1 to make room for model waker")

	reconcileOnce(t, r, "waker")

	assert.Len(t, runtime.sleepCalls, 1, "a sleeping 60 bytes leaves room for the waker")
	require.Len(t, runtime.wakeCalls, 1)
	assert.Equal(t, "waker", runtime.wakeCalls[0].ModelName)
}

func TestReconcilePutsNoNeighbourToSleepForAWakeOnACardHeldForAnotherClaim(t *testing.T) {
	r, runtime, pod := crowdedCard(t, keepNoWakeReserve)
	// Room is being made on this card for a new claim, and the card is held
	// for it.
	require.True(t, r.reservations().hold(cardOf(pod), "new", 400, r.now()))

	reconcileOnce(t, r, "waker")

	assert.Empty(t, runtime.sleepCalls, "the room is the new claim's")
	assert.Empty(t, runtime.wakeCalls)
	assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, "waker").Status.Instances[0].Reason)
}

func TestReconcilePutsNoNeighbourToSleepWhereASleepGivesNoRoomBack(t *testing.T) {
	for name, tc := range map[string]struct {
		policy string
		setUp  func(*ModelClaimReconciler, *fakeRuntime, *corev1.Pod)
	}{
		"a pool that keeps the wake reserve": {policy: `{"lifecycle":{"sleepAfterSeconds":600}}`},
		"a sleeper whose memory asleep is not known": {policy: keepNoWakeReserve,
			setUp: func(_ *ModelClaimReconciler, runtime *fakeRuntime, pod *corev1.Pod) {
				b := &runtime.snapshots[pod.Status.PodIP].Models[1]
				b.Phase = runtimePhaseSleeping
				b.Ready = false
			}},
		"neighbours that have not idled long enough": {
			policy: `{"lifecycle":{"noWakeReserveWhileAsleep":true,"sleepToMakeRoomAfterSeconds":600}}`},
		"an older request on the card": {policy: keepNoWakeReserve,
			setUp: func(r *ModelClaimReconciler, _ *fakeRuntime, pod *corev1.Pod) {
				latest := &corev1.Pod{}
				require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pod), latest))
				latest.Annotations[constants.ModelClaimWakeAnnotationPrefix+"b"] = "2026-10-01T07:59:00Z"
				require.NoError(t, r.Update(context.Background(), latest))
			}},
	} {
		t.Run(name, func(t *testing.T) {
			r, runtime, pod := crowdedCard(t, tc.policy)
			if tc.setUp != nil {
				tc.setUp(r, runtime, pod)
			}

			reconcileOnce(t, r, "waker")

			assert.Empty(t, runtime.sleepCalls)
			assert.Empty(t, runtime.wakeCalls)
			assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, "waker").Status.Instances[0].Reason)
		})
	}
}

func TestReconcileHoldsAWakeOnACardThatCannotBeAccountedForWhereNoReserveWasKept(t *testing.T) {
	r, runtime := lentRoomOnACard(t)
	// A claim on the card that declares nothing opens a hole in its account.
	undeclared := withFinalizer(claimOnPod("undeclared", "warm-1", modelv1alpha1.ModelClaimActive, 300, 100))
	undeclared.Spec.PerGPU = nil
	require.NoError(t, r.Create(context.Background(), undeclared))

	reconcileOnce(t, r, "waker")

	assert.Empty(t, runtime.wakeCalls, "the room it left may be someone else's now")
	assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, "waker").Status.Instances[0].Reason)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "its card cannot be accounted for")
}

func TestDivisionPlansAWakingEngineAtWhatItHoldsWhileItWaitsForRoom(t *testing.T) {
	r, runtime, pod := crowdedCard(t, keepNoWakeReserve)
	// The card was divided before the request came.
	r.divisions().divided(cardOf(pod), "a/awake/300+100,b/awake/300+100,waker/asleep/300+100")

	r.divideCardsAsListed(context.Background(), []corev1.Pod{*pod}, newRuntimeReadings(r.Runtime))

	assert.NotEmpty(t, runtime.kvLimitCalls, "the card is divided, though the waker's reserve does not fit")
	limits := map[string]int64{}
	for _, call := range runtime.kvLimitCalls {
		limits[call.ModelName] = call.LimitBytes
	}
	assert.Equal(t, int64(170), limits["a"], "a and b share what the waker does not hold asleep")
	assert.Equal(t, int64(170), limits["b"])
	assert.NotContains(t, limits, "waker", "the waker stays at the 20 bytes it maps")
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "KVLimitFailed")
}

// lendingNeighbour is a 1000-byte card in a pool that keeps no wake reserve,
// where the claim "lender" serves, held to 600 bytes of KV and mapping kvUsed
// of them, with inFlight requests. The claim "waker" sleeps on it, and a
// request asks to wake it at 08:00. Both declared 300+100, so the floors fit
// together, but the lender holds the room the waker left. The pool policy last
// saw the lender busy at 07:59, and the clocks read 08:00:05. The clock the
// pool policy reads is returned.
func lendingNeighbour(t *testing.T, kvUsed, inFlight int64) (*ModelClaimReconciler, *fakeRuntime, *corev1.Pod, *time.Time) {
	t.Helper()
	deployment, replicaSet, pod := warmPoolObjects(keepNoWakeReserve)
	pod.UID = types.UID("warm-uid")
	pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + "waker": "2026-10-01T08:00:00Z"}
	lender := withFinalizer(claimOnPod("lender", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))
	lender.UID = types.UID("lender-uid")
	lender.Status.Instances[0].Port = 9001
	lender.Status.Instances[0].KVLimitBytes = 600
	waker := withFinalizer(claimOnPod("waker", pod.Name, modelv1alpha1.ModelClaimSleeping, 300, 100))
	waker.UID = types.UID("waker-uid")
	waker.Status.Instances[0].Port = 9002
	waker.Status.Instances[0].KVLimitBytes = 20
	lending := engineHolding("lender", kvUsed, 600)
	lending.RequestsRunning = inFlight
	completed := int64(10)
	lending.RequestSuccessTotal = &completed
	lending.ClaimRef = &ModelClaimRef{Namespace: testNamespace, Name: "lender", UID: "lender-uid"}
	asleep := engineHolding("waker", 20, 20)
	asleep.Port = 9002
	asleep.Phase = runtimePhaseSleeping
	asleep.Ready = false
	asleep.SleepingFootprintBytes = bytesOf(60)
	asleep.ClaimRef = &ModelClaimRef{Namespace: testNamespace, Name: "waker", UID: "waker-uid"}
	r, runtime := newReconciler(t, deployment, replicaSet, pod, lender, waker)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: sizedPodSnapshots(pod.Name, 1000, lending, asleep)[pod.Name]}
	now := time.Date(2026, time.October, 1, 7, 59, 0, 0, time.UTC)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	busy := lending
	busy.RequestsRunning = 1
	_, observed := r.PoolPolicy.observeSnapshot(pod, &RuntimeSnapshot{Models: []RuntimeSnapshotModel{busy}})
	require.True(t, observed)
	now = time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC)
	r.Now = func() time.Time { return now }
	runtime.onSleep = func(req *SleepRequest) {
		models := runtime.snapshots[pod.Status.PodIP].Models
		for i := range models {
			if models[i].ModelName == req.ModelName {
				models[i].Phase = runtimePhaseSleeping
				models[i].Ready = false
				models[i].SleepingFootprintBytes = bytesOf(60)
			}
		}
	}
	return r, runtime, pod, &now
}

func TestReconcilePutsAnIdleNeighbourHoldingLentKVToSleep(t *testing.T) {
	r, runtime, _, _ := lendingNeighbour(t, 350, 0)

	reconcileOnce(t, r, "waker")

	require.Len(t, runtime.sleepCalls, 1, "an idle engine never gives its KV back, and a sleep does")
	assert.Equal(t, "lender", runtime.sleepCalls[0].ModelName)
	for _, call := range runtime.kvLimitCalls {
		assert.False(t, call.ModelName == "lender" && call.LimitBytes < 350, "no limit below what the lender maps")
	}
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "SleptToMakeRoom")

	reconcileOnce(t, r, "waker")

	require.Len(t, runtime.wakeCalls, 1)
}

func TestReconcileLeavesABusyNeighbourHoldingLentKVAloneUntilItIdles(t *testing.T) {
	r, runtime, pod, now := lendingNeighbour(t, 350, 2)

	reconcileOnce(t, r, "waker")

	assert.Empty(t, runtime.sleepCalls, "a neighbour that serves is not put to sleep")
	assert.Empty(t, runtime.wakeCalls)
	for _, call := range runtime.kvLimitCalls {
		assert.False(t, call.ModelName == "lender" && call.LimitBytes < 350,
			"a smaller limit would only hold the busy lender back")
	}
	assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, "waker").Status.Instances[0].Reason)

	// The lender's last requests finish, and it has served nothing for half a
	// minute when the claim is looked at again.
	busy := runtime.snapshots[pod.Status.PodIP].Models[0]
	_, observed := r.PoolPolicy.observeSnapshot(pod, &RuntimeSnapshot{Models: []RuntimeSnapshotModel{busy}})
	require.True(t, observed)
	runtime.snapshots[pod.Status.PodIP].Models[0].RequestsRunning = 0
	*now = now.Add(31 * time.Second)
	reconcileOnce(t, r, "waker")

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "lender", runtime.sleepCalls[0].ModelName)

	reconcileOnce(t, r, "waker")

	require.Len(t, runtime.wakeCalls, 1)
	assert.Equal(t, "waker", runtime.wakeCalls[0].ModelName)
}
