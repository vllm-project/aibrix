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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

var _ = ginkgo.Describe("kvcache default and validation", func() {
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
		var kvcacheList orchestrationapi.KVCacheList
		gomega.Expect(k8sClient.List(ctx, &kvcacheList)).To(gomega.Succeed())

		for _, item := range kvcacheList.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

	type testDefaultingCase struct {
		kvcache     func() *orchestrationapi.KVCache
		wantKvcache func() *orchestrationapi.KVCache
	}
	ginkgo.DescribeTable("Defaulting test",
		func(tc *testDefaultingCase) {
			model := tc.kvcache()
			gomega.Expect(k8sClient.Create(ctx, model)).To(gomega.Succeed())
			gomega.Expect(model).To(gomega.BeComparableTo(tc.wantKvcache(),
				cmpopts.IgnoreTypes(orchestrationapi.KVCacheStatus{}),
				cmpopts.IgnoreFields(orchestrationapi.KVCacheSpec{}, "Service"),
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID",
					"ResourceVersion", "Generation", "CreationTimestamp", "ManagedFields")))
		},
		ginkgo.Entry("apply kvcache with no backend specified", &testDefaultingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster-no-backend").
					Namespace(ns.Name).
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/link-type", "Ethernet").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Metadata: &orchestrationapi.MetadataSpec{},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{},
					Cache:   orchestrationapi.RuntimeSpec{},
				}
				return kv
			},
			wantKvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster-no-backend").
					Namespace(ns.Name).
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/link-type", "Ethernet").
					Annotation("kvcache.orchestration.aibrix.ai/backend", "vineyard").
					Annotation("kvcache.orchestration.aibrix.ai/mode", "vineyard").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Mode:     "distributed",
					Metadata: &orchestrationapi.MetadataSpec{},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{
						Replicas:        1,
						ImagePullPolicy: string(corev1.PullIfNotPresent),
						Env:             []corev1.EnvVar{},
					},
					Cache: orchestrationapi.RuntimeSpec{
						Replicas:        1,
						ImagePullPolicy: string(corev1.PullIfNotPresent),
						Env:             []corev1.EnvVar{},
					},
				}
				return kv
			},
		}),
	)

	type testValidatingCase struct {
		kvcache func() *orchestrationapi.KVCache
		failed  bool
	}
	ginkgo.DescribeTable("test validating",
		func(tc *testValidatingCase) {
			if tc.failed {
				gomega.Expect(k8sClient.Create(ctx, tc.kvcache())).Should(gomega.HaveOccurred())
			} else {
				gomega.Expect(k8sClient.Create(ctx, tc.kvcache())).To(gomega.Succeed())
			}
		},
		ginkgo.Entry("normal creation", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster").
					Namespace(ns.Name).
					Annotation("kvcache.orchestration.aibrix.ai/backend", "infinistore").
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/link-type", "Ethernet").
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/hint-gid-index", "7").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Metadata: &orchestrationapi.MetadataSpec{
						Redis: &orchestrationapi.MetadataConfig{
							Runtime: &orchestrationapi.RuntimeSpec{
								Image:    "aibrix-cn-beijing.cr.volces.com/aibrix/redis:7.4.2",
								Replicas: 1,
							},
						},
					},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{
						Image:           "aibrix-cn-beijing.cr.volces.com/aibrix/kvcache-watcher:v0.3.0",
						ImagePullPolicy: string(corev1.PullAlways),
					},
					Cache: orchestrationapi.RuntimeSpec{
						Replicas:        1,
						Image:           "aibrix-cn-beijing.cr.volces.com/aibrix/infinistore:v0.2.42-20250506",
						ImagePullPolicy: string(corev1.PullIfNotPresent),
					},
				}
				return kv
			},
			failed: false,
		}),
		ginkgo.Entry("invalid backend", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster-invalid-backend").
					Namespace(ns.Name).
					Annotation("kvcache.orchestration.aibrix.ai/backend", "unsupported_backend").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Metadata: &orchestrationapi.MetadataSpec{},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{},
					Cache:   orchestrationapi.RuntimeSpec{},
				}
				return kv
			},
			failed: true,
		}),
		ginkgo.Entry("no backend specified", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster-no-backend").
					Namespace(ns.Name).
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/link-type", "Ethernet").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Metadata: &orchestrationapi.MetadataSpec{},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{},
					Cache:   orchestrationapi.RuntimeSpec{},
				}
				return kv
			},
			failed: false,
		}),
		ginkgo.Entry("mode determines backend", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := wrapper.MakeKVCache("kvcache-cluster-mode-backend").
					Namespace(ns.Name).
					Annotation("infinistore.kvcache.orchestration.aibrix.ai/mode", "distributed").
					Obj()

				kv.Spec = orchestrationapi.KVCacheSpec{
					Metadata: &orchestrationapi.MetadataSpec{},
					Service: orchestrationapi.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{
							{
								Name:       "service",
								Port:       12345,
								TargetPort: intstr.FromInt(12345),
								Protocol:   corev1.ProtocolTCP,
							},
							{
								Name:       "admin",
								Port:       8088,
								TargetPort: intstr.FromInt(8088),
								Protocol:   corev1.ProtocolTCP,
							},
						},
					},
					Watcher: &orchestrationapi.RuntimeSpec{},
					Cache:   orchestrationapi.RuntimeSpec{},
				}
				return kv
			},
			failed: false,
		}),
	)
})
