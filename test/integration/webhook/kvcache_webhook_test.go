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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
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
		var adapters modelapi.ModelAdapterList
		gomega.Expect(k8sClient.List(ctx, &adapters)).To(gomega.Succeed())

		for _, item := range adapters.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

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
				kv := &orchestrationapi.KVCache{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kvcache-cluster",
						Namespace: "default",
						Annotations: map[string]string{
							"kvcache.orchestration.aibrix.ai/backend":                    "infinistore",
							"infinistore.kvcache.orchestration.aibrix.ai/link-type":      "Ethernet",
							"infinistore.kvcache.orchestration.aibrix.ai/hint-gid-index": "7",
						},
					},
					Spec: orchestrationapi.KVCacheSpec{
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
					},
				}
				return kv
			},
			failed: false,
		}),
		ginkgo.Entry("invalid backend", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := &orchestrationapi.KVCache{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kvcache-cluster-invalid-backend",
						Namespace: ns.Name,
						Annotations: map[string]string{
							"kvcache.orchestration.aibrix.ai/backend": "unsupported_backend",
						},
					},
				}
				return kv
			},
			failed: true,
		}),
		ginkgo.Entry("no backend specified", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := &orchestrationapi.KVCache{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kvcache-cluster-no-backend",
						Namespace: ns.Name,
						Annotations: map[string]string{
							"infinistore.kvcache.orchestration.aibrix.ai/link-type": "Ethernet",
						},
					},
				}
				return kv
			},
			failed: false,
		}),
		ginkgo.Entry("mode determines backend", &testValidatingCase{
			kvcache: func() *orchestrationapi.KVCache {
				kv := &orchestrationapi.KVCache{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kvcache-cluster-mode-backend",
						Namespace: ns.Name,
						Annotations: map[string]string{
							"infinistore.kvcache.orchestration.aibrix.ai/mode": "distributed",
						},
					},
				}
				return kv
			},
			failed: false,
		}),
	)
})
