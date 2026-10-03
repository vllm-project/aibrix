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

func TestReconcileMovesAnEngineTheRuntimeCouldNotWake(t *testing.T) {
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 700,
	}}
	sleeper, sleeperSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	sleeper.Annotations = map[string]string{constants.ModelClaimWakeAnnotationPrefix + pm.Name: "2026-10-01T08:00:00Z"}
	sleeperSnapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 700,
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

	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 5, 1, 0, time.UTC) }
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

	r.Now = func() time.Time { return time.Date(2026, time.October, 1, 8, 6, 0, 0, time.UTC) }
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
