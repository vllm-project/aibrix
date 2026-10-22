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
	"github.com/vllm-project/aibrix/pkg/utils"
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

// AugmentPrefillRequest overwrites bootstrap_host, bootstrap_port, a random
// bootstrap_room and the gateway-owned request id on the client body with
// sjson, so that every nested field (messages, tools, …) is preserved at the
// byte level. Chat/completions and completions carry that id as "rid"; the
// Responses API carries it as "request_id". The resulting decode body is stored
// in routingCtx.ReqBody and returned as the prefill base body; the caller
// applies the common prefill constraints on top of it, so both legs carry the
// same id. On error routingCtx.ReqBody is left unchanged.
//
// ValidateRequest is already called in Route() before this method is invoked,
// so duplicate controlled fields have been rejected by then.
func (h *SGLangHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	body []byte,
) ([]byte, error) {
	host, port, room := sglangBootstrapFields(pod)
	// One rid for both legs: it is what /abort_request matches on when the
	// prefill leg fails and the decode leg has to be cancelled. The Responses
	// API uses request_id (and requires the resp_ prefix for its response id),
	// while chat/completions uses rid.
	var rid string
	var decodeBody []byte
	var err error
	if utils.PathWithoutQuery(routingCtx.ReqPath) == pd.ResponsesPath {
		rid = NewPDResponsesRequestID(routingCtx.RequestID)
		decodeBody, err = sglangResponsesDecodeBody(body, host, port, room, rid)
	} else {
		rid = NewPDRequestID(routingCtx.RequestID)
		decodeBody, err = sglangDecodeBody(body, host, port, room, rid)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to prepare SGLang request bodies: %w", err)
	}
	routingCtx.ReqBody = decodeBody
	// Recorded only once the bodies were produced successfully, so the gateway
	// never holds a rid that no engine leg has seen.
	routingCtx.SetPDRequestID(rid)
	return decodeBody, nil
}

// sglangBootstrapFields returns the three bootstrap parameters injected into
// both the prefill and decode request bodies.
func sglangBootstrapFields(pod *v1.Pod) (host string, port int64, room int64) {
	return pod.Status.PodIP, sglangBootstrapPortFor(pod), rand.Int63n(1<<63 - 1)
}

// sglangBootstrapFieldNames are the top-level keys AugmentPrefillRequest
// writes on both the prefill and decode bodies. The gateway owns the
// engine-visible request id of a PD request (see NewPDRequestID and
// NewPDResponsesRequestID) and overwrites whatever the client sent, so a body
// that repeats either id key is ambiguous and must be rejected. Chat and
// completions use "rid"; the Responses API uses "request_id".
var sglangBootstrapFieldNames = []string{"bootstrap_host", "bootstrap_port", "bootstrap_room", "rid", "request_id"}

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

// sglangDecodeBody returns the client body with bootstrap_host/port/room and
// the gateway-owned rid overwritten. Only these four top-level keys change; all
// other bytes of originalBody are preserved.
//
// A client-supplied rid is deliberately overwritten: the gateway owns the
// abort lifecycle of a PD request, and a rid it does not control can be
// duplicated across attempts (poisoning the engine's in-flight request state)
// or be a prefix of another live rid, turning one abort into a fan-out.
func sglangDecodeBody(originalBody []byte, bootstrapHost string, bootstrapPort, bootstrapRoom int64, rid string) ([]byte, error) {
	if err := validateSGLangBody(originalBody); err != nil {
		return nil, err
	}
	if rid == "" {
		return nil, fmt.Errorf("empty rid for SGLang request bodies")
	}
	return pd.NewJSONEditor(originalBody).
		Set("bootstrap_host", bootstrapHost).
		Set("bootstrap_port", bootstrapPort).
		Set("bootstrap_room", bootstrapRoom).
		Set("rid", rid).
		Result()
}

// sglangResponsesDecodeBody is the /v1/responses form of sglangDecodeBody.
// The Responses API names the engine-visible request id "request_id" and uses
// it as the response id, so it must carry the resp_ prefix (see
// NewPDResponsesRequestID). A client-supplied "rid" is removed because SGLang
// ignores it on this endpoint and leaving it would be ambiguous next to the
// gateway-owned request_id.
func sglangResponsesDecodeBody(originalBody []byte, bootstrapHost string, bootstrapPort, bootstrapRoom int64, requestID string) ([]byte, error) {
	if err := validateSGLangBody(originalBody); err != nil {
		return nil, err
	}
	if requestID == "" {
		return nil, fmt.Errorf("empty request_id for SGLang Responses request bodies")
	}
	return pd.NewJSONEditor(originalBody).
		Set("bootstrap_host", bootstrapHost).
		Set("bootstrap_port", bootstrapPort).
		Set("bootstrap_room", bootstrapRoom).
		Set("request_id", requestID).
		Delete("rid").
		Result()
}

// validateSGLangBody is the defense-in-depth guard shared by both body
// builders. Callers (Route → PreparePayload) are required to validate the body
// first, but this prevents sjson from silently producing invalid output if a
// future caller bypasses validation.
func validateSGLangBody(body []byte) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return &InvalidRequestError{Message: "SGLang prefill request body is not a JSON object"}
	}
	return nil
}

// prepareSGLangRequestBodies derives the decode and prefill request bodies from
// the original request bytes, mutating only top-level fields with sjson. All
// nested fields (messages, tools, …) are preserved at the byte level so that
// prompt_token_ids remain stable across prefill, decode, and repeated requests.
//
// The decode body is the client body with bootstrap_host/port/room and rid
// overwritten. The prefill body is derived from the decode body with
// max_tokens=1, max_completion_tokens=1, stream=false, stream_options and
// min_tokens removed. The caller supplies a single bootstrapRoom and a single
// rid that are shared between both bodies.
//
// This mirrors what PreparePayload produces through AugmentPrefillRequest and
// is kept as a single-call helper for tests and direct callers.
func prepareSGLangRequestBodies(
	originalBody []byte,
	bootstrapHost string,
	bootstrapPort int64,
	bootstrapRoom int64,
	rid string,
) (prefillBody, decodeBody []byte, err error) {
	decodeBody, err = sglangDecodeBody(originalBody, bootstrapHost, bootstrapPort, bootstrapRoom, rid)
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
