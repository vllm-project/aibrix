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

package prefill

import (
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// PreparePayload transforms routingCtx.ReqBody into a prefill-specific payload
// ready to be POSTed to the prefill pod. It delegates all engine-specific field
// injection to handler.AugmentPrefillRequest, then applies common constraints:
// max_tokens=1, stream=false, no stream_options, no min_tokens. TRT-LLM does not
// accept max_completion_tokens, so that field is omitted for that engine.
//
// This function is exported so pdRouter can expose a thin backward-compat wrapper
// for tests that call preparePrefillPayload directly.
func PreparePayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string, handler engine.EngineHandler) ([]byte, error) {
	var completionRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &completionRequest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefill request body: %w", err)
	}

	if err := handler.AugmentPrefillRequest(routingCtx, pod, completionRequest); err != nil {
		return nil, fmt.Errorf("failed to augment prefill request for %s: %w", llmEngine, err)
	}

	// Constrain the prefill to a single token so the pod returns immediately
	// after completing the KV-cache fill without generating any real output.
	completionRequest["max_tokens"] = 1
	if llmEngine == trtllmEngine {
		// TRT-LLM uses max_tokens only; max_completion_tokens is not supported.
		delete(completionRequest, "max_completion_tokens")
	} else {
		completionRequest["max_completion_tokens"] = 1
	}
	completionRequest["stream"] = false
	delete(completionRequest, "stream_options")
	delete(completionRequest, "min_tokens")

	return sonic.Marshal(completionRequest)
}
