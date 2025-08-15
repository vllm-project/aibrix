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
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/webhook"
)

var _ = ginkgo.Describe("stormservice default webhook", func() {
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
		var stormservices orchestrationapi.StormServiceList
		gomega.Expect(k8sClient.List(ctx, &stormservices)).To(gomega.Succeed())

		for _, item := range stormservices.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

	type testDefaultingCase struct {
		stormservice     func() *orchestrationapi.StormService
		wantStormService func() *orchestrationapi.StormService
	}
	ginkgo.DescribeTable("Defaulting test",
		func(tc *testDefaultingCase) {
			model := tc.stormservice()
			gomega.Expect(k8sClient.Create(ctx, model)).To(gomega.Succeed())
			gomega.Expect(model).To(gomega.BeComparableTo(tc.wantStormService(),
				cmpopts.IgnoreTypes(orchestrationapi.StormServiceStatus{}),
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID",
					"ResourceVersion", "Generation", "CreationTimestamp", "ManagedFields")),
			)
		},
		ginkgo.Entry("apply StormService with no sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return makeStormServiceWithNoSidecarInjection(
					"stormservice-with-no-inject-sidecar", ns.Name, nil)
			},
			wantStormService: func() *orchestrationapi.StormService {
				return makeStormServiceWithNoSidecarInjection(
					"stormservice-with-no-inject-sidecar", ns.Name, nil)
			},
		}),
		ginkgo.Entry("apply StormService with sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return makeStormServiceWithNoSidecarInjection("stormservice-with-inject-sidecar",
					ns.Name, map[string]string{webhook.SidecarInjectionAnnotation: "true"})
			},
			//nolint:dupl
			wantStormService: func() *orchestrationapi.StormService {
				return &orchestrationapi.StormService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "stormservice-with-inject-sidecar",
						Namespace: ns.Name,
						Annotations: map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
						},
					},
					Spec: orchestrationapi.StormServiceSpec{
						Replicas: ptr.To(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "my-ai-app",
							},
						},
						Stateful: true,
						Template: orchestrationapi.RoleSetTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "my-ai-app",
								},
							},
							Spec: &orchestrationapi.RoleSetSpec{
								UpdateStrategy: orchestrationapi.InterleaveRoleSetStrategyType,
								Roles: []orchestrationapi.RoleSpec{
									{
										Name:         "worker",
										Replicas:     ptr.To(int32(1)),
										UpgradeOrder: ptr.To(int32(1)),
										Stateful:     false,
										Template: v1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{
												Labels: map[string]string{
													"role": "worker",
													"app":  "my-ai-app",
												},
											},
											Spec: v1.PodSpec{
												Containers: []v1.Container{
													{
														Name:  "aibrix-runtime",
														Image: "aibrix/runtime:v0.4.0",
														Command: []string{
															"aibrix_runtime",
															"--port", "8080",
														},
														Ports: []v1.ContainerPort{
															{
																Name:          "metrics",
																ContainerPort: 8080,
																Protocol:      "TCP",
															},
														},
														Env: []v1.EnvVar{
															{
																Name:  "INFERENCE_ENGINE",
																Value: "vllm",
															},
															{
																Name:  "INFERENCE_ENGINE_ENDPOINT",
																Value: "http://localhost:8000",
															},
														},
														Resources: v1.ResourceRequirements{
															Requests: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("100m"),
																corev1.ResourceMemory: resource.MustParse("256Mi"),
															},
															Limits: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("500m"),
																corev1.ResourceMemory: resource.MustParse("512Mi"),
															},
														},
														LivenessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/healthz",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 3,
															PeriodSeconds:       2,
														},
														ReadinessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/ready",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 5,
															PeriodSeconds:       10,
														},
													},
													{
														Name:  "vllm-worker",
														Image: "vllm/vllm-openai:latest",
														Args: []string{
															"--model", "meta-llama/Llama-3-8b",
															"--tensor-parallel-size", "1",
														},
														Ports: []v1.ContainerPort{
															{ContainerPort: 8000, Protocol: "TCP"},
														},
														Resources: v1.ResourceRequirements{
															Requests: v1.ResourceList{
																v1.ResourceCPU:    resource.MustParse("4"),
																v1.ResourceMemory: resource.MustParse("16Gi"),
															},
														},
													},
												},
											},
										},
									},
									{
										Name:         "master",
										Replicas:     ptr.To(int32(1)),
										UpgradeOrder: ptr.To(int32(1)),
										Stateful:     true,
										Template: v1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{
												Labels: map[string]string{
													"role": "master",
													"app":  "my-ai-app",
												},
											},
											Spec: v1.PodSpec{
												Containers: []v1.Container{
													{
														Name:  "aibrix-runtime",
														Image: "aibrix/runtime:v0.4.0",
														Command: []string{
															"aibrix_runtime",
															"--port", "8080",
														},
														Ports: []v1.ContainerPort{
															{
																Name:          "metrics",
																ContainerPort: 8080,
																Protocol:      "TCP",
															},
														},
														Env: []v1.EnvVar{
															{
																Name:  "INFERENCE_ENGINE",
																Value: "vllm",
															},
															{
																Name:  "INFERENCE_ENGINE_ENDPOINT",
																Value: "http://localhost:8000",
															},
														},
														Resources: v1.ResourceRequirements{
															Requests: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("100m"),
																corev1.ResourceMemory: resource.MustParse("256Mi"),
															},
															Limits: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("500m"),
																corev1.ResourceMemory: resource.MustParse("512Mi"),
															},
														},
														LivenessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/healthz",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 3,
															PeriodSeconds:       2,
														},
														ReadinessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/ready",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 5,
															PeriodSeconds:       10,
														},
													},
													{
														Name:  "vllm-master",
														Image: "vllm/vllm-openai:latest",
														Args: []string{
															"--model", "meta-llama/Llama-3-8b",
															"--tensor-parallel-size", "1",
														},
														Ports: []v1.ContainerPort{
															{ContainerPort: 8000, Protocol: "TCP"},
														},
														Resources: v1.ResourceRequirements{
															Requests: v1.ResourceList{
																v1.ResourceCPU:    resource.MustParse("4"),
																v1.ResourceMemory: resource.MustParse("16Gi"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}
			},
		}),
		ginkgo.Entry("apply StormService with sidecar injection annotation "+
			"and sidecar runtime image annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return makeStormServiceWithNoSidecarInjection("stormservice-with-inject-sidecar",
					ns.Name, map[string]string{
						webhook.SidecarInjectionAnnotation: "true",
						//nolint:lll
						webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
					})
			},
			//nolint:dupl
			wantStormService: func() *orchestrationapi.StormService {
				return &orchestrationapi.StormService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "stormservice-with-inject-sidecar",
						Namespace: ns.Name,
						Annotations: map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
							//nolint:lll
							webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
						},
					},
					Spec: orchestrationapi.StormServiceSpec{
						Replicas: ptr.To(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "my-ai-app",
							},
						},
						Stateful: true,
						Template: orchestrationapi.RoleSetTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "my-ai-app",
								},
							},
							Spec: &orchestrationapi.RoleSetSpec{
								UpdateStrategy: orchestrationapi.InterleaveRoleSetStrategyType,
								Roles: []orchestrationapi.RoleSpec{
									{
										Name:         "worker",
										Replicas:     ptr.To(int32(1)),
										UpgradeOrder: ptr.To(int32(1)),
										Stateful:     false,
										Template: v1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{
												Labels: map[string]string{
													"role": "worker",
													"app":  "my-ai-app",
												},
											},
											Spec: v1.PodSpec{
												Containers: []v1.Container{
													{
														Name: "aibrix-runtime",
														Image: "aibrix-container-registry-cn-beijing.cr.volces.com" +
															"/aibrix/runtime:v0.4.0",
														Command: []string{
															"aibrix_runtime",
															"--port", "8080",
														},
														Ports: []v1.ContainerPort{
															{
																Name:          "metrics",
																ContainerPort: 8080,
																Protocol:      "TCP",
															},
														},
														Env: []v1.EnvVar{
															{
																Name:  "INFERENCE_ENGINE",
																Value: "vllm",
															},
															{
																Name:  "INFERENCE_ENGINE_ENDPOINT",
																Value: "http://localhost:8000",
															},
														},
														Resources: v1.ResourceRequirements{
															Requests: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("100m"),
																corev1.ResourceMemory: resource.MustParse("256Mi"),
															},
															Limits: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("500m"),
																corev1.ResourceMemory: resource.MustParse("512Mi"),
															},
														},
														LivenessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/healthz",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 3,
															PeriodSeconds:       2,
														},
														ReadinessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/ready",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 5,
															PeriodSeconds:       10,
														},
													},
													{
														Name:  "vllm-worker",
														Image: "vllm/vllm-openai:latest",
														Args: []string{
															"--model", "meta-llama/Llama-3-8b",
															"--tensor-parallel-size", "1",
														},
														Ports: []v1.ContainerPort{
															{ContainerPort: 8000, Protocol: "TCP"},
														},
														Resources: v1.ResourceRequirements{
															Requests: v1.ResourceList{
																v1.ResourceCPU:    resource.MustParse("4"),
																v1.ResourceMemory: resource.MustParse("16Gi"),
															},
														},
													},
												},
											},
										},
									},
									{
										Name:         "master",
										Replicas:     ptr.To(int32(1)),
										UpgradeOrder: ptr.To(int32(1)),
										Stateful:     true,
										Template: v1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{
												Labels: map[string]string{
													"role": "master",
													"app":  "my-ai-app",
												},
											},
											Spec: v1.PodSpec{
												Containers: []v1.Container{
													{
														Name: "aibrix-runtime",
														Image: "aibrix-container-registry-cn-beijing.cr.volces.com" +
															"/aibrix/runtime:v0.4.0",
														Command: []string{
															"aibrix_runtime",
															"--port", "8080",
														},
														Ports: []v1.ContainerPort{
															{
																Name:          "metrics",
																ContainerPort: 8080,
																Protocol:      "TCP",
															},
														},
														Env: []v1.EnvVar{
															{
																Name:  "INFERENCE_ENGINE",
																Value: "vllm",
															},
															{
																Name:  "INFERENCE_ENGINE_ENDPOINT",
																Value: "http://localhost:8000",
															},
														},
														Resources: v1.ResourceRequirements{
															Requests: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("100m"),
																corev1.ResourceMemory: resource.MustParse("256Mi"),
															},
															Limits: corev1.ResourceList{
																corev1.ResourceCPU:    resource.MustParse("500m"),
																corev1.ResourceMemory: resource.MustParse("512Mi"),
															},
														},
														LivenessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/healthz",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 3,
															PeriodSeconds:       2,
														},
														ReadinessProbe: &v1.Probe{
															ProbeHandler: v1.ProbeHandler{
																HTTPGet: &v1.HTTPGetAction{
																	Path: "/ready",
																	Port: intstr.FromInt(8080),
																},
															},
															InitialDelaySeconds: 5,
															PeriodSeconds:       10,
														},
													},
													{
														Name:  "vllm-master",
														Image: "vllm/vllm-openai:latest",
														Args: []string{
															"--model", "meta-llama/Llama-3-8b",
															"--tensor-parallel-size", "1",
														},
														Ports: []v1.ContainerPort{
															{ContainerPort: 8000, Protocol: "TCP"},
														},
														Resources: v1.ResourceRequirements{
															Requests: v1.ResourceList{
																v1.ResourceCPU:    resource.MustParse("4"),
																v1.ResourceMemory: resource.MustParse("16Gi"),
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}
			},
		}),
	)
})

func makeStormServiceWithNoSidecarInjection(name, namespace string,
	annotations map[string]string) *orchestrationapi.StormService {
	return &orchestrationapi.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: orchestrationapi.StormServiceSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "my-ai-app",
				},
			},
			Stateful: true,
			Template: orchestrationapi.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "my-ai-app",
					},
				},
				Spec: &orchestrationapi.RoleSetSpec{
					UpdateStrategy: orchestrationapi.InterleaveRoleSetStrategyType,
					Roles: []orchestrationapi.RoleSpec{
						{
							Name:         "worker",
							Replicas:     ptr.To(int32(1)),
							UpgradeOrder: ptr.To(int32(1)),
							Stateful:     false,
							Template: v1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"role": "worker",
										"app":  "my-ai-app",
									},
								},
								Spec: v1.PodSpec{
									Containers: []v1.Container{
										{
											Name:  "vllm-worker",
											Image: "vllm/vllm-openai:latest",
											Args: []string{
												"--model", "meta-llama/Llama-3-8b",
												"--tensor-parallel-size", "1",
											},
											Ports: []v1.ContainerPort{
												{ContainerPort: 8000, Protocol: "TCP"},
											},
											Resources: v1.ResourceRequirements{
												Requests: v1.ResourceList{
													v1.ResourceCPU:    resource.MustParse("4"),
													v1.ResourceMemory: resource.MustParse("16Gi"),
												},
											},
										},
									},
								},
							},
						},
						{
							Name:         "master",
							Replicas:     ptr.To(int32(1)),
							UpgradeOrder: ptr.To(int32(1)),
							Stateful:     true,
							Template: v1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"role": "master",
										"app":  "my-ai-app",
									},
								},
								Spec: v1.PodSpec{
									Containers: []v1.Container{
										{
											Name:  "vllm-master",
											Image: "vllm/vllm-openai:latest",
											Args: []string{
												"--model", "meta-llama/Llama-3-8b",
												"--tensor-parallel-size", "1",
											},
											Ports: []v1.ContainerPort{
												{ContainerPort: 8000, Protocol: "TCP"},
											},
											Resources: v1.ResourceRequirements{
												Requests: v1.ResourceList{
													v1.ResourceCPU:    resource.MustParse("4"),
													v1.ResourceMemory: resource.MustParse("16Gi"),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
