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

package kvcache

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache/backends"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("KVCache Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		kvcache := &orchestrationv1alpha1.KVCache{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind KVCache")
			err := k8sClient.Get(ctx, typeNamespacedName, kvcache)
			if err != nil && errors.IsNotFound(err) {
				resource := &orchestrationv1alpha1.KVCache{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
						Labels: map[string]string{
							constants.KVCacheLabelKeyBackend: constants.KVCacheBackendVineyard,
						},
					},
					Spec: orchestrationv1alpha1.KVCacheSpec{
						Metadata: &orchestrationv1alpha1.MetadataSpec{
							Redis: &orchestrationv1alpha1.MetadataConfig{
								Runtime: &orchestrationv1alpha1.RuntimeSpec{
									Replicas: 1,
									Image:    "redis:7.4.1",
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
						Cache: orchestrationv1alpha1.RuntimeSpec{
							Replicas: 3,
							Image:    "aibrix/infinistore:20250502",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:                             resource.MustParse("8"),
									corev1.ResourceMemory:                          resource.MustParse("50Gi"),
									corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                             resource.MustParse("8"),
									corev1.ResourceMemory:                          resource.MustParse("50Gi"),
									corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
								},
							},
						},
						Watcher: &orchestrationv1alpha1.RuntimeSpec{
							Replicas: 1,
							Image:    "aibrix/kvcache-watcher:nightly",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("256"),
								},
							},
						},
						Service: orchestrationv1alpha1.ServiceSpec{
							Type: corev1.ClusterIPNone,
							Ports: []corev1.ServicePort{
								{
									Name: "rdma",
									Port: 12345,
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &orchestrationv1alpha1.KVCache{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance KVCache")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &KVCacheReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Backends: map[string]backends.BackendReconciler{
					constants.KVCacheBackendVineyard:    backends.NewVineyardReconciler(k8sClient),
					constants.KVCacheBackendHPKV:        backends.NewDistributedReconciler(k8sClient, constants.KVCacheBackendHPKV),
					constants.KVCacheBackendInfinistore: backends.NewDistributedReconciler(k8sClient, constants.KVCacheBackendInfinistore),
				},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
