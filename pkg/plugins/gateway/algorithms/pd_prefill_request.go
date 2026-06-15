/*
Copyright 2024 The Aibrix Team.

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

package routingalgorithms

import (
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/transfer"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// doPrefillRequest delegates to the router's PrefillExecutor.
func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string) error {
	logCtx := prefillLogContext(r, routingCtx)
	return r.prefillExecutor.Execute(routingCtx, prefillPod, llmEngine, logCtx)
}

// prefillLogContext resolves effective score-policy names for structured logging.
// Errors from effectiveScorePolicies are non-fatal here because policies were
// already applied during pod selection.
func prefillLogContext(r *pdRouter, routingCtx *types.RoutingContext) prefill.LogContext {
	prefillPol, decodePol, polErr := r.effectiveScorePolicies(routingCtx)

	prefillScorePolicyName := pd.PrefillScorePolicyPrefixCache
	if r.prefillPolicy != nil {
		prefillScorePolicyName = r.prefillPolicy.Name()
	}
	if polErr == nil && prefillPol != nil {
		prefillScorePolicyName = prefillPol.Name()
	}

	decodeScorePolicyName := string(pd.DecodePolicyLoadBalancing)
	if r.decodePolicy != nil {
		decodeScorePolicyName = string(r.decodePolicy.Name())
	}
	if polErr == nil && decodePol != nil {
		decodeScorePolicyName = string(decodePol.Name())
	}

	return prefill.LogContext{
		PrefillScorePolicy: prefillScorePolicyName,
		DecodeScorePolicy:  decodeScorePolicyName,
	}
}

// preparePrefillPayload is kept for test backward compatibility.
// Delegates to prefill.PreparePayload which owns the stateless payload logic.
func (r *pdRouter) preparePrefillPayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string, handler engine.EngineHandler) ([]byte, error) {
	return prefill.PreparePayload(routingCtx, pod, llmEngine, handler)
}

// updateRoutingContextWithKVTransferParams delegates to the vLLM engine handler's
// MergePrefillResponse so existing tests can call this method directly.
func (r *pdRouter) updateRoutingContextWithKVTransferParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	return engine.Resolve(VLLMEngine).MergePrefillResponse(routingCtx, responseData, prefillPod)
}

// updateRoutingContextWithTRTDisaggParams delegates to the TRT-LLM engine handler's
// MergePrefillResponse so existing tests can call this method directly.
func (r *pdRouter) updateRoutingContextWithTRTDisaggParams(routingCtx *types.RoutingContext, responseData map[string]any, prefillPod *v1.Pod) error {
	return engine.Resolve(TensorRTLLM).MergePrefillResponse(routingCtx, responseData, prefillPod)
}

// selectKvConnectorType resolves the KV connector type from a pod label value,
// falling back to aibrixKVConnectorType when the value is absent or unrecognized.
func selectKvConnectorType(value string) string {
	return transfer.ResolveConnectorType(value, aibrixKVConnectorType)
}
