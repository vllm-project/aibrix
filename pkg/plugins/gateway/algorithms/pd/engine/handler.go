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
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// EngineHandler encapsulates engine-specific behaviour for PD-disaggregated
// prefill: how to augment the outbound prefill request body and how to merge
// the prefill response back into the decode request body.
type EngineHandler interface {
	// Name returns the engine identifier (e.g. "vllm", "sglang", "trtllm").
	Name() string
	// IsAsync returns true when the engine's prefill handshake is
	// self-coordinating and should be fired in a goroutine (e.g. SGLang).
	IsAsync() bool
	// AugmentPrefillRequest adds engine-specific fields to completionRequest
	// before it is POSTed to the prefill pod.
	AugmentPrefillRequest(routingCtx *types.RoutingContext, pod *v1.Pod, completionRequest map[string]any) error
	// MergePrefillResponse injects engine-specific data from the prefill
	// response into routingCtx.ReqBody before the decode pod receives it.
	MergePrefillResponse(routingCtx *types.RoutingContext, responseData map[string]any, pod *v1.Pod) error
}

// RawPrefillPayloadPreparer is an optional interface implemented by engines
// that need to preserve the exact byte ordering of the original request body
// (e.g. SGLang, where non-deterministic map key ordering would change
// prompt_token_ids and degrade KV cache hit rates). When the handler
// implements this interface, prefill.PreparePayload uses it instead of the
// unmarshal → mutate → marshal path, which avoids re-serializing nested
// fields such as tool schemas.
type RawPrefillPayloadPreparer interface {
	// PreparePrefillPayload returns the prefill request body and side-effects
	// routingCtx.ReqBody with the decode request body. Both bodies must
	// preserve all untouched fields from the original request at the byte
	// level. On error, routingCtx.ReqBody must not be modified.
	PreparePrefillPayload(routingCtx *types.RoutingContext, pod *v1.Pod) (prefillBody []byte, err error)
}

// InvalidRequestError represents a client-side error (malformed request body)
// that should result in an HTTP 400 response rather than a 5xx. Gateway layers
// should use errors.As to detect this type and map it to BadRequest.
type InvalidRequestError struct {
	Message string
}

func (e *InvalidRequestError) Error() string { return e.Message }
