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

package constants

const (
	KVCacheLabelKeyIdentifier    = "kvcache.orchestration.aibrix.ai/name"
	KVCacheLabelKeyRole          = "kvcache.orchestration.aibrix.ai/role"
	KVCacheLabelKeyMetadataIndex = "kvcache.orchestration.aibrix.ai/etcd-index"
	KVCacheLabelKeyBackend       = "kvcache.orchestration.aibrix.ai/backend"

	KVCacheAnnotationNodeAffinityKey     = "kvcache.orchestration.aibrix.ai/node-affinity-key"
	KVCacheAnnotationNodeAffinityGPUType = "kvcache.orchestration.aibrix.ai/node-affinity-gpu-type"
	KVCacheAnnotationPodAffinityKey      = "kvcache.orchestration.aibrix.ai/pod-affinity-workload"
	KVCacheAnnotationPodAntiAffinity     = "kvcache.orchestration.aibrix.ai/pod-anti-affinity"

	KVCacheAnnotationNodeAffinityDefaultKey = "machine.cluster.vke.volcengine.com/gpu-name"

	// This config will be deprecated in future, users should specify kvcache backend directly.
	KVCacheAnnotationMode = "kvcache.orchestration.aibrix.ai/mode"

	KVCacheLabelValueRoleCache     = "cache"
	KVCacheLabelValueRoleMetadata  = "metadata"
	KVCacheLabelValueRoleKVWatcher = "kvwatcher"

	KVCacheBackendVineyard    = "vineyard"
	KVCacheBackendHPKV        = "hpkv"
	KVCacheBackendInfinistore = "infinistore"
	KVCacheBackendDefault     = KVCacheBackendVineyard
)
