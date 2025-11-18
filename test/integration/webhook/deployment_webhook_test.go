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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vllm-project/aibrix/pkg/webhook"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
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
