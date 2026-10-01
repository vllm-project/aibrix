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

package rayclusterfleet

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/util/expectation"
)

var _ = Describe("RayClusterFleet Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		rayclusterfleet := &orchestrationv1alpha1.RayClusterFleet{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind RayClusterFleet")
			err := k8sClient.Get(ctx, typeNamespacedName, rayclusterfleet)
			if err != nil && errors.IsNotFound(err) {
				labels := map[string]string{
					"app": "rayclusterfleet-test",
				}
				resource := &orchestrationv1alpha1.RayClusterFleet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: orchestrationv1alpha1.RayClusterFleetSpec{

						Replicas: ptr.To(int32(1)),
						Strategy: appsv1.DeploymentStrategy{
							Type: appsv1.RollingUpdateDeploymentStrategyType,
							RollingUpdate: &appsv1.RollingUpdateDeployment{
								MaxUnavailable: ptr.To(intstr.FromInt(0)),
								MaxSurge:       ptr.To(intstr.FromInt(1)),
							},
						},
						Selector: &metav1.LabelSelector{
							MatchLabels: labels,
						},
						Template: orchestrationv1alpha1.RayClusterTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: labels,
							},
							Spec: rayclusterv1.RayClusterSpec{
								RayVersion: "fake-ray-version",
								HeadGroupSpec: rayclusterv1.HeadGroupSpec{
									ServiceType: corev1.ServiceTypeClusterIP,
									Template: corev1.PodTemplateSpec{
										Spec: corev1.PodSpec{
											Containers: []corev1.Container{
												{
													Name:  "ray-head",
													Image: "rayproject/ray:2.10.0",
												},
											},
										},
									},
									RayStartParams: map[string]string{
										"dashboard-host": "0.0.0.0",
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				Expect(k8sClient.Get(ctx, typeNamespacedName, rayclusterfleet)).To(Succeed())
			}
		})

		AfterEach(func() {
			replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					Expect(k8sClient.Delete(ctx, &replicaSets.Items[i])).To(Succeed())
				}
			}

			resource := &orchestrationv1alpha1.RayClusterFleet{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RayClusterFleet")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &RayClusterFleetReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				Expectation: expectation.NewControllerExpectations(),
				Recorder:    record.NewFakeRecorder(10),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the Fleet owns one correctly configured ReplicaSet")
			replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			ownedReplicaSets := make([]*orchestrationv1alpha1.RayClusterReplicaSet, 0, len(replicaSets.Items))
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					ownedReplicaSets = append(ownedReplicaSets, &replicaSets.Items[i])
				}
			}
			Expect(ownedReplicaSets).To(HaveLen(1))

			replicaSet := ownedReplicaSets[0]
			firstReplicaSetName := replicaSet.Name
			firstReplicaSetUID := replicaSet.UID
			Expect(metav1.IsControlledBy(replicaSet, rayclusterfleet)).To(BeTrue())
			Expect(replicaSet.Spec.Replicas).NotTo(BeNil())
			Expect(*replicaSet.Spec.Replicas).To(Equal(int32(1)))
			Expect(replicaSet.Spec.Selector).NotTo(BeNil())
			Expect(replicaSet.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app", "rayclusterfleet-test"))
			Expect(replicaSet.Spec.Template.Labels).To(HaveKeyWithValue("app", "rayclusterfleet-test"))

			By("Reconciling again without creating a duplicate ReplicaSet")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			ownedReplicaSets = ownedReplicaSets[:0]
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					ownedReplicaSets = append(ownedReplicaSets, &replicaSets.Items[i])
				}
			}
			Expect(ownedReplicaSets).To(HaveLen(1))
			Expect(ownedReplicaSets[0].Name).To(Equal(firstReplicaSetName))
			Expect(ownedReplicaSets[0].UID).To(Equal(firstReplicaSetUID))
		})
	})
})
