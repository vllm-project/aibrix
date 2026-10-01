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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
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

func TestReconcileLeavesAnEngineAsleepOnACardPromisedMoreThanItHas(t *testing.T) {
	// The claim declares more than its card holds, as a declaration that grew
	// while the engine slept would.
	pm := claimWithCost(900, 200)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 200,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	pod.Annotations = map[string]string{key: "2026-10-01T08:00:00Z"}
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 200,
		ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.wakeCalls)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WaitingForRoom",
		"an Event on every pass would crowd out the claim's later Events")
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, key, "the request waits for room")
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
