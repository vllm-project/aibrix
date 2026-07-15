/*
Copyright 2025 The Aibrix Team.

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

package webhook

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingapi "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

var _ = ginkgo.Describe("podautoscaler default and validation", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}

		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
		var podautoscalerList autoscalingapi.PodAutoscalerList
		gomega.Expect(k8sClient.List(ctx, &podautoscalerList)).To(gomega.Succeed())

		for _, item := range podautoscalerList.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

	type testValidatingCase struct {
		podautoscaler func() *autoscalingapi.PodAutoscaler
		failed        bool
	}
	ginkgo.DescribeTable("test validating",
		func(tc *testValidatingCase) {
			if tc.failed {
				gomega.Expect(k8sClient.Create(ctx, tc.podautoscaler())).Should(gomega.HaveOccurred())
			} else {
				gomega.Expect(k8sClient.Create(ctx, tc.podautoscaler())).To(gomega.Succeed())
			}
		},
		ginkgo.Entry("valid PodAutoscaler", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("valid-pa").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "100m")).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
					Obj()
			},
			failed: false,
		}),
		ginkgo.Entry("KPA with valid POD metric", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("kpa-pod").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.KPA).
					MinReplicas(0).
					MaxReplicas(10).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourcePod(autoscalingapi.HTTP,
						"8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
					Obj()
			},
			failed: false,
		}),
		ginkgo.Entry("APA with valid EXTERNAL metric", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("apa-external").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.APA).
					MinReplicas(1).
					MaxReplicas(3).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test-ss").
					MetricSource(wrapper.MakeMetricSourceExternal(autoscalingapi.HTTP,
						"monitoring.example.com", "/metrics", "gpu_cache_usage_perc", "0.5")).
					Obj()
			},
			failed: false,
		}),
		ginkgo.Entry("minReplicas > maxReplicas", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("invalid-pa").
					Namespace(ns.Name).
					MinReplicas(10).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("invalid scalingStrategy", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				pa := wrapper.MakePodAutoscaler("bad-strategy").
					Namespace(ns.Name).
					MinReplicas(1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "100m")).
					Obj()
				pa.Spec.ScalingStrategy = "INVALID_STRATEGY"
				return pa
			},
			failed: true,
		}),
		ginkgo.Entry("no metricsSources", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				pa := wrapper.MakePodAutoscaler("no-metric").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					Obj()
				pa.Spec.MetricsSources = []autoscalingapi.MetricSource{} // empty
				return pa
			},
			failed: true,
		}),
		ginkgo.Entry("RESOURCE metric with forbidden port", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				ms := wrapper.MakeMetricSourceResource("cpu", "100m")
				ms.Port = "8080" // not allowed
				return wrapper.MakePodAutoscaler("bad-resource").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(ms).
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("POD metric missing path", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				ms := wrapper.MakeMetricSourcePod(
					autoscalingapi.HTTP, "8080", "", "qps", "100") // path empty
				return wrapper.MakePodAutoscaler("pod-no-path").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.KPA).
					MinReplicas(1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(ms).
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("invalid RESOURCE targetMetric", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("bad-resource-metric").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourceResource("invalid-resource", "1Gi")).
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("invalid quantity format", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("invalid-quantity").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "not-a-number")).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("negative quantity", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("negative-quantity").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.HPA).
					MinReplicas(1).
					MaxReplicas(5).
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "-100m")).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
					Obj()
			},
			failed: true,
		}),
	)

	type circuitBreakerCase struct {
		makePodAutoscaler func() *autoscalingapi.PodAutoscaler
		wantInvalid       bool
		assertStored      func(*autoscalingapi.PodAutoscaler)
	}

	makeCircuitBreakerPA := func(
		name string,
		strategy autoscalingapi.ScalingStrategyType,
		cb *autoscalingapi.CircuitBreakerConfig,
	) *autoscalingapi.PodAutoscaler {
		pa := wrapper.MakePodAutoscaler(name).
			Namespace(ns.Name).
			ScalingStrategy(strategy).
			MinReplicas(1).
			MaxReplicas(5).
			MetricSource(wrapper.MakeMetricSourceResource("cpu", "100m")).
			ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
			Obj()
		pa.Spec.CircuitBreaker = cb
		return pa
	}

	assertCircuitBreaker := func(
		pa *autoscalingapi.PodAutoscaler,
		action autoscalingapi.CircuitBreakerAction,
		failureThreshold, recoveryThreshold int32,
	) {
		gomega.Expect(pa.Spec.CircuitBreaker).ToNot(gomega.BeNil())
		gomega.Expect(pa.Spec.CircuitBreaker.Action).To(gomega.Equal(action))
		gomega.Expect(pa.Spec.CircuitBreaker.FailureThreshold).To(gomega.Equal(failureThreshold))
		gomega.Expect(pa.Spec.CircuitBreaker.RecoveryThreshold).To(gomega.Equal(recoveryThreshold))
	}

	ginkgo.DescribeTable("validates and defaults circuit breaker on create",
		func(tc circuitBreakerCase) {
			pa := tc.makePodAutoscaler()
			err := k8sClient.Create(ctx, pa)
			if tc.wantInvalid {
				gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "expected invalid create, got %v", err)
				return
			}
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			stored := &autoscalingapi.PodAutoscaler{}
			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), stored)).To(gomega.Succeed())
			if tc.assertStored != nil {
				tc.assertStored(stored)
			}
		},
		ginkgo.Entry("KPA enabled defaults to freeze and threshold 3",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA(
						"cb-kpa-defaults",
						autoscalingapi.KPA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: true},
					)
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					assertCircuitBreaker(pa, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)
				},
			}),
		ginkgo.Entry("APA enabled defaults to freeze and threshold 3",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA(
						"cb-apa-defaults",
						autoscalingapi.APA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: true},
					)
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					assertCircuitBreaker(pa, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)
				},
			}),
		ginkgo.Entry("explicit max action and thresholds are preserved",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA("cb-explicit-max", autoscalingapi.KPA, &autoscalingapi.CircuitBreakerConfig{
						Enabled:           true,
						Action:            autoscalingapi.CircuitBreakerActionMax,
						FailureThreshold:  1,
						RecoveryThreshold: 5,
					})
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					assertCircuitBreaker(pa, autoscalingapi.CircuitBreakerActionMax, 1, 5)
				},
			}),
		ginkgo.Entry("HPA enabled circuit breaker is rejected",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA(
						"cb-hpa-enabled",
						autoscalingapi.HPA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: true},
					)
				},
				wantInvalid: true,
			}),
		ginkgo.Entry("HPA disabled circuit breaker is accepted",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA(
						"cb-hpa-disabled",
						autoscalingapi.HPA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: false},
					)
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					gomega.Expect(pa.Spec.CircuitBreaker).ToNot(gomega.BeNil())
					gomega.Expect(pa.Spec.CircuitBreaker.Enabled).To(gomega.BeFalse())
				},
			}),
		ginkgo.Entry("invalid circuit breaker action is rejected",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA("cb-invalid-action", autoscalingapi.KPA, &autoscalingapi.CircuitBreakerConfig{
						Enabled: true,
						Action:  "panic",
					})
				},
				wantInvalid: true,
			}),
		ginkgo.Entry("zero thresholds are treated as defaults",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					return makeCircuitBreakerPA("cb-zero-thresholds", autoscalingapi.KPA, &autoscalingapi.CircuitBreakerConfig{
						Enabled:           true,
						FailureThreshold:  0,
						RecoveryThreshold: 0,
					})
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					assertCircuitBreaker(pa, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)
				},
			}),
		ginkgo.Entry("multiple metric sources are accepted with circuit breaker enabled",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					pa := makeCircuitBreakerPA(
						"cb-multi-metric",
						autoscalingapi.KPA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: true},
					)
					pa.Spec.MetricsSources = append(pa.Spec.MetricsSources, wrapper.MakeMetricSourceResource("memory", "1Gi"))
					return pa
				},
				assertStored: func(pa *autoscalingapi.PodAutoscaler) {
					gomega.Expect(pa.Spec.MetricsSources).To(gomega.HaveLen(2))
					assertCircuitBreaker(pa, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)
				},
			}),
		ginkgo.Entry("empty metric sources are rejected with circuit breaker enabled",
			circuitBreakerCase{
				makePodAutoscaler: func() *autoscalingapi.PodAutoscaler {
					pa := makeCircuitBreakerPA(
						"cb-empty-metric",
						autoscalingapi.KPA,
						&autoscalingapi.CircuitBreakerConfig{Enabled: true},
					)
					pa.Spec.MetricsSources = nil
					return pa
				},
				wantInvalid: true,
			}),
	)

	ginkgo.It("defaults circuit breaker on update and rejects an invalid HPA update", func() {
		pa := makeCircuitBreakerPA(
			"circuit-breaker-update",
			autoscalingapi.KPA,
			&autoscalingapi.CircuitBreakerConfig{Enabled: true},
		)

		gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
		stored := &autoscalingapi.PodAutoscaler{}
		key := client.ObjectKeyFromObject(pa)
		gomega.Expect(k8sClient.Get(ctx, key, stored)).To(gomega.Succeed())
		assertCircuitBreaker(stored, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)

		stored.Spec.CircuitBreaker.Action = ""
		stored.Spec.CircuitBreaker.FailureThreshold = 0
		stored.Spec.CircuitBreaker.RecoveryThreshold = 0
		gomega.Expect(k8sClient.Update(ctx, stored)).To(gomega.Succeed())
		updated := &autoscalingapi.PodAutoscaler{}
		gomega.Expect(k8sClient.Get(ctx, key, updated)).To(gomega.Succeed())
		assertCircuitBreaker(updated, autoscalingapi.CircuitBreakerActionFreeze, 3, 3)

		updated.Spec.ScalingStrategy = autoscalingapi.HPA
		err := k8sClient.Update(ctx, updated)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "expected invalid HPA update, got %v", err)
	})
})
