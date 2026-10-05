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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	modelclaimcontroller "github.com/vllm-project/aibrix/pkg/controller/modelclaim"
)

// ModelClaimFixture owns the client dependencies and fake runtime for a ModelClaim scenario.
type ModelClaimFixture struct {
	ctx      context.Context
	client   client.Client
	timeout  time.Duration
	interval time.Duration
	runtime  *FakeModelClaimRuntime
}

// NewModelClaimFixture starts a runtime, registers cleanup, and configures the scenario's polling helpers.
func NewModelClaimFixture(ctx context.Context, c client.Client, timeout, interval time.Duration) *ModelClaimFixture {
	ginkgo.GinkgoHelper()
	fake := &FakeModelClaimRuntime{
		defaultPhase: "active",
		defaultReady: true,
		nextPort:     19000,
		models:       map[string]modelclaimcontroller.RuntimeSnapshotModel{},
	}
	fake.ip = FindBindableNonLoopbackIPv4(modelclaimcontroller.DefaultRuntimePort)
	fake.server = StartFixedPortHTTPServer(
		fake.ip, modelclaimcontroller.DefaultRuntimePort, fake, 5*time.Second, 50*time.Millisecond,
	)
	fixture := &ModelClaimFixture{
		ctx:      ctx,
		client:   c,
		timeout:  timeout,
		interval: interval,
		runtime:  fake,
	}
	ginkgo.DeferCleanup(fixture.Close)
	return fixture
}

// Runtime returns the fake runtime used by this fixture.
func (f *ModelClaimFixture) Runtime() *FakeModelClaimRuntime {
	ginkgo.GinkgoHelper()
	return f.runtime
}

// Close stops the fixture runtime and tolerates incomplete setup.
func (f *ModelClaimFixture) Close() {
	ginkgo.GinkgoHelper()
	if f != nil && f.runtime != nil {
		f.runtime.Close()
	}
}

// CreateClaim creates a ModelClaim with the integration runtime defaults.
func (f *ModelClaimFixture) CreateClaim(
	namespace, name, pool string,
	modelName *string,
	engineArgs map[string]string,
) *modelapi.ModelClaim {
	ginkgo.GinkgoHelper()
	claim := &modelapi.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: modelapi.ModelClaimSpec{
			ModelName:   modelName,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: pool}},
			ArtifactURL: "huggingface://integration/" + name,
			Engine:      "vllm",
			// Every claim has to declare its per-GPU cost to be placed. The
			// fixture's warm pods expose no GPU, so no card is accounted for and
			// any positive figures will do.
			PerGPU: &modelapi.ModelClaimPerGPU{
				MaximumFootprint: resource.MustParse("1Gi"),
				KVFloor:          resource.MustParse("1Gi"),
			},
		},
	}
	if engineArgs != nil {
		claim.Spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: engineArgs}
	}
	gomega.Expect(f.client.Create(f.ctx, claim)).To(gomega.Succeed())
	gomega.Expect(claim.UID).NotTo(gomega.BeEmpty())
	return claim
}

// CreateWarmPod creates a running pool pod pointing to the fixture runtime.
func (f *ModelClaimFixture) CreateWarmPod(namespace, name, pool string) *corev1.Pod {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.ModelPoolLabelName:    pool,
				constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "aibrix-runtime"}}},
	}
	gomega.Expect(f.client.Create(f.ctx, pod)).To(gomega.Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = f.runtime.IP()
	gomega.Expect(f.client.Status().Update(f.ctx, pod)).To(gomega.Succeed())
	return pod
}

// CreatePoolPod creates a running pool pod that belongs to a Deployment, through
// a ReplicaSet, as a warm pool's pods do. The Deployment carries the given pool
// policy.
func (f *ModelClaimFixture) CreatePoolPod(namespace, name, pool, policy string) *corev1.Pod {
	ginkgo.GinkgoHelper()
	labels := map[string]string{
		constants.ModelPoolLabelName:    pool,
		constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue,
	}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "aibrix-runtime"}}},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{constants.ModelPoolPolicyAnnotationKey: policy},
		},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template},
	}
	gomega.Expect(f.client.Create(f.ctx, deployment)).To(gomega.Succeed())
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-rs",
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")),
			},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template},
	}
	gomega.Expect(f.client.Create(f.ctx, replicaSet)).To(gomega.Succeed())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-pod",
			Namespace: namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet")),
			},
		},
		Spec: template.Spec,
	}
	gomega.Expect(f.client.Create(f.ctx, pod)).To(gomega.Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = f.runtime.IP()
	gomega.Expect(f.client.Status().Update(f.ctx, pod)).To(gomega.Succeed())
	return pod
}

// RequestWake writes a wake request for a claim on a pod, as the gateway does
// for a request that finds the claim's engine asleep.
func (f *ModelClaimFixture) RequestWake(namespace, podName, claimName string) {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{}
	gomega.Expect(f.client.Get(f.ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod)).To(gomega.Succeed())
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claimName] = time.Now().UTC().Format(time.RFC3339)
	gomega.Expect(f.client.Patch(f.ctx, pod, patch)).To(gomega.Succeed())
}

// GetClaim retrieves the latest claim using the supplied polling assertions.
func (f *ModelClaimFixture) GetClaim(g gomega.Gomega, claim *modelapi.ModelClaim) *modelapi.ModelClaim {
	ginkgo.GinkgoHelper()
	latest := &modelapi.ModelClaim{}
	g.Expect(f.client.Get(f.ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
	return latest
}

// TriggerReconcile updates a claim annotation to trigger reconciliation.
func (f *ModelClaimFixture) TriggerReconcile(claim *modelapi.ModelClaim) {
	ginkgo.GinkgoHelper()
	gomega.Eventually(func() error {
		latest := &modelapi.ModelClaim{}
		if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(claim), latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations["test.aibrix.ai/reconcile"] = fmt.Sprintf("%d", time.Now().UnixNano())
		if err := f.client.Patch(f.ctx, latest, patch); err != nil {
			if apierrors.IsConflict(err) {
				return err
			}
			return gomega.StopTrying("patch ModelClaim reconcile annotation").Wrap(err)
		}
		return nil
	}, f.timeout, f.interval).Should(gomega.Succeed())
}

// ExpectRoute checks the pod routing annotation for a claim's port and state.
func (f *ModelClaimFixture) ExpectRoute(
	g gomega.Gomega,
	namespace, podName, claimName string,
	port int32,
	state string,
) {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{}
	g.Expect(f.client.Get(f.ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod)).To(gomega.Succeed())
	annotation := pod.Annotations[constants.ModelClaimPodAnnotationPrefix+claimName]
	route, err := decodeModelClaimRouteAnnotation(annotation)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(route.Port).To(gomega.Equal(port))
	g.Expect(route.State).To(gomega.Equal(state))
}

type modelClaimRouteAnnotation struct {
	Port  int32  `json:"port"`
	State string `json:"state"`
}

func decodeModelClaimRouteAnnotation(annotation string) (modelClaimRouteAnnotation, error) {
	route := modelClaimRouteAnnotation{}
	err := json.Unmarshal([]byte(annotation), &route)
	return route, err
}

// ExpectEvent waits for an event matching the claim UID, type, and reason.
func (f *ModelClaimFixture) ExpectEvent(claim *modelapi.ModelClaim, eventType, reason string) {
	ginkgo.GinkgoHelper()
	gomega.Eventually(func() bool {
		events := &corev1.EventList{}
		if err := f.client.List(f.ctx, events, client.InNamespace(claim.Namespace)); err != nil {
			return false
		}
		for i := range events.Items {
			event := &events.Items[i]
			if event.InvolvedObject.UID == claim.UID && event.Type == eventType && event.Reason == reason {
				return true
			}
		}
		return false
	}, f.timeout, f.interval).Should(gomega.BeTrue())
}

// FakeModelClaimRuntime serves controllable runtime responses for ModelClaim tests.
type FakeModelClaimRuntime struct {
	mu sync.Mutex

	server *httptest.Server
	ip     string

	defaultPhase string
	defaultReady bool
	failures     int
	wakeFailures int
	nextPort     int32

	activateCalls   []modelclaimcontroller.ActivateRequest
	deactivateCalls []modelclaimcontroller.DeactivateRequest
	wakeCalls       []modelclaimcontroller.WakeRequest
	sleepCalls      []modelclaimcontroller.SleepRequest
	models          map[string]modelclaimcontroller.RuntimeSnapshotModel

	// cardBytes is the size of the one card the runtime reports, and zero for
	// none. Without a card nothing is accounted for, as on a CPU pool.
	cardBytes int64
	// sleepingFootprintBytes is what an engine holds after it goes to sleep,
	// and zero for a memory the runtime could not measure.
	sleepingFootprintBytes int64
}

// IP returns the address used by fixture pods to reach the runtime.
func (f *FakeModelClaimRuntime) IP() string {
	ginkgo.GinkgoHelper()
	return f.ip
}

// ServeHTTP handles the runtime activation, deactivation, snapshot, and model-list endpoints.
func (f *FakeModelClaimRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ginkgo.GinkgoHelper()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/activate":
		f.handleActivate(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/deactivate":
		f.handleDeactivate(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/wake":
		f.handleWake(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/sleep":
		f.handleSleep(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/kv-limit":
		f.handleKVLimit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/snapshot":
		f.handleSnapshot(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/models":
		f.handleModelList(w)
	default:
		http.Error(w, "unsupported fake runtime operation", http.StatusNotFound)
	}
}

func (f *FakeModelClaimRuntime) handleActivate(w http.ResponseWriter, r *http.Request) {
	req := modelclaimcontroller.ActivateRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.activateCalls = append(f.activateCalls, req)
	if f.failures > 0 {
		f.failures--
		// As the runtime answers when it could not start an engine: an error
		// status, with its own report of the error in the body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(modelclaimcontroller.ActivateResponse{
			Status: "error", ModelName: req.ModelName, Message: "injected activation failure",
		})
		return
	}

	uid := ""
	if req.ClaimRef != nil {
		uid = req.ClaimRef.UID
	}
	port := f.nextPort
	f.nextPort++
	model := modelclaimcontroller.RuntimeSnapshotModel{
		ModelName:   req.ModelName,
		ArtifactURL: req.ArtifactURL,
		ClaimRef:    req.ClaimRef,
		Port:        port,
		IPCName:     req.IPCName,
		Phase:       f.defaultPhase,
		Alive:       f.defaultPhase != "failed",
		Ready:       f.defaultReady,
	}
	if f.cardBytes > 0 {
		// On a card the engine maps nothing yet, and serves no request. Its
		// request metrics are read, so the pool policy can tell it is idle.
		completed := int64(0)
		model.RequestMetricsObserved = true
		model.RequestSuccessTotal = &completed
	}
	f.models[uid] = model
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.ActivateResponse{
		Status: "success", ModelName: req.ModelName, Port: port, IPCName: req.IPCName,
	})
}

// handleWake wakes the sleeping engines serving the model, as the runtime does:
// they serve again at once here, with no boot to wait for.
func (f *FakeModelClaimRuntime) handleWake(w http.ResponseWriter, r *http.Request) {
	req := modelclaimcontroller.WakeRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeCalls = append(f.wakeCalls, req)
	if f.wakeFailures > 0 {
		f.wakeFailures--
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeOperationResponse{
			Status: "error", ModelName: req.ModelName, OperationID: req.OperationID,
		})
		return
	}
	applied := false
	for uid, model := range f.models {
		if model.ModelName == req.ModelName && model.Phase == "sleeping" {
			model.Phase = "active"
			model.Ready = true
			model.SleepingFootprintBytes = nil
			f.models[uid] = model
			applied = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: applied, Phase: "active",
	})
}

// handleSleep puts the engines serving the model to sleep, as the runtime does,
// and has each hold the sleeping footprint the fixture sets.
func (f *FakeModelClaimRuntime) handleSleep(w http.ResponseWriter, r *http.Request) {
	req := modelclaimcontroller.SleepRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleepCalls = append(f.sleepCalls, req)
	applied := false
	for uid, model := range f.models {
		if model.ModelName == req.ModelName && model.Phase == "active" {
			model.Phase = "sleeping"
			model.Ready = false
			model.SleepingFootprintBytes = f.footprintLocked()
			f.models[uid] = model
			applied = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: applied, Phase: "sleeping",
	})
}

// handleKVLimit writes a KV limit into the engines serving the model, which
// their snapshot then reports.
func (f *FakeModelClaimRuntime) handleKVLimit(w http.ResponseWriter, r *http.Request) {
	req := modelclaimcontroller.SetKVLimitRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	phase := ""
	for uid, model := range f.models {
		if model.ModelName == req.ModelName {
			model.KVCapacityBytes = req.LimitBytes
			f.models[uid] = model
			phase = model.Phase
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeOperationResponse{
		Status: "success", ModelName: req.ModelName, OperationID: req.OperationID, Applied: true, Phase: phase,
	})
}

// footprintLocked is the sleeping footprint an engine reports, or nil when
// the fixture sets none. The caller holds f.mu.
func (f *FakeModelClaimRuntime) footprintLocked() *int64 {
	if f.sleepingFootprintBytes <= 0 {
		return nil
	}
	footprint := f.sleepingFootprintBytes
	return &footprint
}

func (f *FakeModelClaimRuntime) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	req := modelclaimcontroller.DeactivateRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.deactivateCalls = append(f.deactivateCalls, req)
	for uid, model := range f.models {
		if model.ModelName == req.ModelName {
			delete(f.models, uid)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (f *FakeModelClaimRuntime) handleSnapshot(w http.ResponseWriter) {
	f.mu.Lock()
	models := make([]modelclaimcontroller.RuntimeSnapshotModel, 0, len(f.models))
	for _, model := range f.models {
		models = append(models, model)
	}
	var accelerators []modelclaimcontroller.RuntimeAcceleratorSnapshot
	if f.cardBytes > 0 {
		accelerators = []modelclaimcontroller.RuntimeAcceleratorSnapshot{{
			ID: "GPU-0", HBMTotalBytes: f.cardBytes, HBMUsableBytes: f.cardBytes,
		}}
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeSnapshot{
		ObservedAt:   time.Now(),
		Accelerators: accelerators,
		Models:       models,
	})
}

func (f *FakeModelClaimRuntime) handleModelList(w http.ResponseWriter) {
	f.mu.Lock()
	models := make([]modelclaimcontroller.ModelInfo, 0, len(f.models))
	for _, model := range f.models {
		models = append(models, modelclaimcontroller.ModelInfo{
			ModelName: model.ModelName,
			Port:      model.Port,
			IPCName:   model.IPCName,
			Phase:     model.Phase,
			Ready:     model.Ready,
		})
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Models []modelclaimcontroller.ModelInfo `json:"models"`
	}{Models: models})
}

// SetDefaultState sets the initial phase and readiness of future activations.
func (f *FakeModelClaimRuntime) SetDefaultState(phase string, ready bool) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultPhase = phase
	f.defaultReady = ready
}

// FailNextActivations injects failures into the next count activation requests.
func (f *FakeModelClaimRuntime) FailNextActivations(count int) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = count
}

// FailNextWakes makes the next count wake requests fail, as a runtime does
// when vLLM cannot wake its engine.
func (f *FakeModelClaimRuntime) FailNextWakes(count int) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeFailures = count
}

// SetClaimState updates the runtime state for an already activated claim UID.
// An engine set asleep holds the sleeping footprint the fixture sets.
func (f *FakeModelClaimRuntime) SetClaimState(uid, phase string, ready bool, lastError string) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	model, found := f.models[uid]
	gomega.Expect(found).To(gomega.BeTrue(), "fake runtime has no model for claim UID %s", uid)
	model.Phase = phase
	model.Ready = ready
	model.Alive = phase != "failed"
	model.LastError = lastError
	model.SleepingFootprintBytes = nil
	if phase == "sleeping" {
		model.SleepingFootprintBytes = f.footprintLocked()
	}
	f.models[uid] = model
}

// SetCard has the runtime report one card of the given size, which every
// placement on its pod is then accounted against.
func (f *FakeModelClaimRuntime) SetCard(bytes int64) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cardBytes = bytes
}

// SetSleepingFootprint sets what an engine holds once it is asleep.
func (f *FakeModelClaimRuntime) SetSleepingFootprint(bytes int64) {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleepingFootprintBytes = bytes
}

// SleepRequests returns a copy of the recorded sleep requests.
func (f *FakeModelClaimRuntime) SleepRequests() []modelclaimcontroller.SleepRequest {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelclaimcontroller.SleepRequest(nil), f.sleepCalls...)
}

// ActivateCallCount returns the total number of activation attempts, including failures.
func (f *FakeModelClaimRuntime) ActivateCallCount() int {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.activateCalls)
}

// DeactivateRequests returns a defensive copy of the recorded deactivation requests.
func (f *FakeModelClaimRuntime) DeactivateRequests() []modelclaimcontroller.DeactivateRequest {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelclaimcontroller.DeactivateRequest(nil), f.deactivateCalls...)
}

// WakeRequests returns a defensive copy of the recorded wake requests.
func (f *FakeModelClaimRuntime) WakeRequests() []modelclaimcontroller.WakeRequest {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelclaimcontroller.WakeRequest(nil), f.wakeCalls...)
}

// ClaimUIDs returns the unique claim UIDs recorded in activation requests.
func (f *FakeModelClaimRuntime) ClaimUIDs() []string {
	ginkgo.GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	unique := make(map[string]struct{}, len(f.activateCalls))
	for i := range f.activateCalls {
		if f.activateCalls[i].ClaimRef != nil {
			unique[f.activateCalls[i].ClaimRef.UID] = struct{}{}
		}
	}
	uids := make([]string, 0, len(unique))
	for uid := range unique {
		uids = append(uids, uid)
	}
	return uids
}

// Close stops the fake server and tolerates incomplete setup.
func (f *FakeModelClaimRuntime) Close() {
	ginkgo.GinkgoHelper()
	if f != nil && f.server != nil {
		f.server.Close()
	}
}
