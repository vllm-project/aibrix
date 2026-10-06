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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func podNamed(t *testing.T, r *ModelClaimReconciler, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, pod))
	return pod
}

// askWake writes a wake request on a pod, as the gateway does.
func askWake(t *testing.T, r *ModelClaimReconciler, podName, claim, at string) {
	t.Helper()
	pod := podNamed(t, r, podName)
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claim] = at
	require.NoError(t, r.Patch(context.Background(), pod, patch))
}

// sleepingClaim places a claim on warm-1 and has its engine go to sleep.
func sleepingClaim(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, int32) {
	t.Helper()
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t, pm, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))
	reconcileOnce(t, r, pm.Name)
	port := getModel(t, r, pm.Name).Status.Instances[0].Port
	served := servedModelName(pm)
	runtime.models[served] = ModelInfo{ModelName: served, Port: port, Phase: "sleeping"}
	reconcileOnce(t, r, pm.Name)
	require.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	drainEvents(t, r)
	// The tests ask for a wake at 08:00, so the clock is held a moment after.
	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC) }
	return r, runtime, pm, port
}

func TestReconcileWakesASleepingEngineWhenAskedAndTakesTheRequestBackOnceItServes(t *testing.T) {
	r, runtime, pm, port := sleepingClaim(t)
	served := servedModelName(pm)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	assert.Empty(t, runtime.wakeCalls, "nothing has asked for the model yet")
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name],
		`"wakeByRequest":true`, "the route tells the gateway to ask this controller")

	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	assert.Equal(t, served, runtime.wakeCalls[0].ModelName)
	assert.True(t, strings.HasPrefix(runtime.wakeCalls[0].OperationID, "controller-wake/"))
	assert.True(t, strings.HasSuffix(runtime.wakeCalls[0].OperationID, "/2026-10-01T08:00:00Z"),
		"one operation per request, however many passes see it")
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Waking")
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key, "the request stays until the engine serves")

	runtime.models[served] = ModelInfo{ModelName: served, Port: port, Phase: "active"}
	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, key)
	assert.Len(t, runtime.wakeCalls, 1)
}

// An engine that was woken and never finishes booting keeps its request for
// the request's lifetime, and no longer. The request is then taken back
// quietly, since the wake was carried out.
func TestReconcileTakesBackTheRequestOfAnEngineThatNeverFinishesBooting(t *testing.T) {
	r, runtime, pm, port := sleepingClaim(t)
	served := servedModelName(pm)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	now := time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC)
	r.Now = func() time.Time { return now }
	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	drainEvents(t, r)

	// The engine is up, and never becomes ready.
	runtime.models[served] = ModelInfo{ModelName: served, Port: port, Phase: "active"}
	runtime.notReady = true
	for _, at := range []time.Duration{10 * time.Second, 4 * time.Minute} {
		now = time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC).Add(at)
		reconcileOnce(t, r, pm.Name)
		require.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
		assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key, "the request stays while the engine boots")
	}

	now = time.Date(2026, time.October, 1, 8, 5, 10, 0, time.UTC)
	reconcileOnce(t, r, pm.Name)

	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, key, "the request does not outlive its lifetime")
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WakeRequestExpired",
		"the wake was carried out, so nothing expired")
	assert.Len(t, runtime.wakeCalls, 1)
}

func TestReconcileMovesAnEngineTheRuntimeCouldNotWake(t *testing.T) {
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	// Asleep, the engine is held to its floor, as a division leaves it.
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 100,
	}}
	sleeper, sleeperSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	sleeper.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + pm.Name: "2026-10-01T08:00:00Z"}
	sleeperSnapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 100,
		ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	roomy, roomySnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	r, runtime := newReconciler(t, pm, sleeper, roomy)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		sleeper.Status.PodIP: sleeperSnapshot,
		roomy.Status.PodIP:   roomySnapshot,
	}
	runtime.failWake = true
	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC) }

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	require.Len(t, runtime.deactivateCalls, 1, "the engine that could not wake is stopped")
	assert.Equal(t, []string{roomy.Status.PodIP}, runtime.activatedOn)
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "Moving")
	assert.Contains(t, events, "because it could not be woken there")
	assert.NotContains(t, events, "WakeFailed")
}

func TestReconcileSaysWakingOnlyForTheCallThatWokeTheEngine(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Waking")

	// The runtime has not shown the engine awake yet, so the next pass asks
	// again, and the runtime answers that it applied this wake before.
	runtime.wakeAlreadyApplied = true
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 2)
	assert.Equal(t, runtime.wakeCalls[0].OperationID, runtime.wakeCalls[1].OperationID)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "Waking")
}

func TestReconcileTimesAWakeRequestOnItsOwnClock(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	// The gateway's clock is ten minutes behind the controller's.
	askWake(t, r, "warm-1", pm.Name, "2026-10-01T07:50:00Z")

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1, "a request is not old just because its writer's clock is behind")
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WakeRequestExpired")
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key)
}

func TestReconcileReportsAWakeThatFailedAndTakesItsRequestBack(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	runtime.failWake = true

	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "WakeFailed")
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name,
		"the next request for the model asks again")
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Instances[0].Phase)
}

// overcommittedSleeper is a claim whose engine sleeps on warm-1 and declares
// more than that card holds, as a declaration that grew while the engine slept
// would. A request asked for it at 08:00.
func overcommittedSleeper(t *testing.T, others ...client.Object) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim) {
	t.Helper()
	pm := claimWithCost(900, 200)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 200,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + pm.Name: "2026-10-01T08:00:00Z"}
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 200,
		ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	r, runtime := newReconciler(t, append([]client.Object{pm, pod}, others...)...)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 1, 0, 0, time.UTC) }
	return r, runtime, pm
}

func TestReconcileAsksAgainForAWakeTheRuntimeDidNotAnswer(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	runtime.wakeErr = fmt.Errorf("runtime 10.0.0.1:8080 %w", errRuntimeSilent)

	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	assert.Empty(t, runtime.deactivateCalls, "a runtime that was not reached is no reason to move")
	assert.Empty(t, drainEvents(t, r))
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key, "the request stays, to be asked again")
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Instances[0].Phase)

	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 2)
	assert.Equal(t, runtime.wakeCalls[0].OperationID, runtime.wakeCalls[1].OperationID,
		"the same request is the same operation")

	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 5, 6, 0, time.UTC) }
	reconcileOnce(t, r, pm.Name)

	assert.Len(t, runtime.wakeCalls, 2, "a request that waited too long is not asked again")
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "WakeRequestExpired")
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, key)
}

func TestRefusedByRuntimeTellsTheRuntimesOwnReportFromAnyOtherFailure(t *testing.T) {
	const wakeURL = "http://10.0.0.1:8080/v1/runtime/models/wake"
	for name, c := range map[string]struct {
		err     error
		refused bool
	}{
		"a refusal":                {statusError("POST", wakeURL, 404, nil), true},
		"the runtime's own report": {statusError("POST", wakeURL, 500, []byte(`{"status":"error","message":"boom"}`)), true},
		"a bare server error":      {statusError("POST", wakeURL, 500, []byte("Internal Server Error")), false},
		"a proxy that gave up":     {statusError("POST", wakeURL, 502, []byte("<html>bad gateway</html>")), false},
		"a runtime left alone":     {fmt.Errorf("runtime 10.0.0.1:8080 %w", errRuntimeSilent), false},
		"no answer in time":        {&url.Error{Op: "Post", URL: wakeURL, Err: context.DeadlineExceeded}, false},
	} {
		assert.Equal(t, c.refused, refusedByRuntime(c.err), name)
	}
}

func TestReconcileAsksAgainForAWakeAProxyFailed(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	runtime.wakeErr = statusError("POST", "http://10.0.0.1:8080/v1/runtime/models/wake", 502,
		[]byte("<html>bad gateway</html>"))

	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.wakeCalls, 1)
	assert.Empty(t, runtime.deactivateCalls, "a failure the runtime did not report is no reason to move")
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WakeFailed")
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key, "the request stays, to be asked again")
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Instances[0].Phase)
}

func TestReconcileLeavesAnEngineAsleepOnACardPromisedMoreThanItHas(t *testing.T) {
	r, runtime, pm := overcommittedSleeper(t)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "WaitingForRoom")
	pod := podNamed(t, r, "warm-1")
	assert.Contains(t, pod.Annotations, key, "the request waits for room")
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"reason":"WaitingForRoom"`,
		"the route tells the gateway what the client waits for")
	got := getModel(t, r, pm.Name)
	assert.Equal(t, instanceReasonWaitingForRoom, got.Status.Instances[0].Reason)
	ready := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, readyReasonWaitingForRoom, ready.Reason)

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WaitingForRoom",
		"raised once: an Event on every pass would crowd out the claim's later Events")
	assert.Equal(t, instanceReasonWaitingForRoom, getModel(t, r, pm.Name).Status.Instances[0].Reason)
}

func TestReconcileTakesBackAWakeRequestThatWaitedTooLong(t *testing.T) {
	r, runtime, pm := overcommittedSleeper(t)
	reconcileOnce(t, r, pm.Name)
	drainEvents(t, r)

	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 6, 1, 0, time.UTC) }
	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "WakeRequestExpired")
	pod := podNamed(t, r, "warm-1")
	assert.NotContains(t, pod.Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	assert.NotContains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"reason"`)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances[0].Reason)
	ready := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, "EngineSleeping", ready.Reason)
}

func TestReconcileMovesAnEngineItsCardCannotTakeBack(t *testing.T) {
	roomy, roomySnapshot := sizedWarmPod("warm-2", testPeerIP, 2000)
	r, runtime, pm := overcommittedSleeper(t, roomy)
	runtime.snapshots[roomy.Status.PodIP] = roomySnapshot

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls, "a card that cannot take the engine back is not asked to")
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Empty(t, got.Status.Instances[0].Reason)
	require.Len(t, runtime.deactivateCalls, 1, "the sleeping engine is stopped")
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "Moving")
	assert.Contains(t, events, "because its card could not take it back from sleep")
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
}

func TestReconcileLeavesTheEngineAsleepWhenItsMoveCannotBeWritten(t *testing.T) {
	roomy, roomySnapshot := sizedWarmPod("warm-2", testPeerIP, 2000)
	r, runtime, pm := overcommittedSleeper(t, roomy)
	runtime.snapshots[roomy.Status.PodIP] = roomySnapshot
	// The write that marks the move is refused once, as the write of a claim
	// read a moment too early is.
	refused := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			claim, ok := obj.(*modelv1alpha1.ModelClaim)
			if ok && !refused && len(claim.Status.Instances) == 1 &&
				claim.Status.Instances[0].Reason == instanceReasonNoRoomToWake {
				refused = true
				return apierrors.NewConflict(schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
					obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	})

	require.NoError(t, err)
	require.True(t, refused, "the move was marked")
	assert.True(t, result.Requeue)
	assert.Empty(t, runtime.deactivateCalls, "the engine is not stopped before its move is written")
	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, got.Status.Instances[0].Phase)
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)

	// The next pass decides again, and moves the claim.
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	require.Len(t, runtime.deactivateCalls, 1)
}

// aMoveThatDoesNotLand has a claim's engine asleep on warm-1, with a request
// to wake it. With crowded, a neighbour on warm-1 leaves no room for the wake;
// without it, the runtime cannot wake the engine. Either way the claim is
// marked to move. warm-2 can take it by the account, but its card cannot be
// divided, since the engine on it does not take a smaller limit. So the move
// does not land, and the engine on warm-1 is not stopped.
func aMoveThatDoesNotLand(t *testing.T, crowded bool) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *RuntimeSnapshot) {
	t.Helper()
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 100,
	}}
	home, homeSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	home.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + pm.Name: "2026-10-01T08:00:00Z"}
	homeSnapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 100,
		ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	away, awaySnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	other := claimOnPod("other", "warm-2", modelv1alpha1.ModelClaimActive, 300, 100)
	other.Status.Instances[0].KVLimitBytes = 600
	awaySnapshot.Models = []RuntimeSnapshotModel{engineHolding("other", 50, 600)}
	objects := []client.Object{pm, home, away, other}
	if crowded {
		neighbour := claimOnPod("neighbour", "warm-1", modelv1alpha1.ModelClaimActive, 600, 100)
		neighbour.Status.Instances[0].KVLimitBytes = 100
		homeSnapshot.Models = append(homeSnapshot.Models, engineHolding("neighbour", 50, 100))
		objects = append(objects, neighbour)
	}
	r, runtime := newReconciler(t, objects...)
	runtime.snapshots = map[string]*RuntimeSnapshot{home.Status.PodIP: homeSnapshot, away.Status.PodIP: awaySnapshot}
	runtime.deafToKVLimits = true
	runtime.failWake = !crowded
	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC) }

	reconcileOnce(t, r, pm.Name)

	reason := instanceReasonWakeFailed
	if crowded {
		reason = instanceReasonNoRoomToWake
	}
	got := getModel(t, r, pm.Name)
	require.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	require.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	require.Equal(t, reason, got.Status.Instances[0].Reason)
	require.Empty(t, runtime.deactivateCalls, "the move did not start")
	require.Empty(t, runtime.activateCalls)
	drainEvents(t, r)
	return r, runtime, pm, homeSnapshot
}

// A move for want of room that no other pod took is called off once the
// engine's own card can take it back, and the engine wakes where it sleeps.
// Until then the mark would stand, and the claim would wait for another pod.
func TestReconcileCallsOffAMoveThatHasNotStartedOnceTheCardHasRoom(t *testing.T) {
	r, runtime, pm, homeSnapshot := aMoveThatDoesNotLand(t, true)
	require.NoError(t, r.Delete(context.Background(), &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "neighbour", Namespace: testNamespace},
	}))
	homeSnapshot.Models = homeSnapshot.Models[:1]

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, got.Status.Instances[0].Phase)
	assert.Empty(t, got.Status.Instances[0].Reason)
	assert.Equal(t, []string{"10.0.0.1"}, runtime.wokenOn, "woken where it sleeps")
	assert.Empty(t, runtime.deactivateCalls)
	assert.Empty(t, runtime.activateCalls)
	events := strings.Join(drainEvents(t, r), "\n")
	assert.Contains(t, events, "MoveCalledOff")
	assert.Contains(t, events, "stays asleep on pod warm-1")
}

// The route says at once that a called-off move stays, as it says at once
// that a claim moves. With no request waiting, nothing is woken in that pass,
// and until the next health check the gateway would tell a client that the
// claim moves, and ask no wake.
func TestReconcileSaysOnTheRouteThatACalledOffMoveStays(t *testing.T) {
	r, runtime, pm, homeSnapshot := aMoveThatDoesNotLand(t, true)
	require.NoError(t, r.Delete(context.Background(), &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "neighbour", Namespace: testNamespace},
	}))
	homeSnapshot.Models = homeSnapshot.Models[:1]
	home := podNamed(t, r, "warm-1")
	patch := client.MergeFrom(home.DeepCopy())
	delete(home.Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	require.NoError(t, r.Patch(context.Background(), home, patch))

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	binding, routed := utils.ModelClaimBindingsFromPod(podNamed(t, r, "warm-1"))[servedModelName(pm)]
	require.True(t, routed)
	assert.Equal(t, constants.ModelClaimRoutingStateSleeping, binding.State)
	assert.Empty(t, binding.Reason)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "MoveCalledOff")
}

func TestReconcileKeepsAMoveWhileTheCardHasNoRoom(t *testing.T) {
	r, runtime, pm, _ := aMoveThatDoesNotLand(t, true)

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Equal(t, instanceReasonNoRoomToWake, got.Status.Instances[0].Reason)
	assert.Empty(t, runtime.wakeCalls)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "MoveCalledOff")
}

// An engine that is no longer there has been stopped for the move, so the move
// has started, and it goes on.
func TestReconcileKeepsAMoveWhoseEngineIsGone(t *testing.T) {
	r, _, pm, homeSnapshot := aMoveThatDoesNotLand(t, true)
	require.NoError(t, r.Delete(context.Background(), &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "neighbour", Namespace: testNamespace},
	}))
	homeSnapshot.Models = nil

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "MoveCalledOff")
}

// A move after a wake the runtime refused is not called off: the card has
// room, and the engine still could not wake on it.
func TestReconcileKeepsAMoveAfterAWakeTheRuntimeRefused(t *testing.T) {
	r, runtime, pm, _ := aMoveThatDoesNotLand(t, false)

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Equal(t, instanceReasonWakeFailed, got.Status.Instances[0].Reason)
	assert.Len(t, runtime.wakeCalls, 1, "the engine is not asked to wake again")
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "MoveCalledOff")
}

func TestMarkingAMoveWritesItAndSaysItOnTheRoute(t *testing.T) {
	r, runtime, pm := overcommittedSleeper(t)
	claim := getModel(t, r, pm.Name)

	require.NoError(t, r.markMoving(context.Background(), claim, 0, podNamed(t, r, "warm-1"),
		instanceReasonNoRoomToWake, "its card on pod warm-1 is promised more than it has"))

	assert.Empty(t, runtime.deactivateCalls, "marking a move does nothing to the engine")
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Equal(t, instanceReasonNoRoomToWake, got.Status.Instances[0].Reason)
	binding, routed := utils.ModelClaimBindingsFromPod(podNamed(t, r, "warm-1"))[servedModelName(pm)]
	require.True(t, routed)
	assert.Equal(t, constants.ModelClaimRoutingStateFailed, binding.State)
	assert.Equal(t, readyReasonMoving, binding.Reason)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Moving")
}

func TestWakeRequestedListsTheClaimsOnceAPass(t *testing.T) {
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	objs := []client.Object{}
	snapshots := map[string]*RuntimeSnapshot{}
	for i, ip := range []string{"10.0.0.1", testPeerIP} {
		name := fmt.Sprintf("warm-%d", i+1)
		pm.Status.Instances = append(pm.Status.Instances, modelv1alpha1.ModelClaimInstance{
			Pod: name, Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 100,
		})
		pod, snapshot := sizedWarmPod(name, ip, 1000)
		pod.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + pm.Name: "2026-10-01T08:00:00Z"}
		snapshot.Models = []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
			KVCapacityBytes: 100,
			ClaimRef:        &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
		}}
		objs = append(objs, pod)
		snapshots[ip] = snapshot
	}
	r, runtime := newReconciler(t, append([]client.Object{pm}, objs...)...)
	runtime.snapshots = snapshots
	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 0, 5, 0, time.UTC) }
	counting := &countingReader{Reader: r.Client}
	r.APIReader = counting
	claim := getModel(t, r, pm.Name)
	candidates, err := r.listCandidateWarmPods(context.Background(), claim)
	require.NoError(t, err)

	woke, err := r.wakeRequested(context.Background(), claim, candidates, newRuntimeReadings(runtime))

	require.NoError(t, err)
	assert.True(t, woke)
	assert.Len(t, runtime.wakeCalls, 2)
	assert.Equal(t, 1, counting.lists, "both wakes are judged by one listing of the claims")
}

func TestTheGatewayReadsTheRouteAsTheControllerWroteIt(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	// A name with a character that Go quotes in a way JSON does not.
	odd := "odd\amodel"
	pm.Spec.ModelName = &odd
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	r, _ := newReconciler(t, pm, pod)

	require.NoError(t, r.annotateWarmPodWithState(context.Background(), pm, podNamed(t, r, "warm-1"), 0,
		constants.ModelClaimRoutingStateSleeping, readyReasonWaitingForRoom))

	binding, routed := utils.ModelClaimBindingsFromPod(podNamed(t, r, "warm-1"))[odd]
	require.True(t, routed, "the route is read back")
	assert.Equal(t, utils.ModelClaimBinding{
		Model: odd, Port: 0, State: constants.ModelClaimRoutingStateSleeping, Claim: pm.Name,
		WakeByRequest: true, Reason: readyReasonWaitingForRoom,
	}, binding)
}

func TestWakeRequestsEnqueueTheirClaimAlone(t *testing.T) {
	key := constants.ModelClaimWakeAnnotationPrefix + "qwen2-7b"
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.ResourceVersion = "1"
	asked := pod.DeepCopy()
	asked.ResourceVersion = "2"
	asked.Annotations = map[string]string{key: "2026-10-01T08:00:00Z"}

	assert.True(t, wakeRequestsChanged().Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: asked}))
	assert.False(t, notOnlyWakeRequests().Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: asked}),
		"a wake request alone does not enqueue every claim in the namespace")
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "qwen2-7b"}}},
		enqueueRequestedWakes(context.Background(), asked))

	relabeled := asked.DeepCopy()
	relabeled.ResourceVersion = "3"
	relabeled.Labels["extra"] = "yes"
	assert.True(t, notOnlyWakeRequests().Update(event.UpdateEvent{ObjectOld: asked, ObjectNew: relabeled}))
	assert.False(t, wakeRequestsChanged().Update(event.UpdateEvent{ObjectOld: asked, ObjectNew: relabeled}),
		"the same request again is no news")

	takenBack := asked.DeepCopy()
	takenBack.ResourceVersion = "4"
	delete(takenBack.Annotations, key)
	assert.False(t, wakeRequestsChanged().Update(event.UpdateEvent{ObjectOld: asked, ObjectNew: takenBack}))
	assert.False(t, notOnlyWakeRequests().Update(event.UpdateEvent{ObjectOld: asked, ObjectNew: takenBack}))
}

func TestRouteChangesEnqueueOnlyTheClaimsOnTheirPod(t *testing.T) {
	podA := warmPod("warm-a", "b300-pool-a", true, corev1.PodRunning)
	podB := warmPod("warm-b", "b300-pool-a", true, corev1.PodRunning)
	onPod := func(name, pod string) *modelv1alpha1.ModelClaim {
		claim := sampleModelClaim()
		claim.Name = name
		if pod != "" {
			claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: pod, Port: 20000}}
		}
		return claim
	}
	r, _ := newReconciler(t, podA, podB,
		onPod("first-on-a", podA.Name), onPod("second-on-a", podA.Name), onPod("on-b", podB.Name), onPod("waiting", ""))

	podA.ResourceVersion = "1"
	routed := podA.DeepCopy()
	routed.ResourceVersion = "2"
	routed.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "first-on-a": `{"model":"first-on-a","port":20000,"state":"active"}`,
	}
	change := event.UpdateEvent{ObjectOld: podA, ObjectNew: routed}
	assert.False(t, notOnlyAnnotationsChanged().Update(change), "a route alone does not enqueue every claim in the namespace")
	assert.True(t, onlyAnnotationsChanged().Update(change))
	assert.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "first-on-a"}},
		{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "second-on-a"}},
	}, enqueueModelClaimsOnPod(r.Client)(context.Background(), routed))

	relabeled := routed.DeepCopy()
	relabeled.ResourceVersion = "3"
	relabeled.Labels["extra"] = "yes"
	change = event.UpdateEvent{ObjectOld: routed, ObjectNew: relabeled}
	assert.True(t, notOnlyAnnotationsChanged().Update(change))
	assert.False(t, onlyAnnotationsChanged().Update(change))

	unready := routed.DeepCopy()
	unready.ResourceVersion = "4"
	unready.Status.Conditions = append(unready.Status.Conditions,
		corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse})
	change = event.UpdateEvent{ObjectOld: routed, ObjectNew: unready}
	assert.True(t, notOnlyAnnotationsChanged().Update(change), "readiness changes where a claim can go")
	assert.False(t, onlyAnnotationsChanged().Update(change))

	resynced := routed.DeepCopy()
	resynced.ResourceVersion = "5"
	change = event.UpdateEvent{ObjectOld: routed, ObjectNew: resynced}
	assert.False(t, notOnlyAnnotationsChanged().Update(change), "an update that changes nothing enqueues no claim")
	assert.False(t, onlyAnnotationsChanged().Update(change))
}

func TestDeannotateWarmPodTakesTheWakeRequestWithTheRoute(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "qwen2-7b":  `{"model":"qwen2-7b","port":0,"state":"sleeping","wakeByRequest":true}`,
		constants.ModelClaimWakeAnnotationPrefix + "qwen2-7b": "2026-10-01T08:00:00Z",
		constants.ModelClaimWakeAnnotationPrefix + "other":    "2026-10-01T08:00:01Z",
	}
	r, _ := newReconciler(t, pod)

	r.deannotateWarmPod(context.Background(), testNamespace, "warm-1", "qwen2-7b")

	assert.Equal(t, map[string]string{constants.ModelClaimWakeAnnotationPrefix + "other": "2026-10-01T08:00:01Z"},
		podNamed(t, r, "warm-1").Annotations)
}

// The gateway may write a wake request a moment before the route is taken
// away, and the cache may not show it yet. The request still goes with the
// route, so none is left behind for a claim that is gone.
func TestDeannotateWarmPodTakesAWakeRequestTheCacheHasNotSeen(t *testing.T) {
	route := constants.ModelClaimPodAnnotationPrefix + "qwen2-7b"
	wake := constants.ModelClaimWakeAnnotationPrefix + "qwen2-7b"
	other := constants.ModelClaimPodAnnotationPrefix + "other"
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		route: `{"model":"qwen2-7b","port":0,"state":"sleeping","wakeByRequest":true}`,
		other: `{"model":"other","port":9001,"state":"active"}`,
	}
	r, _ := newReconciler(t, pod)
	ctx := context.Background()
	cached := podNamed(t, r, "warm-1")
	asked := cached.DeepCopy()
	asked.Annotations[wake] = "2026-10-01T08:00:00Z"
	require.NoError(t, r.Update(ctx, asked))
	apiServer := r.Client
	r.Client = interceptor.NewClient(apiServer.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if out, ok := obj.(*corev1.Pod); ok && key.Name == "warm-1" {
				cached.DeepCopyInto(out)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r.deannotateWarmPod(ctx, testNamespace, "warm-1", "qwen2-7b")

	after := &corev1.Pod{}
	require.NoError(t, apiServer.Get(ctx, client.ObjectKeyFromObject(pod), after))
	assert.Equal(t, map[string]string{other: `{"model":"other","port":9001,"state":"active"}`}, after.Annotations,
		"the request goes with the route")
}
