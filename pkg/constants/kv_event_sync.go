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
	// EnvKVEventSyncEnabled enables KV event synchronization
	// When true, enables ZMQ-based cache event synchronization
	EnvKVEventSyncEnabled = "AIBRIX_KV_EVENT_SYNC_ENABLED"

	// EnvKVEventPublishAddr specifies ZMQ publish address
	// Format: "tcp://*:5555" or similar ZMQ address
	EnvKVEventPublishAddr = "AIBRIX_KV_EVENT_PUBLISH_ADDR"

	// EnvKVEventSubscribeAddrs specifies ZMQ subscribe addresses
	// Comma-separated list of ZMQ addresses to subscribe to
	EnvKVEventSubscribeAddrs = "AIBRIX_KV_EVENT_SUBSCRIBE_ADDRS"

	// EnvPrefixCacheMetricsEnabled enables prefix cache metrics
	// Added as part of KV Event Sync to control metrics registration
	EnvPrefixCacheMetricsEnabled = "AIBRIX_PREFIX_CACHE_METRICS_ENABLED"
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
