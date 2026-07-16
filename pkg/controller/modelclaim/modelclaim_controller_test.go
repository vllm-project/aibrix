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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	testNamespace = "default"
	testPeerIP    = "10.0.0.2"
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
}

func (f *fakeRuntime) Activate(_ context.Context, _ string, _ int, req *ActivateRequest) (*ActivateResponse, error) {
	f.activateCalls = append(f.activateCalls, *req)
	if f.failActivate {
		return &ActivateResponse{Status: "error", Message: "boom"}, fmt.Errorf("activate failed: boom")
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
	return &ActivateResponse{Status: "success", ModelName: req.ModelName, Port: port, IPCName: req.IPCName}, nil
}

func (f *fakeRuntime) Deactivate(_ context.Context, _ string, _ int, req *DeactivateRequest) error {
	f.deactivateCalls = append(f.deactivateCalls, *req)
	delete(f.models, req.ModelName)
	return nil
}

func (f *fakeRuntime) SetKVLimit(_ context.Context, _ string, _ int, req *SetKVLimitRequest) (*RuntimeOperationResponse, error) {
	f.kvLimitCalls = append(f.kvLimitCalls, *req)
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
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "warm"}},
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
	pod.Labels["app"] = "warm"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}

	r, runtime := newReconciler(t, deployment, replicaSet, pod)
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{
				{
					ModelName: "hot", Phase: "active", Alive: true, Ready: true,
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
	}
	assert.Equal(t, int64(800), limits["hot"])
	assert.Equal(t, int64(200), limits["idle"])
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
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "warm"}},
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
	pod.Labels["app"] = "warm"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}

	r, runtime := newReconciler(t, deployment, replicaSet, pod)
	runtime.nilSnapshots = map[string]bool{pod.Status.PodIP: true}

	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	assert.Empty(t, runtime.kvLimitCalls)
	assert.Empty(t, runtime.sleepCalls)
}

func TestReconcilePoolPoliciesSleepsOnlyAnIdleRedundantInstance(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-runtime-pool",
			Namespace: testNamespace,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{
					"lifecycle": {"sleepAfterSeconds": 60}
				}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "warm"}},
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
	pod.Labels["app"] = "warm"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	peer := warmPod("warm-elsewhere", "other-pool", true, corev1.PodRunning)
	peer.Status.PodIP = testPeerIP
	claim := sampleModelClaim()
	claim.UID = types.UID("claim-uid")
	claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Port: 20000, Phase: modelv1alpha1.ModelClaimActive},
		{Pod: "warm-elsewhere", Port: 20001, Phase: modelv1alpha1.ModelClaimActive},
	}

	r, runtime := newReconciler(t, deployment, replicaSet, pod, peer, claim)
	r.PoolPolicy = newPoolPolicyManager(func() time.Time { return now })
	requestSuccessTotal := int64(10)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		pod.Status.PodIP: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 500}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "qwen2-7b", Phase: "active", Alive: true, Ready: true,
				RequestMetricsObserved: true, RequestSuccessTotal: &requestSuccessTotal,
				ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
			}},
		},
		peer.Status.PodIP: {
			Models: []RuntimeSnapshotModel{{
				ModelName: "qwen2-7b", Phase: "active", Alive: true, Ready: true,
				ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
			}},
		},
	}

	// The first counter observation establishes a conservative idle baseline.
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})
	require.Empty(t, runtime.sleepCalls)

	now = now.Add(61 * time.Second)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*pod})

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "qwen2-7b", runtime.sleepCalls[0].ModelName)
	assert.Equal(t, 1, runtime.sleepCalls[0].Level)
	gotPod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: pod.Name}, gotPod))
	assert.Contains(t, gotPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name], `"port":0`)
	gotClaim := getModel(t, r, claim.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, gotClaim.Status.Instances[0].Phase)
}

func TestPoolPolicyRequiresRuntimeReadyPeerBeforeSleep(t *testing.T) {
	claim := sampleModelClaim()
	claim.UID = types.UID("claim-uid")
	claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "target", Port: 20000, Phase: modelv1alpha1.ModelClaimActive},
		{Pod: "peer", Port: 20001, Phase: modelv1alpha1.ModelClaimActive},
	}
	peer := warmPod("peer", "other-pool", true, corev1.PodRunning)
	peer.Status.PodIP = testPeerIP
	r, runtime := newReconciler(t, claim, peer)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		peer.Status.PodIP: {Models: []RuntimeSnapshotModel{{
			ModelName: "qwen2-7b", Phase: "active", Alive: true, Ready: false,
			ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
		}}},
	}

	runtime.nilSnapshots = map[string]bool{peer.Status.PodIP: true}
	assert.False(t, r.hasOtherRuntimeReadyInstance(context.Background(), claim, "target"))
	delete(runtime.nilSnapshots, peer.Status.PodIP)
	assert.False(t, r.hasOtherRuntimeReadyInstance(context.Background(), claim, "target"))
	runtime.snapshots[peer.Status.PodIP].Models[0].Ready = true
	assert.True(t, r.hasOtherRuntimeReadyInstance(context.Background(), claim, "target"))
}

func TestMarkClaimInstanceSleepingSkipsAbsentPod(t *testing.T) {
	claim := sampleModelClaim()
	claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "present", Port: 20000, Phase: modelv1alpha1.ModelClaimActive,
	}}
	r, _ := newReconciler(t, claim)
	before := getModel(t, r, claim.Name)

	require.NoError(t, r.markClaimInstanceSleeping(context.Background(), before, "absent"))

	after := getModel(t, r, claim.Name)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, after.Status.Instances[0].Phase)
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
	runtime.snapshots = map[string]*RuntimeSnapshot{
		"10.0.0.1": {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMFreeBytes: 900}},
			Models:       []RuntimeSnapshotModel{{ModelName: "other", KVUsedBytes: 1}},
		},
		testPeerIP: {
			Accelerators:    []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMFreeBytes: 100}},
			CachedArtifacts: []string{pm.Spec.ArtifactURL},
		},
	}

	reconcileOnce(t, r, pm.Name)

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
	)

	reconcileOnce(t, r, pm.Name)
	got := getModel(t, r, pm.Name)
	require.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Instances[0].Phase)

	runtime.notReady = true // engine crashed / restarted
	reconcileOnce(t, r, pm.Name)

	got = getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, int32(0), got.Status.ReadyReplicas)
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

// TestReconcileReplicasZeroDeactivates verifies explicit replicas=0 deactivates instances.
func TestReconcileReplicasZeroDeactivates(t *testing.T) {
	zero := int32(0)
	pm := withFinalizer(sampleModelClaim())
	pm.Spec.Replicas = &zero
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
	got := getModel(t, r, pm.Name)
	assert.Empty(t, got.Status.Instances)
	assert.Equal(t, modelv1alpha1.ModelClaimPending, got.Status.Phase)
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
