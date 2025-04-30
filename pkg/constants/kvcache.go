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

	// Deprecated: use kvcache backend directly.
	// distributed, centralized.
	KVCacheAnnotationMode              = "kvcache.orchestration.aibrix.ai/mode"
	KVCacheAnnotationContainerRegistry = "kvcache.orchestration.aibrix.ai/container-registry"

	KVCacheLabelValueRoleCache     = "cache"
	KVCacheLabelValueRoleMetadata  = "metadata"
	KVCacheLabelValueRoleKVWatcher = "kvwatcher"
)
