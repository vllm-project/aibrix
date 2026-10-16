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

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// PreparePayload transforms routingCtx.ReqBody into a prefill-specific payload
// ready to be POSTed to the prefill pod.
//
// The client body is validated as a JSON object, passed to
// handler.AugmentPrefillRequest for engine-specific field injection, and the
// common prefill constraints (max_tokens=1, max_completion_tokens=1,
// stream=false, no stream_options, no min_tokens) are applied on top with
// top-level sjson edits. Nested fields (messages, tools, …) are never
// re-serialised, so their byte ordering is identical in the original, prefill
// and decode bodies and prompt_token_ids stay stable across requests.
//
// routingCtx.ReqBody is only changed by handlers that need a modified decode
// body (SGLang bootstrap fields); for every other engine the decode pod
// receives the client body untouched.
//
// This function is exported so pdRouter can expose a thin backward-compat wrapper
// for tests that call preparePrefillPayload directly.
func PreparePayload(routingCtx *types.RoutingContext, pod *v1.Pod, llmEngine string, handler engine.EngineHandler) ([]byte, error) {
	if err := pd.ValidateJSONObject(routingCtx.ReqBody, "prefill request body"); err != nil {
		return nil, &engine.InvalidRequestError{Message: err.Error()}
	}

	payload, err := handler.AugmentPrefillRequest(routingCtx, pod, routingCtx.ReqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to augment prefill request for %s: %w", llmEngine, err)
	}

	// Constrain the prefill to a single token so the pod returns immediately
	// after completing the KV-cache fill without generating any real output.
	payload, err = pd.ApplyPrefillControlFields(payload, llmEngine)
	if err != nil {
		return nil, fmt.Errorf("failed to apply prefill control fields for %s: %w", llmEngine, err)
	}
	return payload, nil
}
