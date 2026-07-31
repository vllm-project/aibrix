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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestGetKVCacheBackendFromMetadata(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    string
	}{
		{
			name:        "backend specified in annotations",
			annotations: map[string]string{constants.KVCacheLabelKeyBackend: constants.KVCacheBackendHPKV},
			expected:    constants.KVCacheBackendHPKV,
		},
		{
			name:        "distributed mode falls back to infinistore",
			annotations: map[string]string{constants.KVCacheAnnotationMode: "distributed"},
			expected:    constants.KVCacheBackendInfinistore,
		},
		{
			name:        "centralized mode falls back to vineyard",
			annotations: map[string]string{constants.KVCacheAnnotationMode: "centralized"},
			expected:    constants.KVCacheBackendVineyard,
		},
		{
			name:        "empty mode uses default",
			annotations: map[string]string{},
			expected:    constants.KVCacheBackendDefault,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := &v1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			result := getKVCacheBackendFromMetadata(kv)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsValidKVCacheBackend(t *testing.T) {
	validBackends := []string{
		constants.KVCacheBackendVineyard,
		constants.KVCacheBackendHPKV,
		constants.KVCacheBackendInfinistore,
	}
	invalidBackends := []string{"", "redis", "unknown"}

	for _, b := range validBackends {
		assert.True(t, isValidKVCacheBackend(b), "%q should be valid", b)
	}

	for _, b := range invalidBackends {
		assert.False(t, isValidKVCacheBackend(b), "%q should be invalid", b)
	}
}

func TestValidateKVCacheBackend(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectError bool
	}{
		{
			name:        "valid backend from annotation",
			annotations: map[string]string{constants.KVCacheLabelKeyBackend: constants.KVCacheBackendHPKV},
			expectError: false,
		},
		{
			name:        "invalid backend from annotation",
			annotations: map[string]string{constants.KVCacheLabelKeyBackend: "redis"},
			expectError: true,
		},
		{
			name:        "missing backend and unsupported mode",
			annotations: map[string]string{constants.KVCacheAnnotationMode: "unknown"},
			expectError: false,
		},
		{
			name:        "no annotations",
			annotations: map[string]string{},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := &v1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			err := ValidateKVCacheBackend(kv)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
