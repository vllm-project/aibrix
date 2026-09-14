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
	"fmt"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// EngineHandler encapsulates engine-specific behaviour for PD-disaggregated
// prefill: how to derive the outbound prefill request body and how to merge
// the prefill response back into the decode request body.
//
// Both methods operate on raw JSON bytes and must only add, replace or delete
// top-level keys (or keys inside the small gateway-owned objects such as
// kv_transfer_params / disaggregated_params). They must never round-trip the
// body through map[string]any: that re-serialises nested objects (messages,
// tools, …) in random key order, which changes prompt_token_ids between the
// prefill and decode requests and across repeated requests, defeating
// KV-cache prefix reuse.
type EngineHandler interface {
	// Name returns the engine identifier (e.g. "vllm", "sglang", "trtllm").
	Name() string
	// IsAsync returns true when the engine's prefill handshake is
	// self-coordinating and should be fired in a goroutine (e.g. SGLang).
	IsAsync() bool
	// AugmentPrefillRequest returns the prefill request body derived from
	// body, which is the client request already validated as a JSON object.
	// The common prefill constraints (max_tokens=1, stream=false, …) are
	// applied by the caller afterwards. Handlers that must also change the
	// decode body (e.g. SGLang bootstrap fields) update routingCtx.ReqBody;
	// on error routingCtx.ReqBody must be left unchanged.
	AugmentPrefillRequest(routingCtx *types.RoutingContext, pod *v1.Pod, body []byte) ([]byte, error)
	// MergePrefillResponse injects engine-specific data from the raw prefill
	// response into routingCtx.ReqBody before the decode pod receives it.
	MergePrefillResponse(routingCtx *types.RoutingContext, prefillResponse []byte, pod *v1.Pod) error
	// ControlledFields returns the top-level keys, in addition to
	// pd.CommonControlledFields, that AugmentPrefillRequest or
	// MergePrefillResponse may write. ValidateRequest rejects client bodies
	// that contain any of them more than once.
	ControlledFields() []string
}

// ValidateRequest checks that body is a JSON object that does not repeat any
// gateway-controlled top-level key (pd.CommonControlledFields plus
// h.ControlledFields()). It returns *InvalidRequestError so the gateway can
// map the failure to HTTP 400. Route() calls it for every engine before pod
// selection, so a malformed request can neither pollute selection state nor
// reach a prefill pod.
func ValidateRequest(body []byte, h EngineHandler) error {
	return validateRequestBody(body, h.ControlledFields(), "request body")
}

func validateRequestBody(body []byte, extraControlledFields []string, what string) error {
	if err := pd.ValidateJSONObject(body, what); err != nil {
		return &InvalidRequestError{Message: err.Error()}
	}
	controlled := append(append([]string(nil), pd.CommonControlledFields...), extraControlledFields...)
	if key, dup := pd.FindDuplicateTopLevelKey(body, controlled); dup {
		return &InvalidRequestError{Message: fmt.Sprintf("duplicate top-level key %q in %s", key, what)}
	}
	return nil
}

// InvalidRequestError represents a client-side error (malformed request body)
// that should result in an HTTP 400 response rather than a 5xx. Gateway layers
// should use errors.As to detect this type and map it to BadRequest.
type InvalidRequestError struct {
	Message string
}

func (e *InvalidRequestError) Error() string { return e.Message }
