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

package backends

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildRedisPod(t *testing.T) {
	testCases := []struct {
		name           string
		cacheImage     string
		expectedImage  string
		expectedLabels map[string]string
	}{
		{
			name:          "default image",
			cacheImage:    "redis:7-alpine",
			expectedImage: "redis:7-alpine",
			expectedLabels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: "test-cache",
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cache",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.KVCacheSpec{
					Cache: orchestrationv1alpha1.RuntimeSpec{
						Image: tc.cacheImage,
					},
					Metadata: &orchestrationv1alpha1.MetadataSpec{
						Redis: &orchestrationv1alpha1.MetadataConfig{
							Runtime: &orchestrationv1alpha1.RuntimeSpec{
								Image: tc.cacheImage,
							},
						},
					},
				},
			}

			pod := buildRedisPod(kv)

			assert.Equal(t, "test-cache-redis", pod.Name)
			assert.Equal(t, "default", pod.Namespace)
			assert.Equal(t, tc.expectedImage, pod.Spec.Containers[0].Image)

			for k, v := range tc.expectedLabels {
				assert.Equal(t, v, pod.Labels[k])
			}

			assert.Equal(t, int32(6379), pod.Spec.Containers[0].Ports[0].ContainerPort)
		})
	}
}

func TestBuildRedisService(t *testing.T) {
	testCases := []struct {
		name           string
		cacheName      string
		namespace      string
		expectedLabels map[string]string
	}{
		{
			name:      "basic service construction",
			cacheName: "my-cache",
			namespace: "default",
			expectedLabels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: "my-cache",
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
		},
		{
			name:      "custom namespace",
			cacheName: "cache-x",
			namespace: "kvspace",
			expectedLabels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: "cache-x",
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.cacheName,
					Namespace: tc.namespace,
				},
			}

			svc := buildRedisService(kv)

			// Metadata checks
			assert.Equal(t, fmt.Sprintf("%s-redis", tc.cacheName), svc.Name)
			assert.Equal(t, tc.namespace, svc.Namespace)

			// Labels and selectors
			for k, v := range tc.expectedLabels {
				assert.Equal(t, v, svc.Labels[k])
				assert.Equal(t, v, svc.Spec.Selector[k])
			}

			// Ports
			require.Len(t, svc.Spec.Ports, 1)
			assert.Equal(t, int32(6379), svc.Spec.Ports[0].Port)
			assert.Equal(t, intstr.FromInt32(6379), svc.Spec.Ports[0].TargetPort)
			assert.Equal(t, corev1.ProtocolTCP, svc.Spec.Ports[0].Protocol)

			// Type
			assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)

			// OwnerReference
			require.Len(t, svc.OwnerReferences, 1)
			assert.Equal(t, "KVCache", svc.OwnerReferences[0].Kind)
			assert.Equal(t, tc.cacheName, svc.OwnerReferences[0].Name)
		})
	}
}
