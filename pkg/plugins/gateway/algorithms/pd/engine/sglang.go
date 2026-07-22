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

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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

// AugmentPrefillRequest injects bootstrap_host, bootstrap_port, and a random
// bootstrap_room. It also propagates these fields into routingCtx.ReqBody so
// the decode pod receives the bootstrap address.
//
// This method is retained for EngineHandler compliance and direct callers.
// The normal PreparePayload path uses PreparePrefillPayload instead, which
// avoids re-serializing the entire request and keeps nested field order stable.
func (h *SGLangHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	completionRequest map[string]any,
) error {
	host, port, room := sglangBootstrapFields(pod)
	completionRequest["bootstrap_host"] = host
	completionRequest["bootstrap_port"] = port
	completionRequest["bootstrap_room"] = room

	// Propagate bootstrap fields to the decode request body so the decode pod
	// can locate the prefill pod's bootstrap server.
	reqBody, err := sonic.Marshal(completionRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal SGLang prefill request body: %w", err)
	}
	routingCtx.ReqBody = reqBody
	return nil
}

// sglangBootstrapFields returns the three bootstrap parameters injected into
// both the prefill and decode request bodies. Centralised so that
// AugmentPrefillRequest and PreparePrefillPayload can never diverge.
func sglangBootstrapFields(pod *v1.Pod) (host string, port int64, room int64) {
	return pod.Status.PodIP, sglangBootstrapPortFor(pod), rand.Int63n(1<<63 - 1)
}

// sglangControlledFields lists all top-level keys that the gateway sets or
// deletes on the decode/prefill bodies. A client request must not contain
// duplicates of these keys; if it does, the request is rejected to prevent a
// client-supplied value from surviving and overriding the gateway's value.
var sglangControlledFields = []string{
	"bootstrap_host",
	"bootstrap_port",
	"bootstrap_room",
	"max_tokens",
	"max_completion_tokens",
	"stream",
	"stream_options",
	"min_tokens",
}

// sglangControlledSet is a set for O(1) lookup of controlled field names.
var sglangControlledSet = func() map[string]bool {
	m := make(map[string]bool, len(sglangControlledFields))
	for _, f := range sglangControlledFields {
		m[f] = true
	}
	return m
}()

// ValidateSGLangRequest checks the request body for conditions that would make
// SGLang PD routing unsafe or ambiguous. It returns *InvalidRequestError when
// the request contains duplicate top-level controlled fields or is not a valid
// JSON object, so the gateway layer can map the error to HTTP 400.
//
// This function must be called before both the separate-PD prefill path and
// the combined-pod path to ensure consistent validation regardless of routing.
func ValidateSGLangRequest(body []byte) error {
	if !gjson.ValidBytes(body) {
		return &InvalidRequestError{Message: "SGLang request body is not valid JSON"}
	}
	result := gjson.ParseBytes(body)
	if !result.IsObject() {
		return &InvalidRequestError{Message: "SGLang request body is not a JSON object"}
	}

	// Scan the root object once, counting only controlled fields.
	counts := make(map[string]int, len(sglangControlledFields))
	result.ForEach(func(key, _ gjson.Result) bool {
		if sglangControlledSet[key.String()] {
			counts[key.String()]++
		}
		return true
	})
	for _, field := range sglangControlledFields {
		if counts[field] > 1 {
			return &InvalidRequestError{
				Message: fmt.Sprintf("duplicate top-level key %q in SGLang request body", field),
			}
		}
	}
	return nil
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
// Callers must call ValidateSGLangRequest before this function to ensure no
// duplicate controlled fields are present.
func prepareSGLangRequestBodies(
	originalBody []byte,
	bootstrapHost string,
	bootstrapPort int64,
	bootstrapRoom int64,
) (prefillBody, decodeBody []byte, err error) {
	if !gjson.ValidBytes(originalBody) || !gjson.ParseBytes(originalBody).IsObject() {
		return nil, nil, &InvalidRequestError{Message: "SGLang prefill request body is not a JSON object"}
	}

	// --- Decode body: overwrite bootstrap fields ---
	decodeBody = originalBody
	decodeBody, err = sjson.SetBytes(decodeBody, "bootstrap_host", bootstrapHost)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set bootstrap_host: %w", err)
	}
	decodeBody, err = sjson.SetBytes(decodeBody, "bootstrap_port", bootstrapPort)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set bootstrap_port: %w", err)
	}
	decodeBody, err = sjson.SetBytes(decodeBody, "bootstrap_room", bootstrapRoom)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set bootstrap_room: %w", err)
	}

	// --- Prefill body: derive from decode body, apply prefill constraints ---
	prefillBody = decodeBody
	prefillBody, err = sjson.SetBytes(prefillBody, "max_tokens", 1)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set max_tokens: %w", err)
	}
	prefillBody, err = sjson.SetBytes(prefillBody, "max_completion_tokens", 1)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set max_completion_tokens: %w", err)
	}
	prefillBody, err = sjson.SetBytes(prefillBody, "stream", false)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set stream: %w", err)
	}
	prefillBody, err = sjson.DeleteBytes(prefillBody, "stream_options")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to delete stream_options: %w", err)
	}
	prefillBody, err = sjson.DeleteBytes(prefillBody, "min_tokens")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to delete min_tokens: %w", err)
	}

	return prefillBody, decodeBody, nil
}

// PreparePrefillPayload implements RawPrefillPayloadPreparer. It uses
// sjson/gjson to mutate only top-level fields, preserving the byte ordering of
// all nested objects. On success it updates routingCtx.ReqBody to the decode
// body (so the decode pod receives the bootstrap address) and returns the
// prefill body. On error, routingCtx.ReqBody is left unchanged.
func (h *SGLangHandler) PreparePrefillPayload(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
) ([]byte, error) {
	// Validate before any mutation to prevent duplicate controlled fields
	// from surviving sjson.SetBytes (which only replaces the first occurrence).
	// The normal Route() path also calls ValidateSGLangRequest, but this guard
	// protects direct callers of the RawPrefillPayloadPreparer interface.
	if err := ValidateSGLangRequest(routingCtx.ReqBody); err != nil {
		return nil, err
	}

	host, port, room := sglangBootstrapFields(pod)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(
		routingCtx.ReqBody, host, port, room)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare SGLang request bodies: %w", err)
	}
	routingCtx.ReqBody = decodeBody
	return prefillBody, nil
}

// MergePrefillResponse is a no-op for SGLang: the bootstrap handshake is
// self-coordinating, so no data from the prefill response needs to be injected
// into the decode request.
func (h *SGLangHandler) MergePrefillResponse(
	_ *types.RoutingContext,
	_ map[string]any,
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
