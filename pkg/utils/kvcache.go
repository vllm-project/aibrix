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
	"fmt"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// ValidateKVCacheBackend validates that the backend specified in annotations is valid.
func ValidateKVCacheBackend(kv *orchestrationv1alpha1.KVCache) error {
	backend := getKVCacheBackendFromMetadata(kv)
	if !isValidKVCacheBackend(backend) {
		return fmt.Errorf("invalid backend %q specified, supported backends are: %s, %s, %s",
			backend,
			constants.KVCacheBackendVineyard,
			constants.KVCacheBackendHPKV,
			constants.KVCacheBackendInfinistore)
	}
	return nil
}

// getKVCacheBackendFromMetadata returns the backend based on labels and annotations with fallback logic.
func getKVCacheBackendFromMetadata(kv *orchestrationv1alpha1.KVCache) string {
	backend := kv.Annotations[constants.KVCacheLabelKeyBackend]
	if backend != "" {
		return backend
	}

	mode := kv.Annotations[constants.KVCacheAnnotationMode]
	switch mode {
	case "distributed":
		return constants.KVCacheBackendInfinistore
	case "centralized":
		return constants.KVCacheBackendVineyard
	default:
		return constants.KVCacheBackendDefault
	}
}

// isValidKVCacheBackend returns true if the backend is one of the supported backends.
func isValidKVCacheBackend(b string) bool {
	switch b {
	case constants.KVCacheBackendVineyard, constants.KVCacheBackendHPKV, constants.KVCacheBackendInfinistore:
		return true
	default:
		return false
	}
}
