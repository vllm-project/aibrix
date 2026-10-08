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

// Label keys used by the Aibrix system.
// The format `resource.aibrix.ai/attribute` is the standard.

const (
	// ModelLabelName is the label for identifying the model name
	// Example: "model.aibrix.ai/name": "deepseek-llm-7b-chat"
	ModelLabelName = "model.aibrix.ai/name"

	// ModelLabelEngine is the label for identifying the inference engine
	// Example: "model.aibrix.ai/engine": "vllm"
	ModelLabelEngine = "model.aibrix.ai/engine"

	// ModelLabelMetricPort is the label for specifying the metrics port
	// Example: "model.aibrix.ai/metric-port": "8000"
	ModelLabelMetricPort = "model.aibrix.ai/metric-port"

	// ModelLabelPort is the label for specifying the service port
	// Example: "model.aibrix.ai/port": "8080"
	ModelLabelPort = "model.aibrix.ai/port"

	// ModelLabelAdapterEnabled is the label for enabling or disabling adapter dynamic registration
	// Example: "adapter.model.aibrix.ai/enabled": "true"
	ModelLabelAdapterEnabled = "adapter.model.aibrix.ai/enabled"

	// ModelPoolLabelName identifies the warm GPU pool a pod belongs to. A warm GPU
	// pool is an ordinary Deployment of pre-warmed GPU-host pods; ModelClaims
	// are bin-packed onto pods sharing the same pool name.
	// Example: "claim.model.aibrix.ai/pool": "b300-pool-a"
	ModelPoolLabelName = "claim.model.aibrix.ai/pool"

	// ModelPoolLabelEnabled marks a pod as a warm GPU pool member that accepts dynamic
	// ModelClaim attachments (the GPU/CUDA context and the kvcached KV pool are
	// reserved and the runtime sidecar is ready). Analogous to ModelLabelAdapterEnabled.
	// Example: "claim.model.aibrix.ai/enabled": "true"
	ModelPoolLabelEnabled = "claim.model.aibrix.ai/enabled"

	// ModelPoolLabelEnabledValue is the enabled value for ModelPoolLabelEnabled.
	ModelPoolLabelEnabledValue = "true"

	// ModelPoolPolicyAnnotationKey holds one JSON pool policy on the warm
	// Deployment metadata. It intentionally avoids a separate policy CRD while
	// keeping the configuration scoped to the pool that owns the GPU pods.
	// Example: "claim.model.aibrix.ai/pool-policy": '{"reclaim":{"mode":"kv-first","capacityBytes":17179869184}}'
	ModelPoolPolicyAnnotationKey = "claim.model.aibrix.ai/pool-policy"

	// ModelClaimPodAnnotationPrefix marks, on a warm GPU pod, that a ModelClaim
	// has been activated on it. The key is suffixed with the ModelClaim object
	// name (a DNS name, so always annotation-key-safe) and the value is a JSON
	// object {"model":"<servedModelName>","port":<port>,"state":"<state>"}.
	// A controller that wakes sleeping engines itself also writes
	// "wakeByRequest":true there.
	// One key per ModelClaim avoids multi-writer races on a shared annotation.
	// The gateway cache reads these to make active models routable and retain
	// sleeping port-0 bindings for request-triggered wake.
	// Example: "route.claim.model.aibrix.ai/qwen2-7b":
	// '{"model":"qwen2-7b-instruct","port":9001,"state":"active"}'
	ModelClaimPodAnnotationPrefix = "route.claim.model.aibrix.ai/"

	// ModelClaimWakeAnnotationPrefix marks, on the warm pod that holds a
	// sleeping engine, that a request has asked for that engine's model. The
	// key is suffixed with the ModelClaim object name, and the value is the
	// time of the request, in RFC 3339. The gateway writes it, and the
	// controller wakes the engine and removes it. Its prefix is not
	// ModelClaimPodAnnotationPrefix, so it is never read as a binding.
	// Example: "wake.modelclaim.aibrix.ai/qwen2-7b": "2026-10-01T08:00:00Z"
	ModelClaimWakeAnnotationPrefix = "wake.modelclaim.aibrix.ai/"

	// ModelClaim routing states are observed runtime states carried alongside
	// the per-model port. They let the gateway distinguish a sleeping engine
	// that can be woken from an engine that is merely starting or has failed.
	ModelClaimRoutingStateActive     = "active"
	ModelClaimRoutingStateActivating = "activating"
	ModelClaimRoutingStateSleeping   = "sleeping"
	ModelClaimRoutingStateFailed     = "failed"

	// ModelClaimReasonTooLargeForAnyCard is the reason of a claim's Scheduled
	// condition when no card in its pool could hold it, even empty. Waiting
	// does not help, so the gateway does not ask its clients to retry.
	ModelClaimReasonTooLargeForAnyCard = "TooLargeForAnyCard"

	// ModelClaim route reasons say more than a routing state about why a model
	// is not served. The controller writes them on the route and on the claim's
	// Ready condition, and the gateway tells its clients how long to wait by
	// them. A request waits for room to wake the engine, or the claim moves to
	// another pod.
	ModelClaimRouteReasonWaitingForRoom = "WaitingForRoom"
	ModelClaimRouteReasonMoving         = "Moving"

	// ModelClaimReasonMakingRoom is the reason of a claim's Scheduled condition
	// while idle engines are put to sleep to make room for it. The gateway
	// tells a client how long to wait by it as well.
	ModelClaimReasonMakingRoom = "MakingRoom"
)

const (
	// ModelAnnoServiceName identifies the Kubernetes Service backing a model when
	// its externally served name cannot also be used as a Kubernetes object name.
	ModelAnnoServiceName = "model.aibrix.ai/service-name"

	// ModelAnnoRouterCustomPath is the anno for add PathPrefixes in httpRoute, split by comma
	// Example: "model.aibrix.ai/model-router-custom-paths": "/score,/version"
	ModelAnnoRouterCustomPath = "model.aibrix.ai/model-router-custom-paths"

	// ModelAnnoConfig is the annotation holding JSON model config with multiple profiles.
	// Client selects profile at runtime via config-profile header or defaultProfile is selected.
	// See docs/source/designs/model-config-profiles.rst for schema.
	ModelAnnoConfig = "model.aibrix.ai/config"
)

// ModelNameFromMetadata returns the served model name from Kubernetes metadata.
// Labels remain the preferred source, while annotations support names containing
// characters that Kubernetes label values reject, such as '/'.
func ModelNameFromMetadata(labels, annotations map[string]string) (string, bool) {
	if modelName := labels[ModelLabelName]; modelName != "" {
		return modelName, true
	}
	if modelName := annotations[ModelLabelName]; modelName != "" {
		return modelName, true
	}
	return "", false
}
