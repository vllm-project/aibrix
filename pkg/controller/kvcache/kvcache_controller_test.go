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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache/backends"
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
					},
					// TODO(user): Specify other spec details if needed.
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

func Test_getKVCacheBackendFromMetadata(t *testing.T) {
	testCases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		expected    string
	}{
		{
			name: "valid backend label - vineyard",
			labels: map[string]string{
				KVCacheLabelKeyBackend: backends.KVCacheBackendVineyard,
			},
			expected: backends.KVCacheBackendVineyard,
		},
		{
			name: "valid backend label - infinistore",
			labels: map[string]string{
				KVCacheLabelKeyBackend: backends.KVCacheBackendInfinistore,
			},
			expected: backends.KVCacheBackendInfinistore,
		},
		{
			name: "invalid backend label falls back to default",
			labels: map[string]string{
				KVCacheLabelKeyBackend: "unknown-backend",
			},
			expected: backends.KVCacheBackendDefault,
		},
		{
			name: "no label, distributed mode via annotation",
			annotations: map[string]string{
				KVCacheAnnotationMode: "distributed",
			},
			expected: backends.KVCacheBackendInfinistore,
		},
		{
			name: "no label, centralized mode via annotation",
			annotations: map[string]string{
				KVCacheAnnotationMode: "centralized",
			},
			expected: backends.KVCacheBackendVineyard,
		},
		{
			name: "no label, unknown mode falls back to default",
			annotations: map[string]string{
				KVCacheAnnotationMode: "invalid-mode",
			},
			expected: backends.KVCacheBackendDefault,
		},
		{
			name:     "no label or annotation, falls back to default",
			expected: backends.KVCacheBackendDefault,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      tc.labels,
					Annotations: tc.annotations,
				},
			}
			result := getKVCacheBackendFromMetadata(kv)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func Test_isValidKVCacheBackend(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "valid vineyard backend",
			input:    backends.KVCacheBackendVineyard,
			expected: true,
		},
		{
			name:     "valid infinistore backend",
			input:    backends.KVCacheBackendInfinistore,
			expected: true,
		},
		{
			name:     "valid hpkv backend",
			input:    backends.KVCacheBackendHPKV,
			expected: true,
		},
		{
			name:     "invalid backend",
			input:    "not-a-valid-backend",
			expected: false,
		},
		{
			name:     "empty backend",
			input:    "",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isValidKVCacheBackend(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}
