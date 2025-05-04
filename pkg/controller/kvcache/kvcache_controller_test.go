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

package kvcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_getKVCacheBackendFromMetadata(t *testing.T) {
	testCases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		expected    string
	}{
		{
			name: "valid backend annotation - vineyard",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendVineyard,
			},
			expected: constants.KVCacheBackendVineyard,
		},
		{
			name: "valid backend annotation - infinistore",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendInfinistore,
			},
			expected: constants.KVCacheBackendInfinistore,
		},
		{
			name: "invalid backend annotation falls back to default",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: "unknown-backend",
			},
			expected: constants.KVCacheBackendDefault,
		},
		{
			name: "no annotation, distributed mode via annotation",
			annotations: map[string]string{
				constants.KVCacheAnnotationMode: "distributed",
			},
			expected: constants.KVCacheBackendInfinistore,
		},
		{
			name: "no annotation, centralized mode via annotation",
			annotations: map[string]string{
				constants.KVCacheAnnotationMode: "centralized",
			},
			expected: constants.KVCacheBackendVineyard,
		},
		{
			name: "no annotation, unknown mode falls back to default",
			annotations: map[string]string{
				constants.KVCacheAnnotationMode: "invalid-mode",
			},
			expected: constants.KVCacheBackendDefault,
		},
		{
			name:     "no annotation or annotation, falls back to default",
			expected: constants.KVCacheBackendDefault,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
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
			input:    constants.KVCacheBackendVineyard,
			expected: true,
		},
		{
			name:     "valid infinistore backend",
			input:    constants.KVCacheBackendInfinistore,
			expected: true,
		},
		{
			name:     "valid hpkv backend",
			input:    constants.KVCacheBackendHPKV,
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
