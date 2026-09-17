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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	modelclaimcontroller "github.com/vllm-project/aibrix/pkg/controller/modelclaim"
)

const (
	modelClaimTimeout  = 20 * time.Second
	modelClaimInterval = 200 * time.Millisecond
)

var modelClaimListenMu sync.Mutex

var _ = ginkgo.Describe("ModelClaim controller test", func() {
	var (
		ns      *corev1.Namespace
		runtime *modelClaimFakeRuntime
	)

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-modelclaim-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, 3*time.Second, modelClaimInterval).Should(gomega.Succeed())

		runtime = startModelClaimFakeRuntime()
	})

	ginkgo.AfterEach(func() {
		claims := &modelapi.ModelClaimList{}
		gomega.Expect(k8sClient.List(ctx, claims, client.InNamespace(ns.Name))).To(gomega.Succeed())
		for i := range claims.Items {
			_ = k8sClient.Delete(ctx, &claims.Items[i])
		}
		gomega.Eventually(func() int {
			remaining := &modelapi.ModelClaimList{}
			if err := k8sClient.List(ctx, remaining, client.InNamespace(ns.Name)); err != nil {
				return -1
			}
			return len(remaining.Items)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Equal(0))

		err := k8sClient.Delete(ctx, ns)
		gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
		runtime.Close()
	})

	ginkgo.It("progresses from initialization through Activating to Active", func() {
		runtime.setDefaultState("activating", false)
		pod := createModelClaimWarmPod(ns.Name, "warm-1", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-a", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Finalizers).To(gomega.ContainElement(modelclaimcontroller.ModelClaimFinalizer))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActivating))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			initialized := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeInitialized))
			g.Expect(initialized).NotTo(gomega.BeNil())
			g.Expect(initialized.Status).To(gomega.Equal(metav1.ConditionTrue))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("EngineStarting"))
			expectModelClaimRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateActivating)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		activationCallsWhileStarting := runtime.activateCallCount()
		gomega.Expect(activationCallsWhileStarting).To(gomega.BeNumerically(">=", 1))

		runtime.setClaimState(string(claim.UID), "active", true, "")
		triggerModelClaimReconcile(claim)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionTrue))
			expectModelClaimRoute(g, ns.Name, pod.Name, claim.Name, latest.Status.Instances[0].Port, constants.ModelClaimRoutingStateActive)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Consistently(runtime.activateCallCount, time.Second, modelClaimInterval).Should(gomega.Equal(activationCallsWhileStarting))
	})

	ginkgo.It("reports NoMatchingPods and recovers when a warm pod appears", func() {
		runtime.setDefaultState("active", true)
		claim := createIntegrationModelClaim(ns.Name, "claim-late-pod", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		expectModelClaimEvent(claim, corev1.EventTypeWarning, "NoMatchingPods")
		gomega.Expect(runtime.activateCallCount()).To(gomega.Equal(0))

		_ = createModelClaimWarmPod(ns.Name, "warm-late", "pool-a")

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal("warm-late"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("surfaces invalid engine configuration without contacting the runtime", func() {
		_ = createModelClaimWarmPod(ns.Name, "warm-invalid", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-invalid", "pool-a", nil, map[string]string{
			"--tensor-parallel-size": "invalid",
		})

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("InvalidEngineConfig"))
			g.Expect(ready.Message).To(gomega.ContainSubstring("must be a positive integer"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		expectModelClaimEvent(claim, corev1.EventTypeWarning, "InvalidEngineConfig")
		gomega.Expect(runtime.activateCallCount()).To(gomega.Equal(0))
	})

	ginkgo.It("records activation failure and retries to Active", func() {
		runtime.setDefaultState("active", true)
		runtime.failNextActivations(1)
		_ = createModelClaimWarmPod(ns.Name, "warm-retry", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-retry", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("ActivateFailed"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		expectModelClaimEvent(claim, corev1.EventTypeWarning, "ActivateFailed")

		// The controller's failure path returns RequeueAfter rather than relying on
		// a status update event. Wait for that policy-driven retry to succeed.
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(runtime.activateCallCount()).To(gomega.BeNumerically(">=", 2))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
		}, 2*modelclaimcontroller.DefaultRequeueDuration, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("reflects sleeping and terminal failed runtime states", func() {
		runtime.setDefaultState("active", true)
		pod := createModelClaimWarmPod(ns.Name, "warm-state", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-state", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(getIntegrationModelClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		runtime.setClaimState(string(claim.UID), "sleeping", false, "")
		triggerModelClaimReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			g.Expect(latest.Status.Instances[0].Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Reason).To(gomega.Equal("EngineSleeping"))
			expectModelClaimRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateSleeping)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		expectModelClaimEvent(claim, corev1.EventTypeNormal, "Sleeping")

		runtime.setClaimState(string(claim.UID), "failed", false, "restart budget exhausted")
		triggerModelClaimReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			g.Expect(latest.Status.Instances[0].Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Reason).To(gomega.Equal("EngineFailed"))
			expectModelClaimRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateFailed)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		expectModelClaimEvent(claim, corev1.EventTypeWarning, "EngineFailed")
	})

	ginkgo.It("drops a lost assignment and activates on a replacement pod", func() {
		runtime.setDefaultState("active", true)
		pod := createModelClaimWarmPod(ns.Name, "warm-old", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-replace", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		_ = createModelClaimWarmPod(ns.Name, "warm-new", "pool-a")
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal("warm-new"))
			g.Expect(runtime.activateCallCount()).To(gomega.BeNumerically(">=", 2))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("is idempotent across repeated reconciliations", func() {
		runtime.setDefaultState("active", true)
		pod := createModelClaimWarmPod(ns.Name, "warm-idempotent", "pool-a")
		claim := createIntegrationModelClaim(ns.Name, "claim-idempotent", "pool-a", nil, nil)

		var originalAnnotation string
		gomega.Eventually(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: pod.Name}, latestPod)).To(gomega.Succeed())
			originalAnnotation = latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name]
			g.Expect(originalAnnotation).NotTo(gomega.BeEmpty())
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		originalActivationCalls := runtime.activateCallCount()
		gomega.Expect(originalActivationCalls).To(gomega.BeNumerically(">=", 1))

		for range 3 {
			triggerModelClaimReconcile(claim)
		}
		gomega.Consistently(func(g gomega.Gomega) {
			latest := getIntegrationModelClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(runtime.activateCallCount()).To(gomega.Equal(originalActivationCalls))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: pod.Name}, latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name]).To(gomega.Equal(originalAnnotation))
		}, 2*time.Second, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("keeps multiple ModelClaims assignments and status isolated", func() {
		runtime.setDefaultState("active", true)
		pod := createModelClaimWarmPod(ns.Name, "warm-shared", "pool-a")
		servedName := "shared-model"
		first := createIntegrationModelClaim(ns.Name, "claim-one", "pool-a", &servedName, nil)
		second := createIntegrationModelClaim(ns.Name, "claim-two", "pool-a", &servedName, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			firstLatest := getIntegrationModelClaim(g, first)
			secondLatest := getIntegrationModelClaim(g, second)
			g.Expect(firstLatest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(secondLatest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(firstLatest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(secondLatest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(firstLatest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			g.Expect(secondLatest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			g.Expect(runtime.claimUIDs()).To(gomega.ConsistOf(string(first.UID), string(second.UID)))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: pod.Name}, latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations).To(gomega.HaveKey(constants.ModelClaimPodAnnotationPrefix + first.Name))
			g.Expect(latestPod.Annotations).To(gomega.HaveKey(constants.ModelClaimPodAnnotationPrefix + second.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		runtime.setClaimState(string(first.UID), "sleeping", false, "")
		triggerModelClaimReconcile(first)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(getIntegrationModelClaim(g, first).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			g.Expect(getIntegrationModelClaim(g, second).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: pod.Name}, latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+first.Name]).To(gomega.ContainSubstring(`"state":"sleeping"`))
			g.Expect(latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+second.Name]).To(gomega.ContainSubstring(`"state":"active"`))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})
})

func createIntegrationModelClaim(
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
		},
	}
	if engineArgs != nil {
		claim.Spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: engineArgs}
	}
	gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
	gomega.Expect(claim.UID).NotTo(gomega.BeEmpty())
	return claim
}

func createModelClaimWarmPod(namespace, name, pool string) *corev1.Pod {
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
	gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = modelAdapterEngineIP
	gomega.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
	return pod
}

func getIntegrationModelClaim(g gomega.Gomega, claim *modelapi.ModelClaim) *modelapi.ModelClaim {
	ginkgo.GinkgoHelper()
	latest := &modelapi.ModelClaim{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
	return latest
}

func triggerModelClaimReconcile(claim *modelapi.ModelClaim) {
	ginkgo.GinkgoHelper()
	latest := &modelapi.ModelClaim{}
	gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations["test.aibrix.ai/reconcile"] = fmt.Sprintf("%d", time.Now().UnixNano())
	gomega.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
}

func expectModelClaimRoute(
	g gomega.Gomega,
	namespace, podName, claimName string,
	port int32,
	state string,
) {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod)).To(gomega.Succeed())
	annotation := pod.Annotations[constants.ModelClaimPodAnnotationPrefix+claimName]
	g.Expect(annotation).To(gomega.ContainSubstring(fmt.Sprintf(`"port":%d`, port)))
	g.Expect(annotation).To(gomega.ContainSubstring(fmt.Sprintf(`"state":%q`, state)))
}

func expectModelClaimEvent(claim *modelapi.ModelClaim, eventType, reason string) {
	ginkgo.GinkgoHelper()
	gomega.Eventually(func() bool {
		events := &corev1.EventList{}
		if err := k8sClient.List(ctx, events, client.InNamespace(claim.Namespace)); err != nil {
			return false
		}
		for i := range events.Items {
			event := &events.Items[i]
			if event.InvolvedObject.UID == claim.UID && event.Type == eventType && event.Reason == reason {
				return true
			}
		}
		return false
	}, modelClaimTimeout, modelClaimInterval).Should(gomega.BeTrue())
}

type modelClaimFakeRuntime struct {
	mu sync.Mutex

	server *httptest.Server

	defaultPhase string
	defaultReady bool
	failures     int
	nextPort     int32

	activateCalls []modelclaimcontroller.ActivateRequest
	models        map[string]modelclaimcontroller.RuntimeSnapshotModel
}

func startModelClaimFakeRuntime() *modelClaimFakeRuntime {
	ginkgo.GinkgoHelper()
	modelClaimListenMu.Lock()
	defer modelClaimListenMu.Unlock()

	gomega.Expect(modelAdapterEngineIP).NotTo(
		gomega.BeEmpty(),
		"need a non-loopback local IPv4 for the ModelClaim fake runtime",
	)

	fake := &modelClaimFakeRuntime{
		defaultPhase: "active",
		defaultReady: true,
		nextPort:     19000,
		models:       map[string]modelclaimcontroller.RuntimeSnapshotModel{},
	}
	server := httptest.NewUnstartedServer(fake)
	_ = server.Listener.Close()

	addr := fmt.Sprintf("%s:%d", modelAdapterEngineIP, modelclaimcontroller.DefaultRuntimePort)
	var (
		listener net.Listener
		err      error
	)
	deadline := time.Now().Add(5 * time.Second)
	for {
		listener, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "listen on ModelClaim fake runtime address")
		}
		time.Sleep(50 * time.Millisecond)
	}
	server.Listener = listener
	server.Start()
	fake.server = server
	return fake
}

func (f *modelClaimFakeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/activate":
		f.handleActivate(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runtime/models/deactivate":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/snapshot":
		f.handleSnapshot(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/models":
		f.handleModelList(w)
	default:
		http.Error(w, "unsupported fake runtime operation", http.StatusNotFound)
	}
}

func (f *modelClaimFakeRuntime) handleActivate(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "injected activation failure", http.StatusServiceUnavailable)
		return
	}

	uid := ""
	if req.ClaimRef != nil {
		uid = req.ClaimRef.UID
	}
	port := f.nextPort
	f.nextPort++
	f.models[uid] = modelclaimcontroller.RuntimeSnapshotModel{
		ModelName:   req.ModelName,
		ArtifactURL: req.ArtifactURL,
		ClaimRef:    req.ClaimRef,
		Port:        port,
		IPCName:     req.IPCName,
		Phase:       f.defaultPhase,
		Alive:       f.defaultPhase != "failed",
		Ready:       f.defaultReady,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.ActivateResponse{
		Status: "success", ModelName: req.ModelName, Port: port, IPCName: req.IPCName,
	})
}

func (f *modelClaimFakeRuntime) handleSnapshot(w http.ResponseWriter) {
	f.mu.Lock()
	models := make([]modelclaimcontroller.RuntimeSnapshotModel, 0, len(f.models))
	for _, model := range f.models {
		models = append(models, model)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelclaimcontroller.RuntimeSnapshot{
		ObservedAt: time.Now(),
		Models:     models,
	})
}

func (f *modelClaimFakeRuntime) handleModelList(w http.ResponseWriter) {
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

func (f *modelClaimFakeRuntime) setDefaultState(phase string, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultPhase = phase
	f.defaultReady = ready
}

func (f *modelClaimFakeRuntime) failNextActivations(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = count
}

func (f *modelClaimFakeRuntime) setClaimState(uid, phase string, ready bool, lastError string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	model, found := f.models[uid]
	gomega.Expect(found).To(gomega.BeTrue(), "fake runtime has no model for claim UID %s", uid)
	model.Phase = phase
	model.Ready = ready
	model.Alive = phase != "failed"
	model.LastError = lastError
	f.models[uid] = model
}

func (f *modelClaimFakeRuntime) activateCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.activateCalls)
}

func (f *modelClaimFakeRuntime) claimUIDs() []string {
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

func (f *modelClaimFakeRuntime) Close() {
	if f != nil && f.server != nil {
		f.server.Close()
	}
}
