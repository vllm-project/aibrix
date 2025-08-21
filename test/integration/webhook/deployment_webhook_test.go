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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vllm-project/aibrix/pkg/webhook"
)

var _ = ginkgo.Describe("deployment default webhook", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-deployment-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	type testDefaultingCase struct {
		deployment     func() *appsv1.Deployment
		wantDeployment func() *appsv1.Deployment
	}

	ginkgo.DescribeTable("Defaulting test for Deployment",
		func(tc *testDefaultingCase) {
			dep := tc.deployment()
			gomega.Expect(k8sClient.Create(ctx, dep)).To(gomega.Succeed())
			var createdDep appsv1.Deployment
			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &createdDep)).To(gomega.Succeed())

			wantDep := tc.wantDeployment()
			gomega.Expect(&createdDep).To(gomega.BeComparableTo(wantDep,
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion",
					"Generation", "CreationTimestamp", "ManagedFields"),
				cmpopts.IgnoreFields(appsv1.Deployment{}, "Status"),
				cmpopts.EquateEmpty(),
			))
		},
		ginkgo.Entry("apply Deployment with no sidecar injection annotation",
			&testDefaultingCase{
				deployment: func() *appsv1.Deployment {
					return makeDeployment("dep-no-inject", ns.Name, nil, []corev1.Container{
						{
							Name:  "app-container",
							Image: "nginx:latest",
						},
					})
				},
				wantDeployment: func() *appsv1.Deployment {
					return makeDeployment("dep-no-inject", ns.Name, nil, []corev1.Container{
						{
							Name:  "app-container",
							Image: "nginx:latest",
						},
					})
				},
			},
		),
		ginkgo.Entry("apply Deployment with sidecar injection annotation",
			&testDefaultingCase{
				deployment: func() *appsv1.Deployment {
					return makeDeployment("dep-with-inject", ns.Name,
						map[string]string{webhook.SidecarInjectionAnnotation: "true"},
						[]corev1.Container{
							{
								Name:  "vllm-worker",
								Image: "vllm/vllm-openai:latest",
								Args: []string{
									"--model", "meta-llama/Llama-3-8b",
								},
								Ports: []corev1.ContainerPort{
									{ContainerPort: 8000, Protocol: "TCP"},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("4"),
										corev1.ResourceMemory: resource.MustParse("16Gi"),
									},
								},
							},
						})
				},
				//nolint:dupl
				wantDeployment: func() *appsv1.Deployment {
					return makeDeployment("dep-with-inject", ns.Name,
						map[string]string{webhook.SidecarInjectionAnnotation: "true"},
						[]corev1.Container{
							{
								Name:  "aibrix-runtime",
								Image: "aibrix/runtime:v0.4.0",
								Command: []string{
									"aibrix_runtime",
									"--port", "8080",
								},
								ImagePullPolicy: "IfNotPresent",
								Ports: []corev1.ContainerPort{
									{
										Name:          "metrics",
										ContainerPort: 8080,
										Protocol:      "TCP",
									},
								},
								Env: []corev1.EnvVar{
									{
										Name:  "INFERENCE_ENGINE",
										Value: "vllm",
									},
									{
										Name:  "INFERENCE_ENGINE_ENDPOINT",
										Value: "http://localhost:8000",
									},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("500m"),
										corev1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
								LivenessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/healthz",
											Port:   intstr.FromInt(8080),
											Scheme: "HTTP",
										},
									},
									InitialDelaySeconds: 3,
									TimeoutSeconds:      1,
									SuccessThreshold:    1,
									FailureThreshold:    3,
									PeriodSeconds:       2,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/ready",
											Port:   intstr.FromInt(8080),
											Scheme: "HTTP",
										},
									},
									InitialDelaySeconds: 5,
									PeriodSeconds:       10,
									TimeoutSeconds:      1,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
							},
							{
								Name:  "vllm-worker",
								Image: "vllm/vllm-openai:latest",
								Args: []string{
									"--model", "meta-llama/Llama-3-8b",
								},
								Ports: []corev1.ContainerPort{
									{ContainerPort: 8000, Protocol: "TCP"},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("4"),
										corev1.ResourceMemory: resource.MustParse("16Gi"),
									},
								},
							},
						})
				},
			},
		),
		ginkgo.Entry("apply Deployment with sidecar injection annotation and sidecar "+
			"runtime image annotation",
			&testDefaultingCase{
				deployment: func() *appsv1.Deployment {
					return makeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
							//nolint:lll
							webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
						},
						[]corev1.Container{
							{
								Name:  "vllm-worker",
								Image: "vllm/vllm-openai:latest",
								Args: []string{
									"--model", "meta-llama/Llama-3-8b",
								},
								Ports: []corev1.ContainerPort{
									{ContainerPort: 8000, Protocol: "TCP"},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("4"),
										corev1.ResourceMemory: resource.MustParse("16Gi"),
									},
								},
							},
						})
				},
				//nolint:dupl
				wantDeployment: func() *appsv1.Deployment {
					return makeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
							//nolint:lll
							webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
						},
						[]corev1.Container{
							{
								Name:  "aibrix-runtime",
								Image: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
								Command: []string{
									"aibrix_runtime",
									"--port", "8080",
								},
								ImagePullPolicy: "IfNotPresent",
								Ports: []corev1.ContainerPort{
									{
										Name:          "metrics",
										ContainerPort: 8080,
										Protocol:      "TCP",
									},
								},
								Env: []corev1.EnvVar{
									{
										Name:  "INFERENCE_ENGINE",
										Value: "vllm",
									},
									{
										Name:  "INFERENCE_ENGINE_ENDPOINT",
										Value: "http://localhost:8000",
									},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("500m"),
										corev1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
								LivenessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/healthz",
											Port:   intstr.FromInt(8080),
											Scheme: "HTTP",
										},
									},
									InitialDelaySeconds: 3,
									TimeoutSeconds:      1,
									SuccessThreshold:    1,
									FailureThreshold:    3,
									PeriodSeconds:       2,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/ready",
											Port:   intstr.FromInt(8080),
											Scheme: "HTTP",
										},
									},
									InitialDelaySeconds: 5,
									PeriodSeconds:       10,
									TimeoutSeconds:      1,
									SuccessThreshold:    1,
									FailureThreshold:    3,
								},
							},
							{
								Name:  "vllm-worker",
								Image: "vllm/vllm-openai:latest",
								Args: []string{
									"--model", "meta-llama/Llama-3-8b",
								},
								Ports: []corev1.ContainerPort{
									{ContainerPort: 8000, Protocol: "TCP"},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("4"),
										corev1.ResourceMemory: resource.MustParse("16Gi"),
									},
								},
							},
						})
				},
			},
		),
	)
})

func makeDeployment(name, namespace string, annotations map[string]string,
	containers []corev1.Container) *appsv1.Deployment {
	if annotations == nil {
		annotations = map[string]string{}
	}

	// Set default value
	for i := range containers {
		if containers[i].TerminationMessagePath == "" {
			containers[i].TerminationMessagePath = "/dev/termination-log"
		}
		if containers[i].TerminationMessagePolicy == "" {
			containers[i].TerminationMessagePolicy = "File"
		}
		if containers[i].ImagePullPolicy == "" {
			containers[i].ImagePullPolicy = "Always"
		}
	}

	// Set default value
	podSpec := corev1.PodSpec{
		Containers:                    containers,
		RestartPolicy:                 "Always",
		TerminationGracePeriodSeconds: ptr.To[int64](30),
		DNSPolicy:                     "ClusterFirst",
		SecurityContext:               &corev1.PodSecurityContext{},
		SchedulerName:                 "default-scheduler",
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": name},
					Annotations: annotations,
				},
				Spec: podSpec,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To[intstr.IntOrString](intstr.FromString("25%")),
					MaxSurge:       ptr.To[intstr.IntOrString](intstr.FromString("25%")),
				},
			},
			RevisionHistoryLimit:    ptr.To[int32](10),
			ProgressDeadlineSeconds: ptr.To[int32](600),
		},
	}
}
