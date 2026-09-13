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

package webhook

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

var _ = ginkgo.Describe("RayClusterFleet selector admission", func() {
	var ns *corev1.Namespace
	var fleet *orchestrationapi.RayClusterFleet

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "rayclusterfleet-admission-"},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		fleet = &orchestrationapi.RayClusterFleet{
			ObjectMeta: metav1.ObjectMeta{Name: "qwen-coder-7b-instruct", Namespace: ns.Name},
			Spec: orchestrationapi.RayClusterFleetSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{constants.ModelLabelName: "qwen-coder-7b-instruct"},
				},
				Template: orchestrationapi.RayClusterTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.ModelLabelName: "qwen-coder-7b-instruct"},
					},
					Spec: rayv1.RayClusterSpec{
						HeadGroupSpec: rayv1.HeadGroupSpec{
							RayStartParams: map[string]string{},
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{constants.ModelLabelName: "qwen-coder-7b-instruct-b"},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "ray-head", Image: "rayproject/ray:2.10.0"}},
								},
							},
						},
					},
				},
			},
		}
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.It("rejects the reported selector and template label mismatch on create", func() {
		fleet.Spec.Template.Labels[constants.ModelLabelName] = "qwen-coder-7b-instruct-a"
		err := k8sClient.Create(ctx, fleet)
		ginkgo.GinkgoWriter.Printf("Create mismatched Fleet: %v\n", err)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "the API must reject mismatched selector and template labels")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("selector does not match template labels")))
	})

	ginkgo.It("rejects an update that no longer matches the selector", func() {
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())
		fleet.Spec.Template.Labels[constants.ModelLabelName] = "qwen-coder-7b-instruct-a"
		err := k8sClient.Update(ctx, fleet)
		ginkgo.GinkgoWriter.Printf("Update mismatched Fleet: %v\n", err)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue())

		stored := &orchestrationapi.RayClusterFleet{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), stored)).To(gomega.Succeed())
		gomega.Expect(stored.Spec.Template.Labels).To(gomega.HaveKeyWithValue(constants.ModelLabelName, "qwen-coder-7b-instruct"))
	})

	ginkgo.It("accepts matching RayCluster labels when the head Pod has different labels", func() {
		err := k8sClient.Create(ctx, fleet)
		ginkgo.GinkgoWriter.Printf("Create matching Fleet with distinct head Pod labels: %v\n", err)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("accepts a matching expression selector", func() {
		fleet.Spec.Selector = &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      constants.ModelLabelName,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"qwen-coder-7b-instruct"},
			}},
		}
		err := k8sClient.Create(ctx, fleet)
		ginkgo.GinkgoWriter.Printf("Create matching expression selector: %v\n", err)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
