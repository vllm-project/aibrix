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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vllm-project/aibrix/pkg/webhook"
	webhookutils "github.com/vllm-project/aibrix/test/utils/webhook"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

var _ = ginkgo.Describe("deployment default webhook", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = nil
		ns = webhookutils.CreateNamespace(ctx, k8sClient, "test-deployment-ns-")
	})

	ginkgo.AfterEach(func() {
		webhookutils.DeleteNamespace(ctx, k8sClient, ns)
	})

	for _, operation := range []string{"create", "update"} {
		ginkgo.DescribeTable("preserves adapter storage on "+operation,
			func(source *corev1.VolumeSource) {
				dep := wrapper.MakeDeployment("dep-storage", ns.Name, nil).
					AddContainer(corev1.Container{Name: "engine", Image: "vllm/vllm-openai:test"}).Obj()
				dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
					Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}}
				if source != nil {
					dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, corev1.Volume{
						Name: webhook.DefaultAdapterVolumeName, VolumeSource: *source.DeepCopy(),
					})
				}
				wantVolumes := dep.Spec.Template.Spec.DeepCopy().Volumes
				if source == nil {
					wantVolumes = append(wantVolumes, corev1.Volume{
						Name:         webhook.DefaultAdapterVolumeName,
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					})
				}
				if operation == "create" {
					dep.Annotations[webhook.SidecarInjectionAnnotation] = "true"
				}
				gomega.Expect(k8sClient.Create(ctx, dep)).To(gomega.Succeed())
				if operation == "update" {
					dep.Annotations = map[string]string{webhook.SidecarInjectionAnnotation: "true"}
					gomega.Expect(k8sClient.Update(ctx, dep)).To(gomega.Succeed())
				}
				var created appsv1.Deployment
				key := client.ObjectKeyFromObject(dep)
				gomega.Expect(k8sClient.Get(ctx, key, &created)).To(gomega.Succeed())
				expectAdapterStorage(&created.Spec.Template.Spec, wantVolumes)

				created.Annotations["test.aibrix.ai/repeated-admission"] = "true"
				gomega.Expect(k8sClient.Update(ctx, &created)).To(gomega.Succeed())
				gomega.Expect(k8sClient.Get(ctx, key, &created)).To(gomega.Succeed())
				expectAdapterStorage(&created.Spec.Template.Spec, wantVolumes)
			},
			ginkgo.Entry("PVC", &corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "adapter-cache",
			}}),
			ginkgo.Entry("hostPath", &corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/var/lib/adapters", Type: ptr.To(corev1.HostPathDirectory),
			}}),
			ginkgo.Entry("configured emptyDir", &corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("64Mi")),
			}}),
			ginkgo.Entry("missing volume", (*corev1.VolumeSource)(nil)),
		)
	}

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
					return wrapper.MakeDeployment("dep-no-inject", ns.Name, nil).
						AddContainer(corev1.Container{
							Name:  "app-container",
							Image: "nginx:latest",
						}).
						Obj()
				},
				wantDeployment: func() *appsv1.Deployment {
					return wrapper.MakeDeployment("dep-no-inject", ns.Name, nil).
						AddContainer(corev1.Container{
							Name:  "app-container",
							Image: "nginx:latest",
						}).
						Obj()
				},
			},
		),
		ginkgo.Entry("apply Deployment with sidecar injection annotation",
			&testDefaultingCase{
				deployment: func() *appsv1.Deployment {
					return wrapper.MakeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
						}).
						AddModelContainer("vllm-worker", "vllm/vllm-openai:latest", []string{
							"--model", "meta-llama/Llama-3-8b",
						}).
						Obj()
				},
				//nolint:dupl
				wantDeployment: func() *appsv1.Deployment {
					return wrapper.MakeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
						}).
						AddRuntimeContainer("aibrix-runtime", "aibrix/runtime:v0.5.0", []string{
							"aibrix_runtime", "--port", "8080",
						}).
						AddModelContainer("vllm-worker", "vllm/vllm-openai:latest", []string{
							"--model", "meta-llama/Llama-3-8b",
						}).
						Obj()
				},
			},
		),
		ginkgo.Entry("apply Deployment with sidecar injection annotation and sidecar "+
			"runtime image annotation",
			&testDefaultingCase{
				deployment: func() *appsv1.Deployment {
					return wrapper.MakeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
							//nolint:lll
							webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
						}).
						AddModelContainer("vllm-worker", "vllm/vllm-openai:latest", []string{
							"--model", "meta-llama/Llama-3-8b",
						}).
						Obj()
				},
				//nolint:dupl
				wantDeployment: func() *appsv1.Deployment {
					return wrapper.MakeDeployment("dep-with-inject", ns.Name,
						map[string]string{
							webhook.SidecarInjectionAnnotation: "true",
							//nolint:lll
							webhook.SidecarInjectionRuntimeImageAnnotation: "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
						}).
						AddRuntimeContainer(
							"aibrix-runtime",
							"aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0",
							[]string{
								"aibrix_runtime",
								"--port", "8080",
							}).
						AddModelContainer("vllm-worker", "vllm/vllm-openai:latest", []string{
							"--model", "meta-llama/Llama-3-8b",
						}).
						Obj()
				},
			},
		),
	)
})

func expectAdapterStorage(spec *corev1.PodSpec, wantVolumes []corev1.Volume) {
	ginkgo.GinkgoHelper()
	gomega.Expect(spec.Volumes).To(gomega.ConsistOf(wantVolumes))
	gomega.Expect(spec.Containers).To(gomega.HaveLen(2))
	gomega.Expect(spec.Containers[0].Name).To(gomega.Equal(webhook.SidecarName))
	for _, container := range spec.Containers {
		gomega.Expect(container.VolumeMounts).To(gomega.Equal([]corev1.VolumeMount{{
			Name: webhook.DefaultAdapterVolumeName, MountPath: webhook.DefaultAdapterMountPath,
		}}))
	}
}
