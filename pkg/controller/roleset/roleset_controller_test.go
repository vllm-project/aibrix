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

package roleset

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

var _ = Describe("RoleSet Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		var controllerReconciler *RoleSetReconciler

		reconcileOnce := func(g Gomega) {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			g.Expect(err).NotTo(HaveOccurred())
		}

		listPods := func() []corev1.Pod {
			pods := &corev1.PodList{}
			Expect(k8sClient.List(ctx, pods, client.InNamespace(typeNamespacedName.Namespace),
				client.MatchingLabels{constants.RoleSetNameLabelKey: resourceName})).To(Succeed())
			return pods.Items
		}

		BeforeEach(func() {
			controllerReconciler = &RoleSetReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				EventRecorder: record.NewFakeRecorder(100),
				DynamicClient: dynamic.NewForConfigOrDie(cfg),
			}

			By("creating a RoleSet with one role of two replicas")
			resource := &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: typeNamespacedName.Namespace,
				},
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{{
						Name:     "worker",
						Replicas: ptr.To(int32(2)),
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "worker", Image: "busybox"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			By("deleting the RoleSet and reconciling until its finalizer has removed the pods")
			resource := &orchestrationv1alpha1.RoleSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			Eventually(func(g Gomega) {
				reconcileOnce(g)
				err := k8sClient.Get(ctx, typeNamespacedName, &orchestrationv1alpha1.RoleSet{})
				g.Expect(errors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())
			Expect(listPods()).To(BeEmpty())
		})

		It("should add its finalizer and create the role's pods", func() {
			By("reconciling once to add the finalizer")
			reconcileOnce(Default)
			resource := &orchestrationv1alpha1.RoleSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Finalizers).To(ContainElement(RoleSetFinalizer))

			By("reconciling again to create the pods")
			reconcileOnce(Default)
			pods := listPods()
			Expect(pods).To(HaveLen(2))
			for i := range pods {
				Expect(metav1.IsControlledBy(&pods[i], resource)).To(BeTrue(),
					"pod %s should be controlled by the RoleSet", pods[i].Name)
			}
		})
	})
})
