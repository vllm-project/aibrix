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

// KV Event Sync specific constants introduced in the feature/kv-event-pub-sub branch

// Label keys for KV Event Sync
const (
	// KVEventsEnabledLabel indicates if KV events are enabled for this pod
	// This label was introduced specifically for KV Event Sync feature
	// Example: "model.aibrix.ai/kv-events-enabled": "true"
	KVEventsEnabledLabel = "model.aibrix.ai/kv-events-enabled"

	// LoraIDLabel specifies the LoRA adapter ID for KV sync
	// This label is used by KV Event Sync to track LoRA-specific caches
	// Example: "model.aibrix.ai/lora-id": "123"
	LoraIDLabel = "model.aibrix.ai/lora-id"
)

// Environment variable names for KV Event Sync
const (
	// EnvPrefixCacheKVEventSyncEnabled enables KV event synchronization
	// When true, enables ZMQ-based cache event synchronization
	EnvPrefixCacheKVEventSyncEnabled = "AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED"

	// EnvPrefixCacheKVEventPublishAddr specifies ZMQ publish address
	// Format: "tcp://*:5555" or similar ZMQ address
	EnvPrefixCacheKVEventPublishAddr = "AIBRIX_PREFIX_CACHE_KV_EVENT_PUBLISH_ADDR"

	// EnvPrefixCacheKVEventSubscribeAddrs specifies ZMQ subscribe addresses
	// Comma-separated list of ZMQ addresses to subscribe to
	EnvPrefixCacheKVEventSubscribeAddrs = "AIBRIX_PREFIX_CACHE_KV_EVENT_SUBSCRIBE_ADDRS"

	// EnvPrefixCacheLocalRouterMetricsEnabled enables prefix cache metrics
	// Added as part of KV Event Sync to control metrics registration
	EnvPrefixCacheLocalRouterMetricsEnabled = "AIBRIX_PREFIX_CACHE_LOCAL_ROUTER_METRICS_ENABLED"

	// EnvPrefixCacheUseRemoteTokenizer enables remote tokenizer usage
	// When true, uses remote tokenizer service instead of local tokenization
	EnvPrefixCacheUseRemoteTokenizer = "AIBRIX_PREFIX_CACHE_USE_REMOTE_TOKENIZER"

	// EnvPrefixCacheTokenizerType specifies the tokenizer type for prefix cache
	// Options: "character", "tiktoken", "remote"
	EnvPrefixCacheTokenizerType = "AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE"

	// EnvPrefixCacheRemoteTokenizerEndpoint specifies the remote tokenizer service endpoint
	// Format: "http://service:port" - required when using remote tokenizer
	EnvPrefixCacheRemoteTokenizerEndpoint = "AIBRIX_PREFIX_CACHE_REMOTE_TOKENIZER_ENDPOINT"
)

// Helper functions for KV Event Sync labels

// IsKVEventsEnabled checks if KV events are enabled for the pod
func IsKVEventsEnabled(labels map[string]string) bool {
	return labels[KVEventsEnabledLabel] == "true"
}

// GetLoraID retrieves the LoRA adapter ID from pod labels
func GetLoraID(labels map[string]string) string {
	if id, ok := labels[LoraIDLabel]; ok {
		return id
	}
	return ""
}
