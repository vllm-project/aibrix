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
)

const (
	KVCacheAnnotationRDMAPort         = "hpkv.kvcache.orchestration.aibrix.ai/rdma-port"
	KVCacheAnnotationAdminPort        = "hpkv.kvcache.orchestration.aibrix.ai/admin-port"
	KVCacheAnnotationBlockSize        = "hpkv.kvcache.orchestration.aibrix.ai/block-size-bytes"
	KVCacheAnnotationBlockCount       = "hpkv.kvcache.orchestration.aibrix.ai/block-count"
	KVCacheAnnotationTotalSlots       = "hpkv.kvcache.orchestration.aibrix.ai/total-slots"
	KVCacheAnnotationVirtualNodeCount = "hpkv.kvcache.orchestration.aibrix.ai/virtual-node-count"
)

func TestGetPortAnnotation(t *testing.T) {
	annotations := map[string]string{
		"hpkv.kvcache.orchestration.aibrix.ai/rdma-port":  "18515",
		"hpkv.kvcache.orchestration.aibrix.ai/admin-port": "9999",
		"invalid-port": "99999", // out of range
	}

	tests := []struct {
		key          string
		defaultValue int
		expected     int
	}{
		{KVCacheAnnotationRDMAPort, 18512, 18515},
		{KVCacheAnnotationAdminPort, 9100, 9999},
		{"invalid-port", 9100, 9100},
		{"missing-port", 18512, 18512},
	}

	for _, tt := range tests {
		result := GetPortAnnotationOrDefault(annotations, tt.key, tt.defaultValue)
		if result != tt.expected {
			t.Errorf("GetPortAnnotationOrDefault(%q) = %d; want %d", tt.key, result, tt.expected)
		}
	}
}

func TestGetPositiveIntAnnotation(t *testing.T) {
	annotations := map[string]string{
		KVCacheAnnotationBlockSize:        "8192",
		KVCacheAnnotationBlockCount:       "2097152",
		KVCacheAnnotationTotalSlots:       "8192",
		KVCacheAnnotationVirtualNodeCount: "200",
		"invalid-int":                     "notanint",
		"zero-value":                      "0",
		"negative":                        "-42",
	}

	tests := []struct {
		key          string
		defaultValue int
		expected     int
	}{
		{KVCacheAnnotationBlockSize, 4096, 8192},
		{KVCacheAnnotationBlockCount, 1048576, 2097152},
		{KVCacheAnnotationTotalSlots, 4096, 8192},
		{KVCacheAnnotationVirtualNodeCount, 100, 200},
		{"invalid-int", 4096, 4096},
		{"zero-value", 100, 100},
		{"negative", 100, 100},
		{"missing-key", 2048, 2048},
	}

	for _, tt := range tests {
		result := GetPositiveIntAnnotationOrDefault(annotations, tt.key, tt.defaultValue)
		if result != tt.expected {
			t.Errorf("GetPositiveIntAnnotationOrDefault(%q) = %d; want %d", tt.key, result, tt.expected)
		}
	}
}

func TestGetStringAnnotation(t *testing.T) {
	annotations := map[string]string{
		"aibrix.io/container-registry": "ghcr.io/aaahhh",
		"empty-value":                  "",
	}

	tests := []struct {
		key          string
		defaultValue string
		expected     string
	}{
		{"aibrix.io/container-registry", "", "ghcr.io/aaahhh"},
		{"empty-value", "docker.io/aibrix", "docker.io/aibrix"},
		{"missing-key", "default.io", "default.io"},
	}

	for _, tt := range tests {
		result := GetStringAnnotationOrDefault(annotations, tt.key, tt.defaultValue)
		if result != tt.expected {
			t.Errorf("GetStringAnnotationOrDefault(%q) = %q; want %q", tt.key, result, tt.expected)
		}
	}
}
