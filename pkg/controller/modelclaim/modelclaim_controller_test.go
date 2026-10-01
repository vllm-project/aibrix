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
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	testNamespace = "default"
	testPeerIP    = "10.0.0.2"
	warmAppLabel  = "warm"
)

// fakeRuntime is an in-process RuntimeClient that records calls and hands out
// monotonic ports, so the reconcile loop can be tested without a real runtime.
type fakeRuntime struct {
	activateCalls   []ActivateRequest
	deactivateCalls []DeactivateRequest
	kvLimitCalls    []SetKVLimitRequest
	sleepCalls      []SleepRequest
	listCalls       int
	snapshotCalls   int
	portSeq         int32
	failActivate    bool
	// loseActivateAnswer makes Activate start the engine and fail as a call
	// whose answer never arrived.
	loseActivateAnswer bool
	// notReady makes runtime snapshots report activated engines as not yet
	// serveable, so a test can hold a model in the Activating phase.
	notReady bool
	// models tracks the engines the fake currently hosts, keyed by served
	// model name, so Snapshot can drive the controller's readiness gate.
	models map[string]ModelInfo
	// snapshots reports per-pod runtime state for Phase-2 placement tests.
	snapshots map[string]*RuntimeSnapshot
	// nilSnapshots lets defensive-path tests model an invalid client response.
	nilSnapshots map[string]bool
	// deafToKVLimits makes SetKVLimit report success without the segment
	// changing, which is what writing into a segment that is not there looks
	// like from the controller's side.
	deafToKVLimits bool
	// onKVLimit runs after a limit is written, so a test can model an engine
	// that grew between the plan and the reading that confirms it.
	onKVLimit func()
}

func (f *fakeRuntime) Activate(_ context.Context, _ string, _ int, req *ActivateRequest) (*ActivateResponse, error) {
	f.activateCalls = append(f.activateCalls, *req)
	if f.failActivate {
		return &ActivateResponse{Status: "error", Message: "boom"}, &runtimeRefusal{"activate failed: boom"}
	}
	f.portSeq++
	port := req.Port
	if port == 0 {
		port = 9000 + f.portSeq
	}
	if f.models == nil {
		f.models = map[string]ModelInfo{}
	}
	f.models[req.ModelName] = ModelInfo{
		ModelName: req.ModelName,
		Port:      port,
		IPCName:   req.IPCName,
		Phase:     "active",
		Ready:     !f.notReady,
	}
	if f.loseActivateAnswer {
		return nil, &url.Error{Op: "Post", URL: activatePath, Err: context.DeadlineExceeded}
	}
	return &ActivateResponse{Status: "success", ModelName: req.ModelName, Port: port, IPCName: req.IPCName}, nil
}

func (f *fakeRuntime) Deactivate(_ context.Context, _ string, _ int, req *DeactivateRequest) error {
	f.deactivateCalls = append(f.deactivateCalls, *req)
	delete(f.models, req.ModelName)
	return nil
}

func (f *fakeRuntime) SetKVLimit(_ context.Context, _ string, _ int, req *SetKVLimitRequest) (*RuntimeOperationResponse, error) {
	f.kvLimitCalls = append(f.kvLimitCalls, *req)
	// A limit is written into the engine's segment, and a snapshot reads that
	// same segment back, so a seeded engine reports the new limit afterwards.
	if !f.deafToKVLimits {
		for _, snapshot := range f.snapshots {
			for i := range snapshot.Models {
				if snapshot.Models[i].ModelName == req.ModelName {
					snapshot.Models[i].KVCapacityBytes = req.LimitBytes
				}
			}
		}
	}
	if f.onKVLimit != nil {
		f.onKVLimit()
	}
	return &RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: true, Phase: "active",
	}, nil
}

func (f *fakeRuntime) Sleep(_ context.Context, _ string, _ int, req *SleepRequest) (*RuntimeOperationResponse, error) {
	f.sleepCalls = append(f.sleepCalls, *req)
	return &RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: true, Phase: "sleeping",
	}, nil
}

func (f *fakeRuntime) Wake(_ context.Context, _ string, _ int, req *WakeRequest) (*RuntimeOperationResponse, error) {
	return &RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: true, Phase: "active",
	}, nil
}

func (f *fakeRuntime) ListModels(_ context.Context, _ string, _ int) ([]ModelInfo, error) {
	f.listCalls++
	out := make([]ModelInfo, 0, len(f.models))
	for _, m := range f.models {
		if m.Phase != "sleeping" {
			m.Ready = !f.notReady // readiness evaluated at call time so tests can flip it
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeRuntime) Snapshot(_ context.Context, podIP string, _ int) (*RuntimeSnapshot, error) {
	f.snapshotCalls++
	if f.nilSnapshots[podIP] {
		return nil, nil
	}
	result := &RuntimeSnapshot{}
	if snapshot, ok := f.snapshots[podIP]; ok {
		copy := *snapshot
		copy.Models = append([]RuntimeSnapshotModel(nil), snapshot.Models...)
		result = &copy
	}
	for _, model := range f.models {
		present := false
		for i := range result.Models {
			if result.Models[i].ModelName == model.ModelName {
				present = true
				break
			}
		}
		if present {
			continue
		}
		ready := model.Ready
		if model.Phase != "sleeping" {
			ready = !f.notReady
		}
		result.Models = append(result.Models, RuntimeSnapshotModel{
			ModelName: model.ModelName,
			Port:      model.Port,
			IPCName:   model.IPCName,
			Phase:     model.Phase,
			Alive:     model.Phase != "failed",
			Ready:     ready,
			// This fake starts engines, and does not model a KV allocator. A
			// test that wants one seeds the model in f.snapshots instead.
			KVUsedBytes:     kvLimitUnknown,
			KVCapacityBytes: kvLimitUnknown,
		})
	}
	return result, nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	return scheme
}

// warmPod builds a running warm pool pod with an IP set.
func warmPod(name, poolName string, enabled bool, phase corev1.PodPhase) *corev1.Pod {
	labels := map[string]string{}
	if poolName != "" {
		labels[constants.ModelPoolLabelName] = poolName
	}
	if enabled {
		labels[constants.ModelPoolLabelEnabled] = constants.ModelPoolLabelEnabledValue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase, PodIP: "10.0.0.1"},
	}
}

func warmPodWithGPUs(name, poolName string, gpuCount int64) *corev1.Pod {
	pod := warmPod(name, poolName, true, corev1.PodRunning)
	pod.Spec.Containers = []corev1.Container{{
		Name: "aibrix-runtime",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpuCount, resource.DecimalSI),
		}},
	}}
	return pod
}

func sampleModelClaim() *modelv1alpha1.ModelClaim {
	return &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen2-7b", Namespace: testNamespace},
		Spec: modelv1alpha1.ModelClaimSpec{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{constants.ModelPoolLabelName: "b300-pool-a"},
			},
			ArtifactURL: "huggingface://Qwen/Qwen2-7B-Instruct",
			Engine:      "vllm",
			EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{
				Args: map[string]string{"--max-model-len": "2048"},
			},
			// Every claim has to say what it costs to be placed at all.
			PerGPU: &modelv1alpha1.ModelClaimPerGPU{
				MaximumFootprint: resource.MustParse("30Gi"),
				KVFloor:          resource.MustParse("10Gi"),
			},
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) (*ModelClaimReconciler, *fakeRuntime) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		Build()
	runtime := &fakeRuntime{}
	return &ModelClaimReconciler{
		Client:     c,
		Scheme:     scheme,
		Recorder:   record.NewFakeRecorder(32),
		Runtime:    runtime,
		PoolPolicy: newPoolPolicyManager(time.Now),
		SnapshotCache: newRuntimeSnapshotCache(
			defaultRuntimeSnapshotTTL, time.Now,
		),
	}, runtime
}

func TestReconcilePoolPoliciesAppliesDeploymentKVFirstPolicy(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-runtime-pool",
			Namespace: testNamespace,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{
					"reclaim": {
						"mode": "kv-first",
						"capacityBytes": 1000,
						"guaranteedFloorPercent": 20
					}
				}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": warmAppLabel}},
		},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "warm-runtime-pool-rs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Labels["app"] = warmAppLabel
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}

	r, runtime := newReconciler(t, deployment, replicaSet, pod)
	now := time.Unix(1_700_000_000, 0)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			ObservedAt:   now,
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{
				{
					ModelName: "hot", IPCName: "kvc_hot", Phase: "active", Alive: true, Ready: true,
					KVUsedBytes: 100, KVCapacityBytes: 200,
					RequestMetricsObserved: true, RequestsRunning: 2, RequestSuccessTotal: &requestSuccessTotal,
				},
				{
					ModelName: "idle", Phase: "active", Alive: true, Ready: true,
					KVUsedBytes: 100, KVCapacityBytes: 800,
					RequestMetricsObserved: true, RequestSuccessTotal: &requestSuccessTotal,
				},
			},
		},
	}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	require.Len(t, runtime.kvLimitCalls, 2)
	limits := map[string]int64{}
	for _, call := range runtime.kvLimitCalls {
		limits[call.ModelName] = call.LimitBytes
		if call.ModelName == "idle" {
			assert.Contains(t, call.OperationID, "/idle/")
		}
	}
	assert.Equal(t, int64(800), limits["hot"])
	assert.Equal(t, int64(200), limits["idle"])

	firstOperationIDs := map[string]string{}
	for _, call := range runtime.kvLimitCalls {
		firstOperationIDs[call.ModelName] = call.OperationID
	}
	runtime.kvLimitCalls = nil
	now = now.Add(DefaultRequeueDuration)
	runtime.snapshots[pod.Status.PodIP].ObservedAt = now
	// Both engines restarted and put their own limits back, so the same plan
	// has to be written again.
	runtime.snapshots[pod.Status.PodIP].Models[0].KVCapacityBytes = 200
	runtime.snapshots[pod.Status.PodIP].Models[1].KVCapacityBytes = 800

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	require.Len(t, runtime.kvLimitCalls, 2)
	for _, call := range runtime.kvLimitCalls {
		assert.NotEqual(t, firstOperationIDs[call.ModelName], call.OperationID)
	}
}

func TestReconcilePoolPoliciesStandDownWhereAClaimHoldsTheLimit(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-runtime-pool",
			Namespace: testNamespace,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{"reclaim":{"capacityBytes":1000,"guaranteedFloorPercent":20}}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": warmAppLabel}},
		},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "warm-runtime-pool-rs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Labels["app"] = warmAppLabel
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod:          pod.Name,
		Port:         9001,
		Phase:        modelv1alpha1.ModelClaimActive,
		KVLimitBytes: 100,
	}}

	r, runtime := newReconciler(t, deployment, replicaSet, pod, pm)
	now := time.Unix(1_700_000_000, 0)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			ObservedAt:   now,
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{
				{
					ModelName: "qwen2-7b", Phase: "active", Alive: true, Ready: true,
					KVUsedBytes: 50, KVCapacityBytes: 100,
					// Busy, so the annotation would otherwise hand this engine
					// the whole configured capacity.
					RequestMetricsObserved: true, RequestsRunning: 2, RequestSuccessTotal: &requestSuccessTotal,
				},
			},
		},
	}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	assert.Empty(t, runtime.kvLimitCalls)
}

func TestReconcilePoolPoliciesSkipsNilRuntimeSnapshot(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-runtime-pool",
			Namespace: testNamespace,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{"reclaim":{"capacityBytes":1000}}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": warmAppLabel}},
		},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "warm-runtime-pool-rs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Labels["app"] = warmAppLabel
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}

	r, runtime := newReconciler(t, deployment, replicaSet, pod)
	runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	assert.Empty(t, runtime.kvLimitCalls)
	assert.Empty(t, runtime.sleepCalls)
}

// warmPoolObjects builds the Deployment/ReplicaSet/Pod ownership chain of one
// warm pool whose Deployment carries the given policy annotation.
func warmPoolObjects(policy string) (*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "warm-runtime-pool",
			Namespace:   testNamespace,
			Annotations: map[string]string{constants.ModelPoolPolicyAnnotationKey: policy},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": warmAppLabel}},
		},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "warm-runtime-pool-rs",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Labels["app"] = warmAppLabel
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	return deployment, replicaSet, pod
}

func drainEvents(t *testing.T, r *ModelClaimReconciler) []string {
	t.Helper()
	recorder := r.Recorder.(*record.FakeRecorder)
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestReconcilePoolPoliciesWarnsOnceForUnchangedInvalidPolicy(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects(`{"reclaim":{"capacityBytes":0}}`)
	r, runtime := newReconciler(t, deployment, replicaSet, pod)

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	events := drainEvents(t, r)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning InvalidPoolPolicy")
	assert.Contains(t, events[0], "invalid_capacity")
	// Invalid configuration is fail-closed: no KV plan may run.
	assert.Empty(t, runtime.kvLimitCalls)
}

func TestReconcilePoolPoliciesEmitsRecoveryOnceCorrected(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects(`{"reclaim":{"capacityBytes":0}}`)
	r, runtime := newReconciler(t, deployment, replicaSet, pod)
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "hot", Phase: "active", Alive: true, Ready: true,
				KVUsedBytes: 100, KVCapacityBytes: 200,
				RequestMetricsObserved: true, RequestsRunning: 2, RequestSuccessTotal: &requestSuccessTotal,
			}},
		},
	}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	deployment.Annotations[constants.ModelPoolPolicyAnnotationKey] = `{"reclaim":{"capacityBytes":1000}}`
	require.NoError(t, r.Update(context.Background(), deployment))

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	events := drainEvents(t, r)
	require.Len(t, events, 2)
	assert.Contains(t, events[0], "Warning InvalidPoolPolicy")
	assert.Contains(t, events[1], "Normal PoolPolicyValid")
	// Policy execution resumes with the corrected configuration.
	assert.NotEmpty(t, runtime.kvLimitCalls)
}

func TestReconcilePoolPoliciesStaysQuietForValidPolicy(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects(`{"reclaim":{"capacityBytes":1000}}`)
	r, _ := newReconciler(t, deployment, replicaSet, pod)

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	assert.Empty(t, drainEvents(t, r))
}

func TestReconcilePoolPoliciesSleepsIdleSingleReplica(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	deployment, replicaSet, pod := warmPoolObjects(`{"lifecycle":{"sleepAfterSeconds":60}}`)
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

	// A first observation establishes a conservative idle baseline.
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	require.Empty(t, runtime.sleepCalls)

	now = now.Add(61 * time.Second)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "qwen2-7b", runtime.sleepCalls[0].ModelName)
	assert.Equal(t, 1, runtime.sleepCalls[0].Level)
	assert.Contains(t, runtime.sleepCalls[0].OperationID, "/qwen2-7b/")
	gotClaim := getModel(t, r, claim.Name)
	require.Len(t, gotClaim.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, gotClaim.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, gotClaim.Status.Phase)
	gotPod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: pod.Name,
	}, gotPod))
	annotation := gotPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name]
	assert.Contains(t, annotation, `"port":0`)
	assert.Contains(t, annotation, `"state":"sleeping"`)
}

func TestReconcilePoolPoliciesUsesRuntimeTransitionAsWakeGrace(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	deployment, replicaSet, pod := warmPoolObjects(`{"lifecycle":{"sleepAfterSeconds":60}}`)
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
	lastTransition := now
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0"}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "qwen2-7b", Port: 20000, IPCName: "kvc_qwen2-7b",
				Phase: runtimePhaseActive, Alive: true, Ready: true,
				LastTransition:         &lastTransition,
				RequestMetricsObserved: true, RequestSuccessTotal: &requestSuccessTotal,
				ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
			}},
		},
	}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	now = now.Add(120 * time.Second)
	lastTransition = now.Add(-30 * time.Second)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	assert.Empty(t, runtime.sleepCalls, "a recent wake transition must start a fresh idle window")

	now = now.Add(31 * time.Second)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	assert.Len(t, runtime.sleepCalls, 1)
}

func reconcileOnce(t *testing.T, r *ModelClaimReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	require.NoError(t, err)
}

func TestRequeueOnConflict(t *testing.T) {
	result, err := requeueOnConflict(apierrors.NewConflict(
		schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
		"qwen", nil,
	))
	require.NoError(t, err)
	assert.True(t, result.Requeue)

	result, err = requeueOnConflict(fmt.Errorf("runtime unavailable"))
	require.Error(t, err)
	assert.False(t, result.Requeue)
}

func getModel(t *testing.T, r *ModelClaimReconciler, name string) *modelv1alpha1.ModelClaim {
	t.Helper()
	got := &modelv1alpha1.ModelClaim{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: name}, got))
	return got
}

func withFinalizer(pm *modelv1alpha1.ModelClaim) *modelv1alpha1.ModelClaim {
	controllerutil.AddFinalizer(pm, ModelClaimFinalizer)
	return pm
}

// TestListCandidateWarmPods verifies only running, enabled pods matching the
// PodSelector are considered candidates.
func TestListCandidateWarmPods(t *testing.T) {
	pods := []client.Object{
		warmPod("ready", "b300-pool-a", true, corev1.PodRunning),        // candidate
		warmPod("pending", "b300-pool-a", true, corev1.PodPending),      // wrong phase
		warmPod("not-enabled", "b300-pool-a", false, corev1.PodRunning), // missing enabled label
		warmPod("other-pool", "other-pool", true, corev1.PodRunning),    // selector mismatch
	}
	r, _ := newReconciler(t, pods...)

	got, err := r.listCandidateWarmPods(context.Background(), sampleModelClaim())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ready", got[0].Name)
}

func TestListCandidateWarmPodsFiltersMismatchedVLLMParallelism(t *testing.T) {
	pm := sampleModelClaim()
	pm.Spec.EngineConfig.Args["--tensor-parallel-size"] = "2"
	r, _ := newReconciler(t,
		warmPodWithGPUs("one-gpu", "b300-pool-a", 1),
		warmPodWithGPUs("two-gpu", "b300-pool-a", 2),
	)

	got, err := r.listCandidateWarmPods(context.Background(), pm)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "two-gpu", got[0].Name)
}

// TestReconcileAddsFinalizer verifies the first reconcile installs the finalizer
// before any external-effecting work.
func TestReconcileAddsFinalizer(t *testing.T) {
	pm := sampleModelClaim()
	r, runtime := newReconciler(t, pm)

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	assert.True(t, controllerutil.ContainsFinalizer(got, ModelClaimFinalizer))
	assert.Empty(t, runtime.activateCalls, "no activation before finalizer is set")
}

// TestReconcileActivatesOnCandidate verifies the controller bin-packs onto a
// warm pod and asks the runtime sidecar to activate the model.
func TestReconcileActivatesOnCandidate(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	)

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, "qwen2-7b", runtime.activateCalls[0].ModelName)
	assert.Equal(t, "kvc_qwen2-7b", runtime.activateCalls[0].IPCName)
	assert.Equal(t, "vllm", runtime.activateCalls[0].Engine)
	require.NotNil(t, runtime.activateCalls[0].EngineConfig)
	assert.Equal(t, "2048", runtime.activateCalls[0].EngineConfig.Args["--max-model-len"])

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, int32(1), got.Status.ReadyReplicas)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Phase)
	assert.Contains(t, []string{"warm-1", "warm-2"}, got.Status.Instances[0].Pod)
	assert.NotZero(t, got.Status.Instances[0].Port)
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestReconcilePlacementPrefersRuntimeSnapshot(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.UID = types.UID("claim-uid")
	cold := warmPod("cold", "b300-pool-a", true, corev1.PodRunning)
	cold.Status.PodIP = "10.0.0.1"
	hot := warmPod("hot", "b300-pool-a", true, corev1.PodRunning)
	hot.Status.PodIP = testPeerIP
	r, runtime := newReconciler(t, pm, cold, hot)
	// Both runtimes report a card, so both pods go through the account although
	// neither requests nvidia.com/gpu, and both cards can hold the claim.
	runtime.snapshots = map[string]*RuntimeSnapshot{
		"10.0.0.1": {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 80 << 30, HBMFreeBytes: 900, HBMUsableBytes: 80 << 30}},
		},
		testPeerIP: {
			Accelerators:    []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 80 << 30, HBMFreeBytes: 100, HBMUsableBytes: 80 << 30}},
			CachedArtifacts: []string{pm.Spec.ArtifactURL},
		},
	}

	reconcileOnce(t, r, pm.Name)

	// The pod that already has the artifact wins, although the other one has
	// more free memory.
	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, "hot", getModel(t, r, pm.Name).Status.Instances[0].Pod)
	require.NotNil(t, runtime.activateCalls[0].ClaimRef)
	assert.Equal(t, "default", runtime.activateCalls[0].ClaimRef.Namespace)
	assert.Equal(t, "qwen2-7b", runtime.activateCalls[0].ClaimRef.Name)
	assert.Equal(t, "claim-uid", runtime.activateCalls[0].ClaimRef.UID)
}

// TestReconcileReadinessGate verifies the controller does not make a model
// routable until its engine reports ready: while the engine is booting the
// instance stays Activating, the warm-pod annotation holds the non-routable marker
// (port 0), and ReadyReplicas is 0; once the runtime reports ready the annotation
// flips to the real port and the model becomes Active.
func TestReconcileReadinessGate(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)
	runtime.notReady = true // engine spawned but still booting

	reconcileOnce(t, r, pm.Name)

	// Engine spawned, but not routable: Activating + non-routable marker, 0 ready.
	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas, "still-booting engine must not count as ready")
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"], `"port":0`,
		"booting engine must not be routable")

	// Engine reports ready -> flip to the real port, become Active.
	runtime.notReady = false
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Phase)
	assert.Equal(t, int32(1), got.Status.ReadyReplicas)
	livePort := got.Status.Instances[0].Port
	assert.NotZero(t, livePort)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"],
		fmt.Sprintf(`"port":%d`, livePort), "ready engine must be routable on its real port")
	assert.Equal(t, 1, len(runtime.activateCalls), "readiness flip must not re-activate")
}

// TestReconcileActiveDemotedWhenUnhealthy verifies an Active instance whose
// engine stops reporting ready is demoted to Activating, re-stamped
// non-routable (port 0), and drops out of ReadyReplicas.
func TestReconcileActiveDemotedWhenUnhealthy(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	)

	reconcileOnce(t, r, pm.Name)
	got := getModel(t, r, pm.Name)
	require.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)

	runtime.notReady = true // engine crashed / restarted
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas)
	assert.Len(t, runtime.activateCalls, 1, "a transient restart must not move the claim")
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"], `"port":0`,
		"unhealthy engine must be non-routable")
}

func TestReconcileSnapshotCorrectsRouteToActualRuntimePort(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + pm.Name: `{"model":"qwen2-7b","port":9001}`,
	}
	r, runtime := newReconciler(t, pm, pod)
	runtime.models = map[string]ModelInfo{
		servedModelName(pm): {
			ModelName: servedModelName(pm), Port: 9001, Phase: "active", Ready: true,
		},
	}
	runtime.snapshots = map[string]*RuntimeSnapshot{
		"10.0.0.1": {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 9100, Phase: "active", Alive: true, Ready: true,
		}}},
	}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, int32(9100), got.Status.Instances[0].Port)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
	assert.Zero(t, runtime.listCalls, "routing health must use runtime snapshots")
	assert.GreaterOrEqual(t, runtime.snapshotCalls, 1)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: "warm-1",
	}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":9100`)
}

func TestReconcileSnapshotPortZeroNeverRoutesStaleStatusPort(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + pm.Name: `{"model":"qwen2-7b","port":9001}`,
	}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		"10.0.0.1": {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 0, Phase: "active", Alive: true, Ready: true,
		}}},
	}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, int32(0), got.Status.Instances[0].Port)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: "warm-1",
	}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)
}

func TestReconcileSnapshotTerminalFailureDeroutesAndFailsClaim(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + pm.Name: `{"model":"qwen2-7b","port":9001}`,
	}
	r, runtime := newReconciler(t, pm, pod)
	runtime.models = map[string]ModelInfo{
		servedModelName(pm): {
			ModelName: servedModelName(pm), Port: 9001, Phase: "active", Ready: true,
		},
	}
	runtime.snapshots = map[string]*RuntimeSnapshot{
		"10.0.0.1": {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm),
			Port:      9001,
			Phase:     "failed",
			Alive:     false,
			Ready:     false,
			LastError: "engine exited; restart budget exhausted",
		}}},
	}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	condition := meta.FindStatusCondition(
		got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady),
	)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "EngineFailed", condition.Reason)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: "warm-1",
	}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)
	assert.Empty(t, runtime.activateCalls)
}

func TestReconcileSnapshotTerminalFailureReschedulesClaim(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimActive
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	peerModel := "peer-model"
	peer := withFinalizer(sampleModelClaim())
	peer.Name = "peer-claim"
	peer.UID = types.UID("peer-uid")
	peer.Spec.ModelName = &peerModel
	peer.Status.Phase = modelv1alpha1.ModelClaimActive
	peer.Status.ReadyReplicas = 1
	peer.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9002, Phase: modelv1alpha1.ModelClaimActive,
	}}
	failedPod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	failedPod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + pm.Name:   `{"model":"qwen2-7b","port":9001,"state":"active"}`,
		constants.ModelClaimPodAnnotationPrefix + peer.Name: `{"model":"peer-model","port":9002,"state":"active"}`,
	}
	replacementPod := warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning)
	replacementPod.Status.PodIP = testPeerIP
	r, runtime := newReconciler(t, pm, peer, failedPod, replacementPod)
	runtime.models = map[string]ModelInfo{
		peerModel: {ModelName: peerModel, Port: 9002, Phase: runtimePhaseActive, Ready: true},
	}
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedPod.Status.PodIP: {Models: []RuntimeSnapshotModel{
			{
				ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseFailed,
				Alive: false, Ready: false, LastError: "restart budget exhausted",
				ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
			},
			{
				ModelName: peerModel, Port: 9002, Phase: runtimePhaseActive, Alive: true, Ready: true,
				ClaimRef: &ModelClaimRef{Namespace: peer.Namespace, Name: peer.Name, UID: string(peer.UID)},
			},
		}},
		testPeerIP: {},
	}

	reconcileOnce(t, r, pm.Name)

	rescheduled := getModel(t, r, pm.Name)
	require.Len(t, rescheduled.Status.Instances, 1)
	assert.Equal(t, "warm-2", rescheduled.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, rescheduled.Status.Instances[0].Phase)
	require.Len(t, runtime.activateCalls, 1)
	require.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, servedModelName(pm), runtime.deactivateCalls[0].ModelName)

	gotFailedPod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: failedPod.Name,
	}, gotFailedPod))
	assert.NotContains(t, gotFailedPod.Annotations, constants.ModelClaimPodAnnotationPrefix+pm.Name)
	assert.Contains(t, gotFailedPod.Annotations, constants.ModelClaimPodAnnotationPrefix+peer.Name)
	unchangedPeer := getModel(t, r, peer.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, unchangedPeer.Status.Phase)
	assert.Equal(t, "warm-1", unchangedPeer.Status.Instances[0].Pod)

	gotReplacementPod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: replacementPod.Name,
	}, gotReplacementPod))
	assert.Contains(t, gotReplacementPod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)

	reconcileOnce(t, r, pm.Name)

	active := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, active.Status.Phase)
	assert.Equal(t, int32(1), active.Status.ReadyReplicas)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace, Name: replacementPod.Name,
	}, gotReplacementPod))
	assert.Contains(t, gotReplacementPod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name],
		fmt.Sprintf(`"port":%d`, active.Status.Instances[0].Port))
}

func TestReconcileSnapshotTerminalFailureKeepsFailedPodsExcluded(t *testing.T) {
	replicas := int32(2)
	pm := withFinalizer(sampleModelClaim())
	pm.UID = types.UID("claim-uid")
	pm.Spec.Replicas = &replicas
	pm.Status.Phase = modelv1alpha1.ModelClaimActive
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
		{Pod: "warm-2", Port: 9002, Phase: modelv1alpha1.ModelClaimActive},
	}
	failedA := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	failedB := warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning)
	failedB.Status.PodIP = testPeerIP
	replacementA := warmPod("warm-3", "b300-pool-a", true, corev1.PodRunning)
	replacementA.Status.PodIP = "10.0.0.3"
	replacementB := warmPod("warm-4", "b300-pool-a", true, corev1.PodRunning)
	replacementB.Status.PodIP = "10.0.0.4"
	r, runtime := newReconciler(t, pm, failedA, failedB, replacementA, replacementB)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedA.Status.PodIP: {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseFailed,
			Alive: false, Ready: false, LastError: "restart budget exhausted",
			ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
		}}},
		failedB.Status.PodIP: {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 9002, Phase: runtimePhaseFailed,
			Alive: false, Ready: false, LastError: "restart budget exhausted",
			ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
		}}},
		replacementA.Status.PodIP: {},
		replacementB.Status.PodIP: {},
	}

	reconcileOnce(t, r, pm.Name)

	rescheduled := getModel(t, r, pm.Name)
	require.Len(t, rescheduled.Status.Instances, 2)
	gotPods := []string{rescheduled.Status.Instances[0].Pod, rescheduled.Status.Instances[1].Pod}
	assert.ElementsMatch(t, []string{"warm-3", "warm-4"}, gotPods)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, rescheduled.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, rescheduled.Status.Instances[1].Phase)
	require.Len(t, runtime.activateCalls, 2)
	require.Len(t, runtime.deactivateCalls, 2)
}

func TestReconcileReplacesAFailedEngineWhereTheAccountShowsRoom(t *testing.T) {
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimActive
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	failedPod, failedSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	failedSnapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseFailed,
		LastError: "restart budget exhausted",
		ClaimRef:  &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	// warm-2 shows its whole card free. A claim placed there a moment ago has
	// not loaded yet, and the card is promised to it.
	promised, promisedSnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	loading := claimOnPod("loading", promised.Name, modelv1alpha1.ModelClaimActivating, 700, 100)
	// warm-3 shows less free, and has room.
	roomy, roomySnapshot := sizedWarmPod("warm-3", "10.0.0.3", 1000)
	roomySnapshot.Accelerators[0].HBMFreeBytes = 800
	roomySnapshot.Models = []RuntimeSnapshotModel{engineHolding("small", 50, 300)}
	small := claimOnPod("small", roomy.Name, modelv1alpha1.ModelClaimActive, 100, 100)
	r, runtime := newReconciler(t, pm, loading, small, failedPod, promised, roomy)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedPod.Status.PodIP: failedSnapshot,
		promised.Status.PodIP:  promisedSnapshot,
		roomy.Status.PodIP:     roomySnapshot,
	}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-3", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	// Footprints of 100 and 300 and a floor of 100 each leave 400 of the card,
	// shared evenly between the two engines.
	assert.Equal(t, int64(300), got.Status.Instances[0].KVLimitBytes)
	require.Len(t, runtime.activateCalls, 1)
	require.Len(t, runtime.deactivateCalls, 1)
}

func TestReconcileKeepsTheFailedInstanceWhenItsReplacementIsRefused(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimActive
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	failedPod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	replacementPod := warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning)
	replacementPod.Status.PodIP = testPeerIP
	r, runtime := newReconciler(t, pm, failedPod, replacementPod)
	runtime.failActivate = true
	runtime.snapshots = map[string]*RuntimeSnapshot{
		failedPod.Status.PodIP: {Models: []RuntimeSnapshotModel{{
			ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseFailed,
			LastError: "restart budget exhausted",
			ClaimRef:  &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
		}}},
		testPeerIP: {},
	}

	reconcileOnce(t, r, pm.Name)

	// The replacement did not start, so the failed instance stays. The claim
	// still reads as failed, and the next pass leaves warm-1 out again.
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	require.Len(t, runtime.activateCalls, 1)
	said := ""
	for _, event := range recordedEvents(t, r) {
		if strings.Contains(event, "RescheduleFailed") {
			said = event
		}
	}
	assert.Contains(t, said, "warm-2")
}

func TestSnapshotModelForClaimPrefersMatchingClaimUID(t *testing.T) {
	pm := sampleModelClaim()
	pm.UID = types.UID("claim-uid")
	snapshot := &RuntimeSnapshot{Models: []RuntimeSnapshotModel{
		{
			ModelName: servedModelName(pm),
			Port:      9001,
			ClaimRef: &ModelClaimRef{
				Namespace: testNamespace, Name: "other", UID: "other-uid",
			},
		},
		{
			ModelName: "renamed-at-runtime",
			Port:      9100,
			ClaimRef: &ModelClaimRef{
				Namespace: testNamespace, Name: pm.Name, UID: string(pm.UID),
			},
		},
	}}

	observed := snapshotModelForClaim(snapshot, pm, servedModelName(pm))

	require.NotNil(t, observed)
	assert.Equal(t, int32(9100), observed.Port)
}

func TestReconcileSleepingInstanceRemovesRouteAndWakeRestoresIt(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)

	reconcileOnce(t, r, pm.Name)
	active := getModel(t, r, pm.Name)
	require.Len(t, active.Status.Instances, 1)
	port := active.Status.Instances[0].Port
	runtime.models[servedModelName(pm)] = ModelInfo{
		ModelName: servedModelName(pm),
		Port:      port,
		Phase:     "sleeping",
		Ready:     false,
	}

	reconcileOnce(t, r, pm.Name)

	sleeping := getModel(t, r, pm.Name)
	require.Len(t, sleeping.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, sleeping.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, sleeping.Status.Phase)
	assert.Equal(t, int32(0), sleeping.Status.ReadyReplicas)
	condition := meta.FindStatusCondition(sleeping.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "EngineSleeping", condition.Reason)
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"], `"port":0`,
		"sleeping engine must be non-routable")
	assert.Len(t, runtime.activateCalls, 1, "sleep must keep its assigned instance")

	runtime.models[servedModelName(pm)] = ModelInfo{
		ModelName: servedModelName(pm),
		Port:      port,
		Phase:     "active",
		Ready:     false,
	}
	runtime.notReady = true
	reconcileOnce(t, r, pm.Name)

	waking := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, waking.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, waking.Status.Phase)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"], `"port":0`,
		"woken but unready engine must remain non-routable")

	runtime.notReady = false
	reconcileOnce(t, r, pm.Name)

	woken := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, woken.Status.Instances[0].Phase)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, woken.Status.Phase)
	assert.Equal(t, int32(1), woken.Status.ReadyReplicas)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, pod))
	assert.Contains(t, pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"],
		fmt.Sprintf(`"port":%d`, port), "woken and ready engine must regain its route")
	assert.Len(t, runtime.activateCalls, 1, "wake must not launch another engine")
}

// TestReconcileNoCandidatesStaysPending verifies that with no warm pods the
// model neither activates nor errors; it stays Pending.
func TestReconcileNoCandidatesStaysPending(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t, pm)

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimPending, got.Status.Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas)
	// The claim declares its cost, and still nobody is told to look at GPU
	// memory: no pod matched at all.
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "NoMatchingPods", cond.Reason)
	assert.NotContains(t, cond.Message, "GiB")
}

// TestReconcileIdempotent verifies an already-satisfied model is not
// re-activated.
func TestReconcileIdempotent(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	)
	runtime.models = map[string]ModelInfo{
		servedModelName(pm): {ModelName: servedModelName(pm), Port: 9001, Phase: "active", Ready: true},
	}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls, "desired already met, no new activation")
	got := getModel(t, r, pm.Name)
	assert.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Phase)
}

// TestReconcileActivateFailureSetsFailed verifies a runtime failure surfaces as
// the Failed phase and a false Ready condition.
func TestReconcileActivateFailureSetsFailed(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)
	runtime.failActivate = true

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestReconcileInvalidEngineConfigSetsFailed(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.EngineConfig.Args["--tensor-parallel-size"] = "invalid"
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	})
	require.NoError(t, err)
	assert.False(t, result.Requeue)
	assert.Zero(t, result.RequeueAfter)
	assert.Empty(t, runtime.activateCalls)

	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "InvalidEngineConfig", cond.Reason)
	assert.Contains(t, cond.Message, "--tensor-parallel-size must be a positive integer")
}

func TestReconcileRejectsGPUMemoryUtilizationWithKVCached(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.EngineConfig.Args["--gpu-memory-utilization"] = "0.45"
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	})

	require.NoError(t, err)
	assert.False(t, result.Requeue)
	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Phase)
	condition := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, condition)
	assert.Equal(t, "InvalidEngineConfig", condition.Reason)
	assert.Contains(t, condition.Message, "--gpu-memory-utilization is incompatible with kvcached")
}

// TestReconcileAnnotatesWarmPodForRouting verifies activation stamps the
// served-model -> port routing annotation onto the warm pod.
func TestReconcileAnnotatesWarmPodForRouting(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	r, _ := newReconciler(t, pm, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	inst := got.Status.Instances[0]

	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: inst.Pod}, pod))
	val := pod.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name]
	assert.Contains(t, val, `"model":"qwen2-7b"`)
	assert.Contains(t, val, fmt.Sprintf(`"port":%d`, inst.Port))
}

// TestReconcileDeletionRemovesRoutingAnnotation verifies deletion removes the
// routing annotation so the gateway stops routing to the pod.
func TestReconcileDeletionRemovesRoutingAnnotation(t *testing.T) {
	now := metav1.Now()
	pm := withFinalizer(sampleModelClaim())
	pm.DeletionTimestamp = &now
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + pm.Name: `{"model":"qwen2-7b","port":9001}`,
	}
	r, _ := newReconciler(t, pm, pod)

	reconcileOnce(t, r, pm.Name)

	got := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "warm-1"}, got))
	_, ok := got.Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name]
	assert.False(t, ok, "routing annotation should be removed on delete")
}

// TestReconcileDeletionDeactivates verifies deletion stops instances and drops
// the finalizer.
func TestReconcileDeletionDeactivates(t *testing.T) {
	now := metav1.Now()
	pm := withFinalizer(sampleModelClaim())
	pm.DeletionTimestamp = &now
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	r, runtime := newReconciler(t,
		pm,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
	)

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, DeactivateStop, runtime.deactivateCalls[0].Mode)

	got := &modelv1alpha1.ModelClaim{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: pm.Name}, got)
	assert.True(t, err != nil || !controllerutil.ContainsFinalizer(got, ModelClaimFinalizer))
}

// claimWithCost is the sample claim plus a declared per-GPU cost.
func claimWithCost(footprint, floor int64) *modelv1alpha1.ModelClaim {
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.PerGPU = &modelv1alpha1.ModelClaimPerGPU{
		MaximumFootprint: *resource.NewQuantity(footprint, resource.BinarySI),
		KVFloor:          *resource.NewQuantity(floor, resource.BinarySI),
	}
	return pm
}

// sizedWarmPod is a warm pod with one card the runtime could measure.
func sizedWarmPod(name, ip string, hbmUsableBytes int64) (*corev1.Pod, *RuntimeSnapshot) {
	pod := warmPodWithGPUs(name, "b300-pool-a", 1)
	pod.Status.PodIP = ip
	return pod, &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: hbmUsableBytes, HBMUsableBytes: hbmUsableBytes},
		},
	}
}

func TestReconcileRefusesACardWithoutRoomForTheDeclaredCost(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NoMatchingPods", cond.Reason)
	assert.Contains(t, cond.Message, "warm-1 can offer at most")
}

func TestReconcileStopsSayingNoCardWillTakeItOnceOneDoes(t *testing.T) {
	pm := claimWithCost(700, 100)
	small, smallSnapshot := sizedWarmPod("warm-small", "10.0.0.1", 500)
	roomy, roomySnapshot := sizedWarmPod("warm-roomy", "10.0.0.2", 2000)
	r, runtime := newReconciler(t, pm, small)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		small.Status.PodIP: smallSnapshot,
		roomy.Status.PodIP: roomySnapshot,
	}

	reconcileOnce(t, r, pm.Name)

	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)

	// A card with room joins the pool. The earlier refusal must not be left
	// standing as the claim's answer about finding one.
	require.NoError(t, r.Create(context.Background(), roomy))
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	cond = meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Placed", cond.Reason)
	assert.Contains(t, cond.Message, roomy.Name)
}

func TestReconcileRaisesNoMatchingPodsOnlyWhenTheRefusalChanges(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	// Tried on three passes, and turned away the same way on each.
	reconcileOnce(t, r, pm.Name)
	reconcileOnce(t, r, pm.Name)
	reconcileOnce(t, r, pm.Name)

	refusals := 0
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, "NoMatchingPods") {
			refusals++
		}
	}
	assert.Equal(t, 1, refusals, "the same refusal three times over is one Event")
	got := getModel(t, r, pm.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "NoMatchingPods", cond.Reason)
}

func TestReconcileRefusesACardWhoseRoomIsHeldByTheEnginesOnIt(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	// The card could hold this model once the neighbour is back at its floor,
	// and the neighbour has mapped 500.
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 500, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "held by the engines already on it")
}

func TestReconcileRefusesACardRunningAnEngineNoClaimAnswersFor(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	// The card has room on paper. It is also running an engine that no
	// recorded instance answers for, so nobody knows what that engine holds.
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: "stranger", Port: 9001, Phase: "active", Alive: true, Ready: true,
	}}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "answers to no claim")
}

func TestReconcilePlacesOnTheCardThatCanHoldTheDeclaredCost(t *testing.T) {
	pm := claimWithCost(700, 100)
	full, fullSnapshot := sizedWarmPod("warm-full", "10.0.0.1", 1000)
	roomy, roomySnapshot := sizedWarmPod("warm-roomy", "10.0.0.2", 2000)
	neighbour := claimOnPod("neighbour", full.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	r, runtime := newReconciler(t, pm, full, roomy, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		full.Status.PodIP:  fullSnapshot,
		roomy.Status.PodIP: roomySnapshot,
	}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-roomy", got.Status.Instances[0].Pod)
}

func TestReconcileWillNotPlaceOnACardItCannotMeasure(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	r, runtime := newReconciler(t, pm, pod)

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "could not be judged")
}

func TestReconcileWillNotPlaceOnAPodWhoseRuntimeDidNotAnswer(t *testing.T) {
	pm := claimWithCost(700, 100)
	// The pod requests no GPU, as one given its cards by a dynamic resource
	// claim does. Only its runtime could say whether it has cards, and the
	// runtime did not answer.
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Status.PodIP = "10.0.0.1"
	r, runtime := newReconciler(t, pm, pod)
	runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "its runtime did not answer")
}

func TestReconcileDoesNotPlaceAClaimThatDeclaresNoCost(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.PerGPU = nil
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	r, runtime := newReconciler(t, pm, pod)

	reconcileOnce(t, r, pm.Name)
	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions, string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "InvalidPerGPU", cond.Reason)
	assert.Contains(t, cond.Message, "spec.perGPU is missing")

	// Said once, not again on every pass while nothing changes.
	said := 0
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, "InvalidPerGPU") {
			said++
		}
	}
	assert.Equal(t, 1, said)
}

// No reading of a runtime can place a claim that does not say what it costs,
// so no runtime is read for it.
func TestReconcileReadsNoRuntimeForAClaimThatDeclaresNoCost(t *testing.T) {
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.PerGPU = nil
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Zero(t, runtime.snapshotCalls)
	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "InvalidPerGPU", cond.Reason)
}

func TestReconcileDoesNotPlaceAClaimThatDeclaresAZeroFloor(t *testing.T) {
	pm := claimWithCost(30<<30, 0)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "InvalidPerGPU", cond.Reason)
	assert.Contains(t, cond.Message, "spec.perGPU.kvFloor is 0, which is not positive")
}

// readyEngine is what a runtime reports for an engine that is serving, with
// the KV limit its allocator currently holds.
func readyEngine(kvCapacityBytes int64) RuntimeSnapshotModel {
	return RuntimeSnapshotModel{
		ModelName:       "qwen2-7b",
		Port:            9001,
		Phase:           "active",
		Alive:           true,
		Ready:           true,
		KVCapacityBytes: kvCapacityBytes,
	}
}

func TestArrangeCardGivesARetryItsOwnOperation(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 80<<30)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	// The engine runs under its allocator's own limit, well above its share.
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 2<<30, 76<<30)}
	snapshot.ObservedAt = time.Unix(1_700_000_000, 0)
	r, runtime := newReconciler(t, neighbour, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	arrange := func() {
		t.Helper()
		ledgers := r.collectPodLedgers(context.Background(), testNamespace,
			[]corev1.Pod{*pod}, map[string]*RuntimeSnapshot{pod.Name: snapshot})
		ledger := ledgers[pod.Name]
		require.True(t, ledger.judgeable)
		_, err := r.arrangeCard(context.Background(), pod, ledger, ledger.engines)
		require.NoError(t, err)
	}

	arrange()
	require.Len(t, runtime.kvLimitCalls, 1)

	// The engine restarts and puts its allocator's limit back. The plan is the
	// same as before, so only the moment it was planned from tells the runtime
	// this is a new attempt rather than the first one repeated.
	snapshot.Models[0].KVCapacityBytes = 76 << 30
	snapshot.ObservedAt = snapshot.ObservedAt.Add(DefaultRequeueDuration)
	arrange()

	require.Len(t, runtime.kvLimitCalls, 2)
	assert.Equal(t, runtime.kvLimitCalls[0].LimitBytes, runtime.kvLimitCalls[1].LimitBytes)
	assert.NotEqual(t, runtime.kvLimitCalls[0].OperationID, runtime.kvLimitCalls[1].OperationID)
}

func TestReconcileShrinksTheNeighbourToMakeRoomForANewModel(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	// The card holds two footprints of 300 and two floors of 100, leaving 200
	// to share evenly.
	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, "neighbour", runtime.kvLimitCalls[0].ModelName)
	assert.Equal(t, int64(200), runtime.kvLimitCalls[0].LimitBytes)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, int64(200), got.Status.Instances[0].KVLimitBytes)
	require.Len(t, runtime.activateCalls, 1)

	held := &modelv1alpha1.ModelClaim{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "neighbour"}, held))
	assert.Equal(t, int64(200), held.Status.Instances[0].KVLimitBytes)

	// The neighbour did not ask to be shrunk, so its own claim has to say it
	// was, rather than only the pod it happens to run on.
	said := ""
	for _, event := range recordedEvents(t, r) {
		if strings.Contains(event, "KVLimitSet") && strings.Contains(event, "neighbour") {
			said = event
		}
	}
	assert.NotEmpty(t, said, "the neighbour's claim records no KVLimitSet event")
}

// recordedEvents drains the fake recorder and returns what it was given.
func recordedEvents(t *testing.T, r *ModelClaimReconciler) []string {
	t.Helper()
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	require.True(t, ok)
	var seen []string
	for {
		select {
		case event := <-recorder.Events:
			seen = append(seen, event)
		default:
			return seen
		}
	}
}

func TestReconcileWillNotPlaceWhenTheNeighbourDoesNotTakeItsLimit(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "NoMatchingPods", cond.Reason)
	assert.Contains(t, cond.Message, "could not be divided")
	assert.Contains(t, cond.Message, "did not take a KV limit")
}

func TestReconcileLeavesTheRecordsAloneWhenACardCannotBeDivided(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The write reaches no segment, so the reading that should confirm it does
	// not, and the card is not divided.
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Empty(t, runtime.activateCalls)
	// The neighbour keeps the limit it was given, not the smaller one it was
	// never confirmed to hold. Recorded, the smaller one would be enforced by
	// the health loop for a model that was never placed.
	held := &modelv1alpha1.ModelClaim{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "neighbour"}, held))
	assert.Equal(t, int64(600), held.Status.Instances[0].KVLimitBytes)
}

// recordCheckingRuntime looks a claim up on the API server at the moment the
// runtime is asked to start its engine.
type recordCheckingRuntime struct {
	*fakeRuntime
	reader    client.Reader
	claim     types.NamespacedName
	instances []modelv1alpha1.ModelClaimInstance
}

func (f *recordCheckingRuntime) Activate(ctx context.Context, ip string, port int, req *ActivateRequest) (*ActivateResponse, error) {
	stored := &modelv1alpha1.ModelClaim{}
	if err := f.reader.Get(ctx, f.claim, stored); err == nil {
		f.instances = stored.Status.Instances
	}
	return f.fakeRuntime.Activate(ctx, ip, port, req)
}

func TestReconcileRecordsAnInstanceBeforeItsEngineIsStarted(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	checking := &recordCheckingRuntime{
		fakeRuntime: runtime, reader: r.Client,
		claim: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	}
	r.Runtime = checking

	reconcileOnce(t, r, pm.Name)

	// The record is what the account charges. Written after the start, it
	// would leave the card free to a second claim while the engine loads.
	require.Len(t, runtime.activateCalls, 1)
	require.Len(t, checking.instances, 1)
	assert.Equal(t, pod.Name, checking.instances[0].Pod)
	assert.Equal(t, int64(300), checking.instances[0].KVLimitBytes)
}

func TestReconcileRefusesASecondClaimWhileTheFirstStillLoads(t *testing.T) {
	first := claimWithCost(500, 100)
	first.Name = "first"
	second := claimWithCost(500, 100)
	second.Name = "second"
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, first, second, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The first engine has not come up, so it has mapped nothing yet.
	runtime.notReady = true

	reconcileOnce(t, r, first.Name)
	reconcileOnce(t, r, second.Name)

	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, "first", runtime.activateCalls[0].ModelName)
	assert.Empty(t, getModel(t, r, second.Name).Status.Instances)
}

func TestReconcileDoesNotTakeAConflictOnTheRecordForAFailedStart(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pm, pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object,
				_ ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
					obj.GetName(), fmt.Errorf("the object has been modified"))
			},
		}).
		Build()
	runtime := &fakeRuntime{snapshots: map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}}
	r := &ModelClaimReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32), Runtime: runtime,
		PoolPolicy:    newPoolPolicyManager(time.Now),
		SnapshotCache: newRuntimeSnapshotCache(defaultRuntimeSnapshotTTL, time.Now),
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	})

	// The claim was read a moment too early. No engine was asked for, so
	// nothing failed, and the next pass works from the claim as it is.
	require.NoError(t, err)
	assert.True(t, result.Requeue)
	assert.Empty(t, runtime.activateCalls)
	for _, event := range drainEvents(t, r) {
		assert.NotContains(t, event, "ActivateFailed")
	}
}

func TestReconcileRecordsNoMoreThanTheCardWhenARecordCannotBeWritten(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	grows := claimOnPod("a-grows", pod.Name, modelv1alpha1.ModelClaimActive, 100, 50)
	grows.Status.Instances[0].KVLimitBytes = 60
	shrinks := claimOnPod("c-shrinks", pod.Name, modelv1alpha1.ModelClaimActive, 200, 100)
	shrinks.Status.Instances[0].KVLimitBytes = 500
	newcomer := claimWithCost(100, 50)
	newcomer.Name = "b-newcomer"
	// The busy engine is to grow, and the quiet one to shrink.
	busy := engineHolding("a-grows", 50, 60)
	busy.RequestsRunning = 4
	snapshot.Models = []RuntimeSnapshotModel{busy, engineHolding("c-shrinks", 100, 500)}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grows, shrinks, newcomer, pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if obj.GetName() == "c-shrinks" {
					return fmt.Errorf("etcdserver: request timed out")
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	runtime := &fakeRuntime{snapshots: map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}}
	r := &ModelClaimReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32), Runtime: runtime,
		PoolPolicy:    newPoolPolicyManager(time.Now),
		SnapshotCache: newRuntimeSnapshotCache(defaultRuntimeSnapshotTTL, time.Now),
	}

	reconcileOnce(t, r, newcomer.Name)

	require.Empty(t, runtime.activateCalls, "the card was not divided, so nothing is placed")
	// A record is what an engine is raised to when it next comes up. So the
	// records that go down are written first: written part way, the records
	// still come to no more than the card.
	promised := int64(0)
	for name, footprint := range map[string]int64{"a-grows": 100, "c-shrinks": 200} {
		promised += footprint + getModel(t, r, name).Status.Instances[0].KVLimitBytes
	}
	assert.LessOrEqual(t, promised, int64(1000))
	assert.Equal(t, int64(60), getModel(t, r, "a-grows").Status.Instances[0].KVLimitBytes)
}

func TestRecordKVLimitTriesAgainAfterAConflict(t *testing.T) {
	pod, _ := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	scheme := testScheme(t)
	// The neighbour's own reconcile wrote its status just before, so the
	// first write of the new limit loses the race.
	conflicts, updates := 1, 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(neighbour, pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				updates++
				if conflicts > 0 {
					conflicts--
					return apierrors.NewConflict(schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
						obj.GetName(), fmt.Errorf("the object has been modified"))
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &ModelClaimReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32), Runtime: &fakeRuntime{}}

	claim, err := r.recordKVLimit(context.Background(), testNamespace, "neighbour", pod.Name, 200)

	require.NoError(t, err)
	assert.Equal(t, 2, updates, "a conflict should be tried again, from a fresh read")
	assert.Equal(t, int64(200), claim.Status.Instances[0].KVLimitBytes)
	stored := &modelv1alpha1.ModelClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "neighbour"}, stored))
	assert.Equal(t, int64(200), stored.Status.Instances[0].KVLimitBytes)
}

// activatingWithoutEngine is a claim whose instance was recorded on a card
// that was divided for it, and whose engine the runtime never started: the
// controller stopped between the two, or a failed start was never taken back
// from the record.
func activatingWithoutEngine(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *corev1.Pod) {
	t.Helper()
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 600},
	}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, pod
}

func TestReconcileStartsAgainAnActivatingInstanceWithNoEngine(t *testing.T) {
	r, runtime, pm, _ := activatingWithoutEngine(t)

	reconcileOnce(t, r, pm.Name)

	// Placement does not run again, since the claim has its instance, so this
	// is the only thing that would ever start the engine the card holds room
	// for.
	require.Len(t, runtime.activateCalls, 1)
	assert.Equal(t, servedModelName(pm), runtime.activateCalls[0].ModelName)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.NotZero(t, got.Status.Instances[0].Port)
	assert.Equal(t, int64(600), got.Status.Instances[0].KVLimitBytes)
}

func TestReconcileGivesTheCardBackWhenAnEngineCannotBeStartedAgain(t *testing.T) {
	r, runtime, pm, _ := activatingWithoutEngine(t)
	runtime.failActivate = true

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances, "an instance with no engine and no way to start one must not keep its room")
	failed := false
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, "ActivateFailed") && strings.Contains(event, "had no engine") {
			failed = true
		}
	}
	assert.True(t, failed, "dropping the instance should be reported")
}

func TestReconcileKeepsTheRecordWhenTheAnswerToAStartIsLost(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The runtime starts the engine, and its answer never arrives.
	runtime.loseActivateAnswer = true

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1, "the engine may have started, so its record stays")
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	placed := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, placed)
	assert.Equal(t, "Placed", placed.Reason)

	// The next pass finds the engine, and goes on with it. Without the record
	// the engine would answer to no claim, and its card would be out of use.
	runtime.loseActivateAnswer = false
	reconcileOnce(t, r, pm.Name)

	assert.Len(t, runtime.activateCalls, 1, "the engine is there, so it is not started again")
	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, pod.Name, got.Status.Instances[0].Pod)
	assert.NotZero(t, got.Status.Instances[0].Port)
}

func TestReconcileKeepsAnInstanceWhenTheAnswerToStartingItAgainIsLost(t *testing.T) {
	r, runtime, pm, _ := activatingWithoutEngine(t)
	runtime.loseActivateAnswer = true

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1, "the engine may have started, so its record stays")
}

func TestCallNotDone(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for name, c := range map[string]struct {
		err     error
		notDone bool
	}{
		"a refusal":                   {&runtimeRefusal{"runtime POST /x returned 400: boom"}, true},
		"a wrapped error status":      {fmt.Errorf("start: %w", &runtimeRefusal{"boom"}), true},
		"never connected":             {&url.Error{Op: "Post", URL: activatePath, Err: refused}, true},
		"an address nobody parsed":    {&url.Error{Op: "parse", URL: "http://[", Err: errors.New("missing ']'")}, true},
		"no answer in time":           {&url.Error{Op: "Post", URL: activatePath, Err: context.DeadlineExceeded}, false},
		"connection dropped":          {&url.Error{Op: "Post", URL: activatePath, Err: io.ErrUnexpectedEOF}, false},
		"an answer nobody could read": {errors.New("decode runtime response: unexpected end of JSON input"), false},
	} {
		assert.Equal(t, c.notDone, callNotDone(c.err), name)
	}
}

func TestReconcileStartsNoEngineWhenTheRuntimeCannotBeRead(t *testing.T) {
	r, runtime, pm, pod := activatingWithoutEngine(t)
	// A runtime that cannot be read says nothing about the engine.
	runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
}

// podWithUnrequestedGPU is a warm pod that asks for no nvidia.com/gpu, while
// its runtime reports a card, as with a GPU given by a dynamic resource claim.
// Like any card the runtime has read, it comes with its total memory.
func podWithUnrequestedGPU(hbmUsableBytes int64) (*corev1.Pod, *RuntimeSnapshot) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Status.PodIP = "10.0.0.1"
	return pod, &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMTotalBytes: hbmUsableBytes, HBMFreeBytes: hbmUsableBytes, HBMUsableBytes: hbmUsableBytes},
		},
	}
}

func TestReconcileAccountsForACardTheRuntimeReportsWithoutAGPURequest(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(300)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	// The card is too small for the model. Taken for a pod with no GPU, it
	// would have been used without an account.
	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Equal(t, "NoMatchingPods", cond.Reason)
	assert.Contains(t, cond.Message, "can offer at most")
}

func TestReconcileHoldsAnEngineToItsLimitOnAPodWithoutAGPURequest(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(1000)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, Port: 9001, KVLimitBytes: 600},
	}
	// Ready, and still under its allocator's own limit.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(900)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(600), runtime.kvLimitCalls[0].LimitBytes)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase,
		"an engine above its limit must not be routed")
}

func TestReconcileWillNotPlaceOnAPodWhoseCardCouldNotBeReadThisTime(t *testing.T) {
	pm := claimWithCost(300, 100)
	// The pod requests no GPU, and this reading of its runtime reports no card,
	// as when NVML fails once. Its engine holds a KV segment all the same, so
	// the pod has a card, and the card is full.
	pod, snapshot := podWithUnrequestedGPU(1000)
	snapshot.Accelerators = nil
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 500, 500)}
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 400, 100)
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "its cards could not be measured")
}

func TestReconcileWillNotPlaceBesideARecordedLimitWhenNothingShowsTheCard(t *testing.T) {
	pm := claimWithCost(300, 100)
	// The pod requests no GPU, and this reading of its runtime reports no
	// card. The neighbour was placed a moment ago and is still loading, so no
	// engine holds a KV segment either. Its instance records a limit, and a
	// limit is recorded only where a card was divided. So the pod has a card,
	// and the card is promised 900 of its 1000.
	pod, snapshot := podWithUnrequestedGPU(1000)
	snapshot.Accelerators = nil
	loading := engineHolding("neighbour", kvLimitUnknown, kvLimitUnknown)
	loading.Phase = "booting"
	loading.Ready = false
	snapshot.Models = []RuntimeSnapshotModel{loading}
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActivating, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "its cards could not be measured")
}

// The rule holds for an instance in any phase. A failed one is charged
// nothing, and its limit still says that the pod has a card.
func TestReconcileCountsACardWhereAFailedInstanceRecordsALimit(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(1000)
	snapshot.Accelerators = nil
	failed := claimOnPod("failed", pod.Name, modelv1alpha1.ModelClaimFailed, 300, 100)
	failed.Status.Instances[0].KVLimitBytes = 600
	r, runtime := newReconciler(t, pm, pod, failed)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "its cards could not be measured")
}

// An instance that records no limit was placed where no card was divided. It
// does not make its pod one with a card.
func TestReconcilePlacesBesideAnInstanceThatRecordsNoLimitOnAPodWithoutACard(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(1000)
	snapshot.Accelerators = nil
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].Port = 9001
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", kvLimitUnknown, kvLimitUnknown)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	placed := getModel(t, r, pm.Name).Status.Instances
	require.Len(t, placed, 1)
	assert.Zero(t, placed[0].KVLimitBytes, "nothing was divided, so no limit is recorded")
}

func TestReconcileHoldsAnEngineToItsLimitWhenItsCardCouldNotBeReadThisTime(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(1000)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, Port: 9001, KVLimitBytes: 600},
	}
	snapshot.Accelerators = nil
	// Ready, and still under its allocator's own limit.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(900)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase,
		"an engine above its limit must not be routed")
}

func TestReconcileKeepsAnEngineOffTheRouteWhileNothingShowsItsLimit(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := podWithUnrequestedGPU(1000)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, Port: 9001, KVLimitBytes: 600},
	}
	// This reading reports no card, and the engine's segment cannot be read.
	// The instance records a limit all the same, so the engine has one to hold.
	snapshot.Accelerators = nil
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(kvLimitUnknown)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase,
		"an engine not known to hold its limit must not be routed")
}

func TestReconcileRecordsNoLimitOnAPodWithoutACard(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Status.PodIP = "10.0.0.1"
	r, runtime := newReconciler(t, pm, pod)

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	// No card was divided, so there is no limit to record, and the pool
	// policy does not stand down on this pod.
	assert.Zero(t, got.Status.Instances[0].KVLimitBytes)
	assert.False(t, r.claimHoldsAKVLimitOn(context.Background(), pod))
}

// The pool policy runs at the end of a pass, right after the pass may have
// recorded the first limit on a pod. The cache may not show that record yet,
// so the policy reads the claims from the API server before it writes a limit.
func TestPoolKVPolicyStandsDownForALimitTheCacheHasNotSeen(t *testing.T) {
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	cached := claimOnPod("a", pod.Name, modelv1alpha1.ModelClaimActivating, 300, 100)
	cached.Status.Instances[0].KVLimitBytes = 0
	recorded := cached.DeepCopy()
	recorded.Status.Instances[0].KVLimitBytes = 600
	r, _ := newReconciler(t, cached, pod)
	r.APIReader = fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(recorded, pod.DeepCopy()).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		Build()

	assert.True(t, r.claimHoldsAKVLimitOn(context.Background(), pod))
}

// The runtime's mock mode reports one card with no memory at all, so that the
// single-GPU pool policy runs on the CPU pools of the end-to-end tests. That is
// no card to account for: the claim is placed as on a pod without a GPU, and
// its engine is routed once it is ready.
func TestReconcilePlacesAndRoutesOnAPodWhoseRuntimeReportsAnUnsizedCard(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	pod.Status.PodIP = "10.0.0.1"
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{{ID: "mock-gpu-0"}},
	}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	assert.Empty(t, runtime.kvLimitCalls, "a card with no size is not divided")
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, pod.Name, got.Status.Instances[0].Pod)

	// The engine is ready. Like the mock's, it has no KV allocator to read.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(-1)}
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
}

func TestReconcileTriesTheNextPodWhenACardCannotBeDivided(t *testing.T) {
	pm := claimWithCost(20<<30, 4<<30)
	// warm-1 is the roomier card and is tried first, but the engine on it will
	// not take the smaller limit that would make the room.
	roomy, roomySnapshot := sizedWarmPod("warm-1", "10.0.0.1", 90<<30)
	neighbour := claimOnPod("neighbour", roomy.Name, modelv1alpha1.ModelClaimActive, 20<<30, 4<<30)
	roomySnapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 2<<30, 76<<30)}
	other, otherSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 60<<30)
	r, runtime := newReconciler(t, pm, neighbour, roomy, other)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		roomy.Status.PodIP: roomySnapshot,
		other.Status.PodIP: otherSnapshot,
	}
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	failed := false
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, "KVLimitFailed") && strings.Contains(event, "warm-1") {
			failed = true
		}
	}
	assert.True(t, failed, "the card that could not be divided should still be reported")
}

func TestReconcileWillNotPlaceWhenANeighbourHasOutgrownItsNewLimit(t *testing.T) {
	pm := claimWithCost(300, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	// The account was built when the neighbour held 100, and by the time the
	// limit is read back it has mapped 250.
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	runtime.onKVLimit = func() { snapshot.Models[0].KVUsedBytes = 250 }

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	cond := meta.FindStatusCondition(got.Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "past the")
}

func TestReconcileHoldsAnEngineToItsLimitBeforeRouting(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	// Alone on a card of 1000, the model keeps its floor of 100 and the 200
	// left over once its footprint is paid for.
	assert.Equal(t, int64(300), got.Status.Instances[0].KVLimitBytes)

	// The engine comes up under its allocator's own limit.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(300), runtime.kvLimitCalls[0].LimitBytes)
	got = getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas)

	// The write lands, and the engine is routable on the next pass.
	snapshot.Models[0].KVCapacityBytes = 300
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
	assert.Equal(t, int32(1), got.Status.ReadyReplicas)
}

// An engine whose segment cannot be read is not known to be held to anything,
// so it does not keep its route.
func TestReconcileDeroutesAnEngineWhoseLimitCannotBeRead(t *testing.T) {
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod:          "warm-1",
		Port:         9001,
		Phase:        modelv1alpha1.ModelClaimActive,
		KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(kvLimitUnknown)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Empty(t, runtime.kvLimitCalls, "there is no segment to write into")
}

// Through a whole pass: one card that could never hold the model and offers
// more, and one that could hold it once its engine gives memory back.
func TestReconcileNamesTheCardWorthWaitingFor(t *testing.T) {
	pm := claimWithCost(300, 100)
	never, neverSnapshot := sizedWarmPod("a-never", "10.0.0.1", 1000)
	later, laterSnapshot := sizedWarmPod("b-later", "10.0.0.2", 1000)
	full := claimOnPod("full", never.Name, modelv1alpha1.ModelClaimActive, 600, 100)
	full.Status.Instances[0].KVLimitBytes = 400
	busy := claimOnPod("busy", later.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	busy.Status.Instances[0].KVLimitBytes = 700
	neverSnapshot.Models = []RuntimeSnapshotModel{engineHolding("full", 100, 400)}
	laterSnapshot.Models = []RuntimeSnapshotModel{engineHolding("busy", 500, 700)}
	r, runtime := newReconciler(t, pm, never, later, full, busy)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		never.Status.PodIP: neverSnapshot, later.Status.PodIP: laterSnapshot,
	}

	reconcileOnce(t, r, pm.Name)

	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "b-later has")
}

// A card that had room and could not be divided is worth waiting for as well.
// It is named before a card whose room is held, since it offers more. A card
// that never could hold the model would come last whatever this one counts
// as, so it could not show the rule.
func TestReconcileNamesTheCardThatCouldNotBeDividedBeforeOneWhoseRoomIsHeld(t *testing.T) {
	pm := claimWithCost(300, 100)
	held, heldSnapshot := sizedWarmPod("a-held", "10.0.0.1", 1000)
	room, roomSnapshot := sizedWarmPod("b-room", "10.0.0.2", 1000)
	busy := claimOnPod("busy", held.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	busy.Status.Instances[0].KVLimitBytes = 700
	deaf := claimOnPod("deaf", room.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	deaf.Status.Instances[0].KVLimitBytes = 700
	heldSnapshot.Models = []RuntimeSnapshotModel{engineHolding("busy", 500, 700)}
	roomSnapshot.Models = []RuntimeSnapshotModel{engineHolding("deaf", 100, 700)}
	r, runtime := newReconciler(t, pm, held, room, busy, deaf)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		held.Status.PodIP: heldSnapshot, room.Status.PodIP: roomSnapshot,
	}
	// The engine on the card with room does not take its new limit.
	runtime.deafToKVLimits = true

	reconcileOnce(t, r, pm.Name)

	require.Empty(t, runtime.activateCalls)
	cond := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions,
		string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "b-room has room, but its card could not be divided")
}

// The engine whose record goes down runs below that record already, as after
// a division that was not carried through. Its limit then goes up while its
// record goes down. What is written first is decided by the records, and not
// by the limits in force.
func TestReconcileOrdersTheRecordsByTheRecordsAndNotByTheLimitsInForce(t *testing.T) {
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	grows := claimOnPod("a-grows", pod.Name, modelv1alpha1.ModelClaimActive, 100, 50)
	grows.Status.Instances[0].KVLimitBytes = 60
	shrinks := claimOnPod("c-shrinks", pod.Name, modelv1alpha1.ModelClaimActive, 200, 100)
	shrinks.Status.Instances[0].KVLimitBytes = 500
	newcomer := claimWithCost(100, 50)
	newcomer.Name = "b-newcomer"
	busy := engineHolding("a-grows", 50, 60)
	busy.RequestsRunning = 4
	// Held to 120, below its record of 500.
	snapshot.Models = []RuntimeSnapshotModel{busy, engineHolding("c-shrinks", 100, 120)}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grows, shrinks, newcomer, pod).
		WithStatusSubresource(&modelv1alpha1.ModelClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if obj.GetName() == "c-shrinks" {
					return fmt.Errorf("etcdserver: request timed out")
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	runtime := &fakeRuntime{snapshots: map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}}
	r := &ModelClaimReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32), Runtime: runtime,
		PoolPolicy:    newPoolPolicyManager(time.Now),
		SnapshotCache: newRuntimeSnapshotCache(defaultRuntimeSnapshotTTL, time.Now),
	}

	reconcileOnce(t, r, newcomer.Name)

	require.Empty(t, runtime.activateCalls)
	promised := int64(0)
	for name, footprint := range map[string]int64{"a-grows": 100, "c-shrinks": 200} {
		promised += footprint + getModel(t, r, name).Status.Instances[0].KVLimitBytes
	}
	assert.LessOrEqual(t, promised, int64(1000))
}

func TestReconcileDeroutesAnEngineHeldToMoreThanItsRecord(t *testing.T) {
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod:          "warm-1",
		Port:         9001,
		Phase:        modelv1alpha1.ModelClaimActive,
		KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	// The engine restarted and its allocator put the whole pool back, so it
	// could grow into memory the card holds for its neighbours.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(300), runtime.kvLimitCalls[0].LimitBytes)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas)
	said := false
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, "KVLimitNotHeld") {
			said = true
		}
	}
	assert.True(t, said, "the event should blame the limit, not the engine")

	// The write lands, and the route comes back on the next pass.
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
}

func TestReconcileKeepsAnEngineRoutableWhileALargerLimitIsPending(t *testing.T) {
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod:          "warm-1",
		Port:         9001,
		Phase:        modelv1alpha1.ModelClaimActive,
		KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	// Held to less than its record: a larger share was recorded for it and has
	// not been written yet.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(200)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	// Growing it is the card's division to do, in its own order.
	assert.Empty(t, runtime.kvLimitCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
}

func TestReconcileGrowsANewEngineToItsRecordBeforeRouting(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)
	require.Equal(t, int64(300), getModel(t, r, pm.Name).Status.Instances[0].KVLimitBytes)

	// The engine comes up under an allocator default smaller than the room it
	// was given. That room was made for it when the card was divided, so it is
	// raised to its record before it takes any traffic.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(200)}
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, int64(300), runtime.kvLimitCalls[0].LimitBytes)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)

	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
}

func TestReconcileWritesNoLimitIntoAnEngineWithoutASegment(t *testing.T) {
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	snapshot.Models = []RuntimeSnapshotModel{readyEngine(-1)}
	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.kvLimitCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
}

func TestReconcileStillRoutesAnInstanceWhoseClaimDeclaresNoCost(t *testing.T) {
	// A claim stored before the declaration existed can already have an engine
	// running. The claim is not placed again, but the engine it has keeps its
	// route, and there is no limit to hold it to.
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.PerGPU = nil
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating},
	}
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	reconcileOnce(t, r, pm.Name)

	assert.Empty(t, runtime.activateCalls)
	assert.Empty(t, runtime.kvLimitCalls)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)
	assert.Zero(t, got.Status.Instances[0].KVLimitBytes)
}
