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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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
	newSchedulesPA := func(
		name string,
		schedules ...autoscalingapi.PodAutoscalerSchedule,
	) *autoscalingapi.PodAutoscaler {
		pa := wrapper.MakePodAutoscaler(name).
			Namespace(ns.Name).
			ScalingStrategy(autoscalingapi.HPA).
			MinReplicas(1).
			MaxReplicas(10).
			MetricSource(wrapper.MakeMetricSourceResource("cpu", "50")).
			ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
			Obj()
		pa.Spec.Schedules = schedules
		return pa
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
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "50")).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deploy").
					Obj()
			},
			failed: false,
		}),
		ginkgo.Entry("valid schedules", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-valid",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "weekday-peak",
						Timezone:    "America/Los_Angeles",
						DaysOfWeek:  []string{"Mon", "Tue", "Wed", "Thu", "Fri"},
						StartTime:   "09:00",
						EndTime:     "18:00",
						MinReplicas: ptr.To(int32(4)),
					},
				)
			},
			failed: false,
		}),
		ginkgo.Entry("schedules invalid time", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-invalid-time",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "invalid-time",
						StartTime:   "9:00",
						EndTime:     "18:00",
						MinReplicas: ptr.To(int32(1)),
					},
				)
			},
			failed: true,
		}),
		ginkgo.Entry("schedules cross midnight", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-cross-midnight", autoscalingapi.PodAutoscalerSchedule{
					Name: "cross-midnight", StartTime: "18:00", EndTime: "09:00", MinReplicas: ptr.To(int32(1)),
				})
			},
			failed: true,
		}),
		ginkgo.Entry("schedules invalid days", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-invalid-days",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "invalid-days",
						DaysOfWeek:  []string{"Monday"},
						StartTime:   "09:00",
						EndTime:     "18:00",
						MinReplicas: ptr.To(int32(1)),
					},
				)
			},
			failed: true,
		}),
		ginkgo.Entry("schedules invalid timezone", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-invalid-timezone",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "invalid-timezone",
						Timezone:    "Mars/Olympus_Mons",
						StartTime:   "09:00",
						EndTime:     "18:00",
						MinReplicas: ptr.To(int32(1)),
					},
				)
			},
			failed: true,
		}),
		ginkgo.Entry("schedules duplicate name", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-duplicate-name",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "peak",
						StartTime:   "09:00",
						EndTime:     "10:00",
						MinReplicas: ptr.To(int32(2)),
					},
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "peak",
						StartTime:   "10:00",
						EndTime:     "11:00",
						MinReplicas: ptr.To(int32(3)),
					},
				)
			},
			failed: true,
		}),
		ginkgo.Entry("schedules missing override", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-missing-override", autoscalingapi.PodAutoscalerSchedule{
					Name: "missing-overrides", StartTime: "09:00", EndTime: "18:00",
				})
			},
			failed: true,
		}),
		ginkgo.Entry("schedules invalid effective bounds", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-invalid-effective",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "invalid-effective-bounds",
						StartTime:   "09:00",
						EndTime:     "18:00",
						MinReplicas: ptr.To(int32(11)),
					},
				)
			},
			failed: true,
		}),
		ginkgo.Entry("schedules overlap", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return newSchedulesPA("schedules-overlap",
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "morning",
						StartTime:   "09:00",
						EndTime:     "11:00",
						MinReplicas: ptr.To(int32(2)),
					},
					autoscalingapi.PodAutoscalerSchedule{
						Name:        "late-morning",
						StartTime:   "10:00",
						EndTime:     "12:00",
						MinReplicas: ptr.To(int32(3)),
					},
				)
			},
			failed: true,
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
		ginkgo.Entry("KPA with valid metric windows", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				observeWindow := int64(600)
				panicWindow := int64(60)
				pa := wrapper.MakePodAutoscaler("kpa-windows").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.KPA).
					MinReplicas(0).
					MaxReplicas(10).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourcePod(autoscalingapi.HTTP,
						"8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
					Obj()
				pa.Spec.ObserveWindowSeconds = &observeWindow
				pa.Spec.PanicWindowSeconds = &panicWindow
				return pa
			},
			failed: false,
		}),
		ginkgo.Entry("metric window must be positive", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				observeWindow := int64(0)
				pa := wrapper.MakePodAutoscaler("bad-window").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.KPA).
					MinReplicas(0).
					MaxReplicas(10).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourcePod(autoscalingapi.HTTP,
						"8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
					Obj()
				pa.Spec.ObserveWindowSeconds = &observeWindow
				return pa
			},
			failed: true,
		}),
		ginkgo.Entry("panic window must not exceed observe window", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				observeWindow := int64(60)
				panicWindow := int64(120)
				pa := wrapper.MakePodAutoscaler("bad-window-order").
					Namespace(ns.Name).
					ScalingStrategy(autoscalingapi.KPA).
					MinReplicas(0).
					MaxReplicas(10).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourcePod(autoscalingapi.HTTP,
						"8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
					Obj()
				pa.Spec.ObserveWindowSeconds = &observeWindow
				pa.Spec.PanicWindowSeconds = &panicWindow
				return pa
			},
			failed: true,
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
		ginkgo.Entry("negative minReplicas", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("negative-min-replicas").
					Namespace(ns.Name).
					MinReplicas(-1).
					MaxReplicas(5).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "50")).
					Obj()
			},
			failed: true,
		}),
		ginkgo.Entry("non-positive maxReplicas", &testValidatingCase{
			podautoscaler: func() *autoscalingapi.PodAutoscaler {
				return wrapper.MakePodAutoscaler("non-positive-max-replicas").
					Namespace(ns.Name).
					MinReplicas(0).
					MaxReplicas(0).
					ScaleTargetRefWithKind("Deployment", "apps/v1", "test").
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "50")).
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
					MetricSource(wrapper.MakeMetricSourceResource("cpu", "50")).
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
				ms := wrapper.MakeMetricSourceResource("cpu", "50")
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
})
