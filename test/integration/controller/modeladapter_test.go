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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/modeladapter"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	modelAdapterTimeout    = time.Second * 20
	modelAdapterInterval   = time.Millisecond * 250
	modelAdapterEnginePort = 8000
)

// modelAdapterEngineIP is a non-loopback local IPv4. EndpointSlice rejects
// loopback addresses, so the mock inference engine must listen on a real
// local address that we also stamp onto Pod.Status.PodIP.
var modelAdapterEngineIP = localNonLoopbackIPv4()

var _ = ginkgo.Describe("ModelAdapter controller test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-modeladapter-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		// Delete adapters first so finalizers/owned objects are cleaned before the namespace goes terminating.
		adapters := &modelapi.ModelAdapterList{}
		gomega.Expect(k8sClient.List(ctx, adapters, client.InNamespace(ns.Name))).To(gomega.Succeed())
		for i := range adapters.Items {
			_ = k8sClient.Delete(ctx, &adapters.Items[i])
		}
		gomega.Eventually(func() int {
			latest := &modelapi.ModelAdapterList{}
			if err := k8sClient.List(ctx, latest, client.InNamespace(ns.Name)); err != nil {
				return -1
			}
			return len(latest.Items)
		}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Equal(0))

		err := k8sClient.Delete(ctx, ns)
		gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
	})

	ginkgo.Context("Service and EndpointSlice lifecycle", func() {
		var srv *modelAdapterMockEngine

		ginkgo.BeforeEach(func() {
			// Placeholder handler; each It replaces it with an adapter-specific response.
			srv = startModelAdapterMockEngine(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(modelListJSON("placeholder")))
			})
		})

		ginkgo.AfterEach(func() {
			if srv != nil {
				srv.Close()
			}
		})

		ginkgo.It("creates an owned headless Service and EndpointSlice after the adapter loads", func() {
			adapterName := "adapter-svc"
			pod := createModelAdapterEnginePod(ns.Name, "engine-0", "base-model", true, time.Now().Add(-30*time.Second))
			srv.setHandler(newModelAdapterLoadHandler(adapterName, 0))

			adapter := createIntegrationModelAdapter(ns.Name, adapterName, "base-model", nil)

			svc := &corev1.Service{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, svc)
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
			gomega.Expect(svc.Spec.ClusterIP).To(gomega.Equal(corev1.ClusterIPNone))
			gomega.Expect(svc.Spec.Ports).ToNot(gomega.BeEmpty())
			gomega.Expect(svc.Spec.Ports[0].Port).To(gomega.Equal(int32(modelAdapterEnginePort)))
			expectModelAdapterOwnedBy(svc, adapter)

			eps := &discoveryv1.EndpointSlice{}
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, eps)
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
			gomega.Expect(eps.AddressType).To(gomega.Equal(discoveryv1.AddressTypeIPv4))
			gomega.Expect(eps.Labels).To(gomega.HaveKeyWithValue("kubernetes.io/service-name", adapterName))
			gomega.Expect(eps.Endpoints).To(gomega.HaveLen(1))
			gomega.Expect(eps.Endpoints[0].Addresses).To(gomega.ConsistOf(modelAdapterEngineIP))
			expectModelAdapterOwnedBy(eps, adapter)

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Instances).To(gomega.ConsistOf(pod.Name))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
				g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelAdapterRunning))
				ready := apimeta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelAdapterConditionReady))
				g.Expect(ready).NotTo(gomega.BeNil())
				g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionTrue))
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
		})

		ginkgo.It("updates EndpointSlice addresses when another engine pod becomes ready", func() {
			adapterName := "adapter-eps-update"
			_ = createModelAdapterEnginePod(ns.Name, "engine-a", "base-model", true, time.Now().Add(-30*time.Second))
			srv.setHandler(newModelAdapterLoadHandler(adapterName, 0))
			adapter := createIntegrationModelAdapter(ns.Name, adapterName, "base-model", nil)

			gomega.Eventually(func(g gomega.Gomega) {
				eps := &discoveryv1.EndpointSlice{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, eps)).To(gomega.Succeed())
				g.Expect(eps.Endpoints).To(gomega.HaveLen(1))
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())

			_ = createModelAdapterEnginePod(ns.Name, "engine-b", "base-model", true, time.Now().Add(-30*time.Second))

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Instances).To(gomega.ContainElements("engine-a", "engine-b"))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(2)))

				eps := &discoveryv1.EndpointSlice{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, eps)).To(gomega.Succeed())
				g.Expect(eps.Endpoints).To(gomega.HaveLen(2))
				var addrs []string
				for _, ep := range eps.Endpoints {
					addrs = append(addrs, ep.Addresses...)
				}
				g.Expect(addrs).To(gomega.ConsistOf(modelAdapterEngineIP, modelAdapterEngineIP))
			}, time.Second*40, modelAdapterInterval).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("pod readiness and scheduling backoff", func() {
		ginkgo.It("reports NoReadyPods while matching pods are not ready", func() {
			_ = createModelAdapterEnginePod(ns.Name, "not-ready", "base-model", false, time.Time{})
			adapter := createIntegrationModelAdapter(ns.Name, "adapter-no-ready", "base-model", ptr.To(int32(1)))

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(0)))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))

				scheduled := apimeta.FindStatusCondition(
					latest.Status.Conditions,
					string(modelapi.ModelAdapterConditionTypeScheduled),
				)
				g.Expect(scheduled).NotTo(gomega.BeNil())
				g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
				g.Expect(scheduled.Reason).To(gomega.Equal(modeladapter.NoReadyPodsReason))

				ready := apimeta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelAdapterConditionReady))
				g.Expect(ready).NotTo(gomega.BeNil())
				g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(gomega.Equal(modeladapter.NoReadyPodsReason))
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
		})

		ginkgo.It("waits out the readiness backoff before scheduling a recently ready pod", func() {
			adapterName := "adapter-backoff"
			podName := "fresh-ready"
			srv := startModelAdapterMockEngine(newModelAdapterLoadHandler(adapterName, 0))
			defer srv.Close()

			_ = createModelAdapterEnginePod(ns.Name, podName, "base-model", true, time.Now().Add(-1*time.Second))
			adapter := createIntegrationModelAdapter(ns.Name, adapterName, "base-model", ptr.To(int32(1)))

			// While the pod is still inside the RetryBackoffSeconds window, the
			// single-replica path must not schedule it (Candidates may be 1 from
			// getActivePodsForModelAdapter, but ReadyReplicas stays 0).
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(1)))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
				g.Expect(latest.Status.Instances).To(gomega.BeEmpty())

				scheduled := apimeta.FindStatusCondition(
					latest.Status.Conditions,
					string(modelapi.ModelAdapterConditionTypeScheduled),
				)
				g.Expect(scheduled).NotTo(gomega.BeNil())
				g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
				g.Expect(scheduled.Reason).To(gomega.Equal(modeladapter.NoReadyPodsReason))
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())

			gomega.Consistently(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			}, time.Second*2, modelAdapterInterval).Should(gomega.Succeed())

			// After the remaining RetryBackoffSeconds window elapses, the pod should
			// be scheduled and the mock engine should accept the load path.
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Status.Instances).To(gomega.ConsistOf(podName))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("retry annotations", func() {
		ginkgo.It("sets retry annotations on load failure and clears them after a successful load", func() {
			adapterName := "adapter-retry"
			// Fail only the first POST /v1/load_lora_adapter so retryCount stays exactly 1.
			srv := startModelAdapterMockEngine(newModelAdapterLoadHandler(adapterName, 1))
			defer srv.Close()

			pod := createModelAdapterEnginePod(ns.Name, "retry-engine", "base-model", true, time.Now().Add(-30*time.Second))
			adapter := createIntegrationModelAdapter(ns.Name, adapterName, "base-model", nil)

			retryCountKey := fmt.Sprintf("%s.%s", modeladapter.RetryCountAnnotationKey, pod.Name)
			lastRetryKey := fmt.Sprintf("%s.%s", modeladapter.LastRetryTimeAnnotationKey, pod.Name)

			var observedCount int
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Annotations).To(gomega.HaveKey(retryCountKey))
				g.Expect(latest.Annotations).To(gomega.HaveKey(lastRetryKey))
				count, err := strconv.Atoi(latest.Annotations[retryCountKey])
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(count).To(gomega.Equal(1))
				observedCount = count
			}, modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())

			// Backoff after retryCount=1 is RetryBackoffSeconds * 2^1; wait that out plus
			// the usual reconcile timeout for the successful load to clear annotations.
			backoff := time.Duration(modeladapter.RetryBackoffSeconds*(1<<observedCount)) * time.Second
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &modelapi.ModelAdapter{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), latest)).To(gomega.Succeed())
				g.Expect(latest.Annotations).NotTo(gomega.HaveKey(retryCountKey))
				g.Expect(latest.Annotations).NotTo(gomega.HaveKey(lastRetryKey))
				g.Expect(latest.Status.Instances).To(gomega.ConsistOf(pod.Name))
				g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))

				svc := &corev1.Service{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, svc)).To(gomega.Succeed())
				eps := &discoveryv1.EndpointSlice{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: adapterName}, eps)).To(gomega.Succeed())
			}, backoff+modelAdapterTimeout, modelAdapterInterval).Should(gomega.Succeed())
		})
	})
})

func expectModelAdapterOwnedBy(obj metav1.Object, owner *modelapi.ModelAdapter) {
	ginkgo.GinkgoHelper()
	refs := obj.GetOwnerReferences()
	gomega.Expect(refs).To(gomega.HaveLen(1))
	ref := refs[0]
	gomega.Expect(ref.Kind).To(gomega.Equal("ModelAdapter"))
	gomega.Expect(ref.APIVersion).To(gomega.Equal(modelapi.GroupVersion.String()))
	gomega.Expect(ref.Name).To(gomega.Equal(owner.Name))
	gomega.Expect(ref.UID).To(gomega.Equal(owner.UID))
	gomega.Expect(ref.Controller).NotTo(gomega.BeNil())
	gomega.Expect(*ref.Controller).To(gomega.BeTrue())
}

func createIntegrationModelAdapter(namespace, name, baseModel string, replicas *int32) *modelapi.ModelAdapter {
	ginkgo.GinkgoHelper()
	adapter := wrapper.MakeModelAdapter(name).
		Namespace(namespace).
		ArtifactURL("huggingface://example/test-adapter").
		PodSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":                    "modeladapter-engine",
				constants.ModelLabelName: baseModel,
			},
		}).
		Replicas(replicas).
		Obj()
	adapter.Labels = map[string]string{
		constants.ModelLabelName: name,
	}
	gomega.Expect(k8sClient.Create(ctx, adapter)).To(gomega.Succeed())
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(adapter), adapter)
	}, time.Second*3, modelAdapterInterval).Should(gomega.Succeed())
	return adapter
}

func createModelAdapterEnginePod(namespace, name, baseModel string, ready bool, readySince time.Time) *corev1.Pod {
	ginkgo.GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                    "modeladapter-engine",
				constants.ModelLabelName: baseModel,
				modeladapter.ModelAdapterPodTemplateLabelKey: modeladapter.ModelAdapterPodTemplateLabelValue,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "vllm", Image: "vllm/vllm-openai:test"},
			},
		},
	}
	gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())

	gomega.Eventually(func(g gomega.Gomega) {
		latest := &corev1.Pod{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latest)).To(gomega.Succeed())
		latest.Status.Phase = corev1.PodRunning
		latest.Status.PodIP = modelAdapterEngineIP
		latest.Status.PodIPs = []corev1.PodIP{{IP: modelAdapterEngineIP}}
		if ready {
			cond := corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
				Reason: "TestReady",
			}
			if !readySince.IsZero() {
				cond.LastTransitionTime = metav1.NewTime(readySince)
			} else {
				cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Second))
			}
			latest.Status.Conditions = []corev1.PodCondition{cond}
		} else {
			latest.Status.Conditions = []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionFalse,
				Reason:             "TestNotReady",
				LastTransitionTime: metav1.Now(),
			}}
		}
		g.Expect(k8sClient.Status().Update(ctx, latest)).To(gomega.Succeed())
		*pod = *latest
	}, time.Second*5, modelAdapterInterval).Should(gomega.Succeed())

	return pod
}

func modelListJSON(adapterName string) string {
	return fmt.Sprintf(`{
  "object": "list",
  "data": [
    {
      "id": "%s",
      "object": "model",
      "created": 1765479369,
      "owned_by": "vllm",
      "root": "dummy/path",
      "parent": null
    }
  ]
}`, adapterName)
}

func emptyModelListJSON() string {
	return `{
  "object": "list",
  "data": []
}`
}

// newModelAdapterLoadHandler returns a mock engine handler that exercises the real
// load path: GET /v1/models is empty until a successful POST /v1/load_lora_adapter,
// after which the adapter appears in the model list. failLoadPosts controls how
// many load POSTs return 503 before succeeding (0 = succeed immediately).
func newModelAdapterLoadHandler(adapterName string, failLoadPosts int) http.HandlerFunc {
	var mu sync.Mutex
	loaded := false
	remainingFails := failLoadPosts
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models"):
			w.WriteHeader(http.StatusOK)
			if loaded {
				_, _ = w.Write([]byte(modelListJSON(adapterName)))
			} else {
				_, _ = w.Write([]byte(emptyModelListJSON()))
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "load_lora_adapter"):
			if remainingFails > 0 {
				remainingFails--
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("service unavailable"))
				return
			}
			loaded = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

func localNonLoopbackIPv4() string {
	// Prefer an address already assigned to the host that EndpointSlice will accept
	// and that we can bind the mock engine to.
	candidates := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			candidates = append(candidates, ip.String())
		}
	}
	for _, candidate := range candidates {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", candidate, modelAdapterEnginePort))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return candidate
	}
	return ""
}

type modelAdapterMockEngine struct {
	server  *httptest.Server
	mu      sync.Mutex
	handler http.HandlerFunc
}

func (m *modelAdapterMockEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	h := m.handler
	m.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (m *modelAdapterMockEngine) setHandler(handler http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = handler
}

func (m *modelAdapterMockEngine) Close() {
	if m != nil && m.server != nil {
		m.server.Close()
	}
}

var modelAdapterListenMu sync.Mutex

func startModelAdapterMockEngine(handler http.HandlerFunc) *modelAdapterMockEngine {
	ginkgo.GinkgoHelper()
	modelAdapterListenMu.Lock()
	defer modelAdapterListenMu.Unlock()

	gomega.Expect(modelAdapterEngineIP).NotTo(
		gomega.BeEmpty(),
		"need a non-loopback local IPv4 for EndpointSlice-compatible mock engine",
	)

	mock := &modelAdapterMockEngine{handler: handler}
	addr := fmt.Sprintf("%s:%d", modelAdapterEngineIP, modelAdapterEnginePort)
	ts := httptest.NewUnstartedServer(mock)
	_ = ts.Listener.Close()

	var l net.Listener
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "listen on mock engine address")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ts.Listener = l
	ts.Start()
	mock.server = ts
	return mock
}
