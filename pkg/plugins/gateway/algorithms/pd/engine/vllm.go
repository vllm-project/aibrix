/*
Copyright 2026 The Aibrix Team.

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

package engine

import (
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/transfer"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// connectorTypeFunc returns the live global KV connector type fallback.
// Defaults to loading AIBRIX_KV_CONNECTOR_TYPE. The routingalgorithms package
// overrides this via SetConnectorTypeFunc so tests that modify the package-level
// var in routingalgorithms are reflected here without a second env-var copy.
var connectorTypeFunc = func() string {
	return utils.LoadEnv("AIBRIX_KV_CONNECTOR_TYPE", transfer.ConnectorTypeSHFS)
}

// SetConnectorTypeFunc installs a function that returns the current global
// KV connector type. Call this once at startup with a closure over the
// routingalgorithms package-level var.
func SetConnectorTypeFunc(fn func() string) {
	connectorTypeFunc = fn
}

func init() {
	Register(&VLLMHandler{})
}

// VLLMHandler implements EngineHandler for vLLM.
// KV-transfer augmentation is fully delegated to the KVTransferAgent resolved
// for the prefill pod, so adding a new connector type (e.g. Mooncake) requires
// no changes here.
type VLLMHandler struct{}

func (h *VLLMHandler) Name() string  { return "vllm" }
func (h *VLLMHandler) IsAsync() bool { return false }

func (h *VLLMHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	completionRequest map[string]any,
) error {
	agent, err := transfer.ResolveAgentForPod(pod, connectorTypeFunc())
	if err != nil {
		return err
	}
	return agent.AugmentPrefillRequest(routingCtx, pod, completionRequest)
}

func (h *VLLMHandler) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	responseData map[string]any,
	pod *v1.Pod,
) error {
	agent, err := transfer.ResolveAgentForPod(pod, connectorTypeFunc())
	if err != nil {
		return err
	}
	return agent.MergePrefillResponse(routingCtx, responseData, pod)
}
