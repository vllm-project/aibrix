/*
Copyright 2024 The Aibrix Team.

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

package modeladapter

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
)

// Disable Ginkgo tests that require Kubernetes environment
var _ = XDescribe("ModelAdapter Controller", func() {
	var (
		ctx        context.Context
		reconciler *ModelAdapterReconciler
		scheme     *runtime.Scheme
		recorder   *record.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(modelv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		recorder = record.NewFakeRecorder(100)
	})

	Describe("Pod Readiness Validation", func() {
		BeforeEach(func() {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler = &ModelAdapterReconciler{
				Client:   client,
				Scheme:   scheme,
				Recorder: recorder,
			}
		})

		Context("isPodReadyForScheduling", func() {
			It("should return false for non-ready pods", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionFalse,
							},
						},
					},
				}

				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
				Expect(result).To(BeFalse())
			})

			It("should return false for recently ready pods (within backoff period)", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Second)), // Recently ready
							},
						},
					},
				}

				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
				Expect(result).To(BeFalse())
			})

			It("should return true for stable ready pods", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Second)), // Stable ready
							},
						},
					},
				}

				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				result := reconciler.isPodReadyForScheduling(ctx, instance, pod)
				Expect(result).To(BeTrue())
			})
		})

		Context("isPodHealthy", func() {
			It("should return false for terminating pods", func() {
				now := metav1.Now()
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-pod",
						Namespace:         "default",
						DeletionTimestamp: &now,
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				result := reconciler.isPodHealthy(pod)
				Expect(result).To(BeFalse())
			})

			It("should return true for healthy running pods", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}

				result := reconciler.isPodHealthy(pod)
				Expect(result).To(BeTrue())
			})
		})
	})

	Describe("Retry Logic and Error Handling", func() {
		BeforeEach(func() {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler = &ModelAdapterReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      recorder,
				RuntimeConfig: config.RuntimeConfig{},
			}
		})

		Context("isRetriableError", func() {
			It("should identify connection refused as retriable", func() {
				err := fmt.Errorf("connection refused")
				result := reconciler.isRetriableError(err)
				Expect(result).To(BeTrue())
			})

			It("should identify timeout as retriable", func() {
				err := fmt.Errorf("request timeout")
				result := reconciler.isRetriableError(err)
				Expect(result).To(BeTrue())
			})

			It("should identify network errors as retriable", func() {
				err := fmt.Errorf("no route to host")
				result := reconciler.isRetriableError(err)
				Expect(result).To(BeTrue())
			})

			It("should not identify generic errors as retriable", func() {
				err := fmt.Errorf("invalid request format")
				result := reconciler.isRetriableError(err)
				Expect(result).To(BeFalse())
			})
		})

		Context("getRetryInfo and updateRetryInfo", func() {
			It("should return zero values for new instances", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
				Expect(count).To(Equal(int32(0)))
				Expect(lastTime.IsZero()).To(BeTrue())
			})

			It("should store and retrieve retry info correctly", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				reconciler.updateRetryInfo(instance, "test-pod", 3)

				count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
				Expect(count).To(Equal(int32(3)))
				Expect(lastTime).NotTo(BeZero())
			})

			It("should clear retry info correctly", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
						Annotations: map[string]string{
							fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey):    "3",
							fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey): time.Now().Format(time.RFC3339),
						},
					},
				}

				reconciler.clearRetryInfo(instance, "test-pod")

				count, lastTime := reconciler.getRetryInfo(instance, "test-pod")
				Expect(count).To(Equal(int32(0)))
				Expect(lastTime.IsZero()).To(BeTrue())
			})

			It("should handle multiple pods independently", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
				}

				reconciler.updateRetryInfo(instance, "pod1", 2)
				reconciler.updateRetryInfo(instance, "pod2", 4)

				count1, _ := reconciler.getRetryInfo(instance, "pod1")
				count2, _ := reconciler.getRetryInfo(instance, "pod2")

				Expect(count1).To(Equal(int32(2)))
				Expect(count2).To(Equal(int32(4)))
			})
		})
	})

	Describe("Reconcile Loading Logic", func() {
		var (
			testPods []corev1.Pod
		)

		BeforeEach(func() {
			// Create test pods
			testPods = []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "healthy-pod",
						Namespace: "default",
						Labels:    map[string]string{"model.aibrix.ai/name": "test-model"},
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "unhealthy-pod",
						Namespace: "default",
						Labels:    map[string]string{"model.aibrix.ai/name": "test-model"},
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.2",
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&testPods[0], &testPods[1],
			).Build()

			reconciler = &ModelAdapterReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      recorder,
				RuntimeConfig: config.RuntimeConfig{},
			}
		})

		Context("getActivePodsForModelAdapter", func() {
			It("should return only healthy pods", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
					},
					Spec: modelv1alpha1.ModelAdapterSpec{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"model.aibrix.ai/name": "test-model"},
						},
					},
				}

				pods, err := reconciler.getActivePodsForModelAdapter(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(pods)).To(Equal(1))
				Expect(pods[0].Name).To(Equal("healthy-pod"))
			})
		})
	})

	Describe("Error Recovery and Resilience", func() {
		BeforeEach(func() {
			// Create pods with various states for testing resilience
			goodPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "good-pod",
					Namespace: "default",
					Labels:    map[string]string{"model.aibrix.ai/name": "test-model"},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:               corev1.PodReady,
							Status:             corev1.ConditionTrue,
							LastTransitionTime: metav1.NewTime(time.Now().Add(-60 * time.Second)),
						},
					},
				},
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(goodPod).Build()

			reconciler = &ModelAdapterReconciler{
				Client:        client,
				Scheme:        scheme,
				Recorder:      recorder,
				RuntimeConfig: config.RuntimeConfig{},
				scheduler:     &mockScheduler{},
			}
		})

		Context("Retry annotation management", func() {
			It("should clean up retry annotations after successful operations", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
						Annotations: map[string]string{
							fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey):    "2",
							fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey): time.Now().Format(time.RFC3339),
						},
					},
				}

				reconciler.clearRetryInfo(instance, "test-pod")

				// Verify annotations are removed
				retryKey := fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey)
				timeKey := fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey)

				_, hasRetryKey := instance.Annotations[retryKey]
				_, hasTimeKey := instance.Annotations[timeKey]

				Expect(hasRetryKey).To(BeFalse())
				Expect(hasTimeKey).To(BeFalse())
			})

			It("should preserve other annotations while managing retry info", func() {
				instance := &modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: "default",
						Annotations: map[string]string{
							"user.custom/annotation":                               "should-be-preserved",
							fmt.Sprintf("%s/test-pod", RetryCountAnnotationKey):    "2",
							fmt.Sprintf("%s/test-pod", LastRetryTimeAnnotationKey): time.Now().Format(time.RFC3339),
						},
					},
				}

				reconciler.clearRetryInfo(instance, "test-pod")

				// User annotation should remain
				userAnnotation, exists := instance.Annotations["user.custom/annotation"]
				Expect(exists).To(BeTrue())
				Expect(userAnnotation).To(Equal("should-be-preserved"))
			})
		})
	})

	Describe("Helper Functions", func() {
		Context("getPodNames", func() {
			It("should extract pod names correctly", func() {
				pods := []corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
				}

				names := getPodNames(pods)
				Expect(names).To(Equal([]string{"pod1", "pod2", "pod3"}))
			})

			It("should handle empty pod list", func() {
				pods := []corev1.Pod{}
				names := getPodNames(pods)
				Expect(names).To(Equal([]string{}))
			})
		})

		Context("URL building", func() {
			It("should build correct URLs for different runtime configurations", func() {
				testCases := []struct {
					name           string
					config         config.RuntimeConfig
					podIP          string
					expectedHost   string
					expectedLoader string
				}{
					{
						name:           "Default mode",
						config:         config.RuntimeConfig{DebugMode: false, EnableRuntimeSidecar: false},
						podIP:          "10.0.0.1",
						expectedHost:   "http://10.0.0.1:8000",
						expectedLoader: "http://10.0.0.1:8000/v1/load_lora_adapter",
					},
					{
						name:           "Debug mode",
						config:         config.RuntimeConfig{DebugMode: true, EnableRuntimeSidecar: false},
						podIP:          "10.0.0.1",
						expectedHost:   "http://localhost:30081",
						expectedLoader: "http://localhost:30081/v1/load_lora_adapter",
					},
					{
						name:           "Runtime sidecar mode",
						config:         config.RuntimeConfig{DebugMode: false, EnableRuntimeSidecar: true},
						podIP:          "10.0.0.1",
						expectedHost:   "http://10.0.0.1:8080",
						expectedLoader: "http://10.0.0.1:8080/v1/lora_adapter/load",
					},
				}

				for _, tc := range testCases {
					By(fmt.Sprintf("Testing %s", tc.name))
					urls := BuildURLs(tc.podIP, tc.config)
					Expect(urls.BaseURL).To(Equal(tc.expectedHost))
					Expect(urls.LoadAdapterURL).To(Equal(tc.expectedLoader))
				}
			})
		})
	})
})

// mockScheduler implements the Scheduler interface for testing
type mockScheduler struct {
	selectedPod string
	err         error
}

func (m *mockScheduler) SelectPod(ctx context.Context, model string, readyPods []corev1.Pod) (*corev1.Pod, error) {
	if m.err != nil {
		return nil, m.err
	}

	if m.selectedPod == "" && len(readyPods) > 0 {
		return &readyPods[0], nil
	}

	for i := range readyPods {
		if readyPods[i].Name == m.selectedPod {
			return &readyPods[i], nil
		}
	}

	if len(readyPods) > 0 {
		return &readyPods[0], nil
	}

	return nil, fmt.Errorf("no pods available")
}
