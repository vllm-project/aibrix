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

package stormservice

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

var _ = Describe("StormService Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		var controllerReconciler *StormServiceReconciler

		reconcileOnce := func(g Gomega) {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			g.Expect(err).NotTo(HaveOccurred())
		}

		listRoleSets := func() []orchestrationv1alpha1.RoleSet {
			roleSets := &orchestrationv1alpha1.RoleSetList{}
			Expect(k8sClient.List(ctx, roleSets, client.InNamespace(typeNamespacedName.Namespace),
				client.MatchingLabels{constants.StormServiceNameLabelKey: resourceName})).To(Succeed())
			return roleSets.Items
		}

		BeforeEach(func() {
			controllerReconciler = &StormServiceReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				EventRecorder: record.NewFakeRecorder(100),
			}

			By("creating a StormService with one replica of a single-role RoleSet")
			labels := map[string]string{"app": resourceName}
			resource := &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{{
								Name:     "worker",
								Replicas: ptr.To(int32(1)),
								Template: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{Name: "worker", Image: "busybox"}},
									},
								},
							}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			By("deleting the StormService and reconciling until its finalizer has removed the RoleSets")
			resource := &orchestrationv1alpha1.StormService{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				reconcileOnce(g)
				err := k8sClient.Get(ctx, typeNamespacedName, &orchestrationv1alpha1.StormService{})
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())
			Expect(listRoleSets()).To(BeEmpty())
		})

		It("should add its finalizer and create a RoleSet from the template", func() {
			By("reconciling the created resource")
			reconcileOnce(Default)

			resource := &orchestrationv1alpha1.StormService{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Finalizers).To(ContainElement(StormServiceFinalizer))

			roleSets := listRoleSets()
			Expect(roleSets).To(HaveLen(1))
			Expect(metav1.IsControlledBy(&roleSets[0], resource)).To(BeTrue(),
				"roleset %s should be controlled by the StormService", roleSets[0].Name)
			Expect(roleSets[0].Spec.Roles).To(HaveLen(1))
			Expect(roleSets[0].Spec.Roles[0].Name).To(Equal("worker"))
		})
	})
})
