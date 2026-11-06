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
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	controllerutils "github.com/vllm-project/aibrix/test/utils/controller"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

// These specs scale a replica-mode StormService whose RoleSets run a prefill pod
// and a decode pod group of 2 pods. The StormService, RoleSet and PodSet
// controllers create the pods; the specs mark them ready, as a kubelet would,
// and serve their metric from a local server.
var _ = ginkgo.Describe("PodAutoscaler RoleSet scaling", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = controllerutils.CreateNamespace(ctx, k8sClient, "test-pa-roleset-", 3*time.Second)
	})

	ginkgo.AfterEach(func() {
		controllerutils.DeleteNamespace(ctx, k8sClient, ns)
	})

	const (
		ssName      = "roleset-pa-ss"
		podsPerSet  = 3 // 1 prefill pod and a decode pod group of 2
		targetValue = "50"
	)

	// serveMetric starts a metrics server that reports value and returns its host
	// and port.
	serveMetric := func(value int) (string, string) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc %d\n", value)
		}))
		ginkgo.DeferCleanup(server.Close)
		u, err := url.Parse(server.URL)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return u.Hostname(), u.Port()
	}

	createStormService := func(replicas int32) {
		matchLabel := map[string]string{"app": ssName}
		roleTemplate := corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"app":                      ssName,
				constants.ModelLabelEngine: "vllm",
			}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Image: "vllm/vllm-openai:latest"}}},
		}
		roleSetSpec := &orchestrationapi.RoleSetSpec{Roles: []orchestrationapi.RoleSpec{
			{Name: "prefill", Replicas: ptr.To(int32(1)), Template: roleTemplate},
			{Name: "decode", Replicas: ptr.To(int32(1)), PodGroupSize: ptr.To(int32(2)), Template: roleTemplate},
		}}
		ss := wrapper.MakeStormService(ssName).
			Namespace(ns.Name).
			Mode(orchestrationapi.StormServiceReplicaMode).
			Replicas(ptr.To(replicas)).
			Selector(metav1.SetAsLabelSelector(matchLabel)).
			UpdateStrategyType(orchestrationapi.RollingUpdateStormServiceStrategyType).
			RoleSetTemplateMeta(metav1.ObjectMeta{Labels: matchLabel}, roleSetSpec).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, ss)).To(gomega.Succeed())
	}

	// markPodsReady waits until the StormService has pods for want RoleSets and
	// marks every pod that is not ready yet as running and ready at podIP.
	markPodsReady := func(want int, podIP string) {
		gomega.Eventually(func(g gomega.Gomega) {
			pods := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, pods, client.InNamespace(ns.Name),
				client.MatchingLabels{controllerconstants.StormServiceNameLabelKey: ssName})).To(gomega.Succeed())
			g.Expect(pods.Items).To(gomega.HaveLen(want * podsPerSet))
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Status.PodIP != "" {
					continue
				}
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = podIP
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
			}
		}, 20*time.Second, 250*time.Millisecond).Should(gomega.Succeed())
	}

	stormServiceReplicas := func() int32 {
		ss := &orchestrationapi.StormService{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: ssName}, ss)).To(gomega.Succeed())
		return *ss.Spec.Replicas
	}

	ginkgo.DescribeTable("sizes RoleSets of pod groups and counts a starting RoleSet as carrying no load",
		func(strategy autoscalingv1alpha1.ScalingStrategyType) {
			host, port := serveMetric(60)

			createStormService(2)
			markPodsReady(2, host)
			pa := wrapper.MakePodAutoscaler("roleset-pa").
				Namespace(ns.Name).
				ScalingStrategy(strategy).
				MinReplicas(1).
				MaxReplicas(10).
				ScaleTargetRefWithKind("StormService", orchestrationapi.GroupVersion.String(), ssName).
				MetricSource(autoscalingv1alpha1.MetricSource{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
					Port:             port,
					Path:             "/metrics",
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      targetValue,
				}).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
			// envtest never finishes deleting a namespace, so delete the PodAutoscaler
			// before the metrics server closes; it would keep the controller busy
			// retrying the scrape during the specs that follow.
			ginkgo.DeferCleanup(func() {
				err := k8sClient.Delete(ctx, pa)
				gomega.Expect(err == nil || apierrors.IsNotFound(err)).To(gomega.BeTrue())
				gomega.Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), &autoscalingv1alpha1.PodAutoscaler{})
					return apierrors.IsNotFound(err)
				}, 10*time.Second, 250*time.Millisecond).Should(gomega.BeTrue())
			})

			// 2 ready RoleSets whose decode leaders report 60 against 50: 2.4 RoleSets.
			gomega.Eventually(stormServiceReplicas, 30*time.Second, 250*time.Millisecond).Should(gomega.Equal(int32(3)))

			// The 3rd RoleSet's pods exist and are not ready. Counting it as carrying no
			// load, the 2 ready RoleSets' 120 is 2.4 RoleSets, so the scale stays at 3.
			gomega.Consistently(stormServiceReplicas, 15*time.Second, time.Second).Should(gomega.Equal(int32(3)))

			// Once it is ready, 3 RoleSets at 60 is 3.6 RoleSets. The metric window
			// averages the summed load, so this takes up to its 1 minute panic window.
			// The 4th RoleSet then starts and the 180 of the 3 ready RoleSets holds 4.
			markPodsReady(3, host)
			gomega.Eventually(stormServiceReplicas, 2*time.Minute, 250*time.Millisecond).Should(gomega.Equal(int32(4)))
			gomega.Consistently(stormServiceReplicas, 15*time.Second, time.Second).Should(gomega.Equal(int32(4)))
		},
		ginkgo.Entry("APA", autoscalingv1alpha1.APA),
		ginkgo.Entry("KPA", autoscalingv1alpha1.KPA),
	)
})
