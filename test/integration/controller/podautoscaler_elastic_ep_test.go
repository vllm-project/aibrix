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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	controllerutils "github.com/vllm-project/aibrix/test/utils/controller"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

// elasticEPTestEngine simulates the endpoints an elastic-EP enabled vLLM
// engine pod serves: the scaling-state endpoint that the PodAutoscaler
// observation probes and the metrics endpoint that its pod metric source
// fetches. The mode can be flipped while a test runs, so a single pod can be
// observed as idle, scaling, or unable to answer.
type elasticEPTestEngine struct {
	mode atomic.Int32
}

const (
	elasticEPTestEngineIdle int32 = iota
	elasticEPTestEngineScaling
	elasticEPTestEngineUnavailable
)

func (e *elasticEPTestEngine) setMode(mode int32) {
	e.mode.Store(mode)
}

func (e *elasticEPTestEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc 50\n"))
	case "/is_scaling_elastic_ep":
		w.Header().Set("Content-Type", "application/json")
		switch e.mode.Load() {
		case elasticEPTestEngineScaling:
			_, _ = w.Write([]byte(`{"is_scaling_elastic_ep":true}`))
		case elasticEPTestEngineUnavailable:
			http.Error(w, "engine unavailable", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"is_scaling_elastic_ep":false}`))
		}
	default:
		http.NotFound(w, r)
	}
}

var _ = ginkgo.Describe("PodAutoscaler elastic EP scaling observation", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = controllerutils.CreateNamespace(ctx, k8sClient, "test-pa-eep-", 3*time.Second)
	})

	ginkgo.AfterEach(func() {
		controllerutils.DeleteNamespace(ctx, k8sClient, ns)
	})

	// startEngine serves one fake engine and returns its host and port so the
	// test can point pods and the pod metric source at it.
	startEngine := func() (*elasticEPTestEngine, string, string) {
		engine := &elasticEPTestEngine{}
		server := httptest.NewServer(engine)
		ginkgo.DeferCleanup(server.Close)

		engineURL, err := url.Parse(server.URL)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(engineURL.Hostname()).NotTo(gomega.BeEmpty())
		gomega.Expect(engineURL.Port()).NotTo(gomega.BeEmpty())
		return engine, engineURL.Hostname(), engineURL.Port()
	}

	createEngineDeployment := func(name string, replicas int32) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "engine", Image: "vllm/vllm-openai:latest"}},
					},
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, deployment)).To(gomega.Succeed())
	}

	// createEnginePod creates a ready pod with an IP and the port label that the
	// elastic EP observation resolves. Elastic EP pods carry the enabling flag
	// on their container command line, like a real engine deployment.
	createEnginePod := func(name, deployment string, elasticEP bool, ip, port string) {
		container := corev1.Container{Name: "engine", Image: "vllm/vllm-openai:latest"}
		if elasticEP {
			container.Args = []string{"--enable-elastic-ep"}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns.Name,
				Labels: map[string]string{
					"app":                      deployment,
					constants.ModelLabelEngine: "vllm",
					constants.ModelLabelPort:   port,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
		}
		gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())

		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = ip
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		gomega.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
	}

	createElasticEPPodAutoscaler := func(name, deployment, metricPort string) *autoscalingv1alpha1.PodAutoscaler {
		pa := wrapper.MakePodAutoscaler(name).
			Namespace(ns.Name).
			ScalingStrategy(autoscalingv1alpha1.KPA).
			MinReplicas(1).
			MaxReplicas(6).
			ScaleTargetRefWithKind("Deployment", "apps/v1", deployment).
			MetricSource(autoscalingv1alpha1.MetricSource{
				MetricSourceType: autoscalingv1alpha1.POD,
				ProtocolType:     autoscalingv1alpha1.HTTP,
				Port:             metricPort,
				Path:             "/metrics",
				TargetMetric:     "gpu_cache_usage_perc",
				TargetValue:      "50",
			}).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())

		// Delete the PodAutoscaler and wait until it is gone before the fake
		// engines shut down. The controller re-enqueues every remaining
		// PodAutoscaler on its resync, and a reconcile whose metric source no
		// longer answers holds the single reconcile worker for seconds, which
		// starves the specs that run later in the suite.
		ginkgo.DeferCleanup(func() {
			gomega.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pa))).To(gomega.Succeed())
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), &autoscalingv1alpha1.PodAutoscaler{})
				g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
			}, 10*time.Second, 250*time.Millisecond).Should(gomega.Succeed())
		})
		return pa
	}

	getElasticEPStatus := func(pa *autoscalingv1alpha1.PodAutoscaler) *autoscalingv1alpha1.ElasticEPScalingStatus {
		return validation.GetPodAutoscaler(ctx, k8sClient, pa).Status.ElasticEPScaling
	}

	getDeploymentReplicas := func(name string) int32 {
		deployment := &appsv1.Deployment{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: name}, deployment)).To(gomega.Succeed())
		gomega.Expect(deployment.Spec.Replicas).NotTo(gomega.BeNil())
		return *deployment.Spec.Replicas
	}

	ginkgo.It("tracks the scaling lifecycle and keeps the previous status when an engine cannot answer", func() {
		engine1, host1, port1 := startEngine()
		engine2, host2, port2 := startEngine()

		createEngineDeployment("eep-deployment", 2)
		createEnginePod("eep-pod-1", "eep-deployment", true, host1, port1)
		createEnginePod("eep-pod-2", "eep-deployment", true, host2, port2)
		pa := createElasticEPPodAutoscaler("pa-eep", "eep-deployment", port1)

		gomega.Eventually(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeFalse())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(0)))
			g.Expect(status.LastTransitionTime).NotTo(gomega.BeNil())
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

		engine1.setMode(elasticEPTestEngineScaling)
		engine2.setMode(elasticEPTestEngineScaling)

		gomega.Eventually(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeTrue())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(2)))
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

		engine2.setMode(elasticEPTestEngineIdle)

		gomega.Eventually(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeTrue())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(1)))
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

		keptTransition := getElasticEPStatus(pa).LastTransitionTime

		// An engine that cannot report its state must not be read as idle: the
		// previous status is kept while the other engine still reports scaling.
		engine2.setMode(elasticEPTestEngineUnavailable)
		gomega.Consistently(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeTrue())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(1)))
			g.Expect(status.LastTransitionTime.Time.Equal(keptTransition.Time)).To(gomega.BeTrue())
		}, 15*time.Second, time.Second).Should(gomega.Succeed())

		engine1.setMode(elasticEPTestEngineIdle)
		engine2.setMode(elasticEPTestEngineIdle)

		gomega.Eventually(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeFalse())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(0)))
			g.Expect(status.LastTransitionTime.Time.After(keptTransition.Time)).To(gomega.BeTrue())
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

		// The observation is informational and must not change the replica decision.
		gomega.Expect(getDeploymentReplicas("eep-deployment")).To(gomega.Equal(int32(2)))
	})

	ginkgo.It("clears the status when no engine pod enables elastic EP", func() {
		_, host, port := startEngine()

		createEngineDeployment("eep-clear-deployment", 2)
		createEnginePod("eep-clear-pod-1", "eep-clear-deployment", true, host, port)
		createEnginePod("eep-clear-pod-2", "eep-clear-deployment", true, host, port)
		pa := createElasticEPPodAutoscaler("pa-eep-clear", "eep-clear-deployment", port)

		gomega.Eventually(func(g gomega.Gomega) {
			status := getElasticEPStatus(pa)
			g.Expect(status).NotTo(gomega.BeNil())
			g.Expect(status.InProgress).To(gomega.BeFalse())
			g.Expect(status.ObservedEngines).To(gomega.Equal(int32(2)))
			g.Expect(status.ScalingEngines).To(gomega.Equal(int32(0)))
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())

		// Replace the elastic EP engines with plain engines: the recorded status
		// no longer describes the scale target and must be cleared.
		deleteEnginePod := func(name string) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}}
			gomega.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod))).To(gomega.Succeed())
		}
		deleteEnginePod("eep-clear-pod-1")
		deleteEnginePod("eep-clear-pod-2")
		createEnginePod("eep-clear-pod-3", "eep-clear-deployment", false, host, port)
		createEnginePod("eep-clear-pod-4", "eep-clear-deployment", false, host, port)

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(getElasticEPStatus(pa)).To(gomega.BeNil())
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Succeed())
	})
})
