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
	"math/rand"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

const (
	sglangBootstrapPort           int64  = 8998
	sglangBootstrapPortIdentifier string = "model.aibrix.ai/sglang-bootstrap-port"
)

func init() {
	Register(&SGLangHandler{})
}

// SGLangHandler implements EngineHandler for SGLang.
// SGLang uses a bootstrap handshake (bootstrap_host/port/room) to coordinate
// KV transfer between prefill and decode pods out-of-band, so the prefill
// fires asynchronously (IsAsync returns true).
type SGLangHandler struct{}

func (h *SGLangHandler) Name() string  { return "sglang" }
func (h *SGLangHandler) IsAsync() bool { return true }

// AugmentPrefillRequest overwrites bootstrap_host, bootstrap_port and a random
// bootstrap_room on the client body with sjson, so that every nested field
// (messages, tools, …) is preserved at the byte level. The resulting decode
// body is stored in routingCtx.ReqBody and returned as the prefill base body;
// the caller applies the common prefill constraints on top of it. On error
// routingCtx.ReqBody is left unchanged.
//
// ValidateRequest is already called in Route() before this method is invoked,
// so duplicate controlled fields have been rejected by then.
func (h *SGLangHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	body []byte,
) ([]byte, error) {
	host, port, room := sglangBootstrapFields(pod)
	decodeBody, err := sglangDecodeBody(body, host, port, room)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare SGLang request bodies: %w", err)
	}
	routingCtx.ReqBody = decodeBody
	return decodeBody, nil
}

// sglangBootstrapFields returns the three bootstrap parameters injected into
// both the prefill and decode request bodies.
func sglangBootstrapFields(pod *v1.Pod) (host string, port int64, room int64) {
	return pod.Status.PodIP, sglangBootstrapPortFor(pod), rand.Int63n(1<<63 - 1)
}

// sglangBootstrapFieldNames are the top-level keys AugmentPrefillRequest
// writes on both the prefill and decode bodies.
var sglangBootstrapFieldNames = []string{"bootstrap_host", "bootstrap_port", "bootstrap_room"}

// ControlledFields returns the SGLang bootstrap keys; the common prefill
// control keys are added by ValidateRequest.
func (h *SGLangHandler) ControlledFields() []string { return sglangBootstrapFieldNames }

// ValidateSGLangRequest checks the request body for conditions that would make
// SGLang PD routing unsafe or ambiguous. It returns *InvalidRequestError when
// the request contains duplicate top-level controlled fields or is not a valid
// JSON object, so the gateway layer can map the error to HTTP 400.
//
// It is the SGLang-specific form of ValidateRequest and is kept for callers
// that validate outside Route().
func ValidateSGLangRequest(body []byte) error {
	return validateRequestBody(body, sglangBootstrapFieldNames, "SGLang request body")
}

// sglangDecodeBody returns the client body with bootstrap_host/port/room
// overwritten. Only these three top-level keys change; all other bytes of
// originalBody are preserved.
func sglangDecodeBody(originalBody []byte, bootstrapHost string, bootstrapPort, bootstrapRoom int64) ([]byte, error) {
	// Defense-in-depth: callers (Route → PreparePayload) are required to
	// validate the body first, but this guard prevents sjson from silently
	// producing invalid output if a future caller bypasses validation.
	if !gjson.ValidBytes(originalBody) || !gjson.ParseBytes(originalBody).IsObject() {
		return nil, &InvalidRequestError{Message: "SGLang prefill request body is not a JSON object"}
	}
	return pd.NewJSONEditor(originalBody).
		Set("bootstrap_host", bootstrapHost).
		Set("bootstrap_port", bootstrapPort).
		Set("bootstrap_room", bootstrapRoom).
		Result()
}

// prepareSGLangRequestBodies derives the decode and prefill request bodies from
// the original request bytes, mutating only top-level fields with sjson. All
// nested fields (messages, tools, …) are preserved at the byte level so that
// prompt_token_ids remain stable across prefill, decode, and repeated requests.
//
// The decode body is the client body with bootstrap_host/port/room overwritten.
// The prefill body is derived from the decode body with max_tokens=1,
// max_completion_tokens=1, stream=false, stream_options and min_tokens removed.
// The caller supplies a single bootstrapRoom that is shared between both bodies.
//
// This mirrors what PreparePayload produces through AugmentPrefillRequest and
// is kept as a single-call helper for tests and direct callers.
func prepareSGLangRequestBodies(
	originalBody []byte,
	bootstrapHost string,
	bootstrapPort int64,
	bootstrapRoom int64,
) (prefillBody, decodeBody []byte, err error) {
	decodeBody, err = sglangDecodeBody(originalBody, bootstrapHost, bootstrapPort, bootstrapRoom)
	if err != nil {
		return nil, nil, err
	}
	prefillBody, err = pd.ApplyPrefillControlFields(decodeBody, "sglang")
	if err != nil {
		return nil, nil, err
	}
	return prefillBody, decodeBody, nil
}

// MergePrefillResponse is a no-op for SGLang: the bootstrap handshake is
// self-coordinating, so no data from the prefill response needs to be injected
// into the decode request.
func (h *SGLangHandler) MergePrefillResponse(
	_ *types.RoutingContext,
	_ []byte,
	_ *v1.Pod,
) error {
	return nil
}

func sglangBootstrapPortFor(pod *v1.Pod) int64 {
	if portStr, exists := pod.Annotations[sglangBootstrapPortIdentifier]; exists {
		if port, err := strconv.ParseInt(portStr, 10, 32); err == nil {
			return port
		}
	}
	return sglangBootstrapPort
}
