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

package prefill

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// fakeHandler is a test double implementing EngineHandler. It records the
// body it was given, optionally injects a top-level field, and can fail.
type fakeHandler struct {
	augmentCalled bool
	gotBody       []byte
	failAugment   bool
}

func (f *fakeHandler) Name() string               { return "fake" }
func (f *fakeHandler) IsAsync() bool              { return false }
func (f *fakeHandler) ControlledFields() []string { return nil }

func (f *fakeHandler) AugmentPrefillRequest(
	_ *types.RoutingContext, _ *v1.Pod, body []byte,
) ([]byte, error) {
	f.augmentCalled = true
	f.gotBody = body
	if f.failAugment {
		return nil, fmt.Errorf("sentinel error from fakeHandler")
	}
	return []byte(`{"injected":"by-handler","messages":[{"role":"user","content":"x"}],"stream":true,"stream_options":{"include_usage":true},"min_tokens":3}`), nil
}

func (f *fakeHandler) MergePrefillResponse(
	_ *types.RoutingContext, _ []byte, _ *v1.Pod,
) error {
	return nil
}

func TestPreparePayload_AppliesControlFieldsOnHandlerOutput(t *testing.T) {
	original := []byte(`{"messages":[],"max_tokens":256,"stream":true}`)
	routingCtx := &types.RoutingContext{
		ReqBody: original,
		Context: context.Background(),
	}
	handler := &fakeHandler{}

	payload, err := PreparePayload(routingCtx, &v1.Pod{}, "fake", handler)
	require.NoError(t, err)

	assert.True(t, handler.augmentCalled, "AugmentPrefillRequest must be called")
	assert.Equal(t, original, handler.gotBody, "handler must receive the client body")

	// Common prefill constraints are applied to what the handler returned.
	assert.Equal(t, "by-handler", gjson.GetBytes(payload, "injected").String())
	assert.Equal(t, int64(1), gjson.GetBytes(payload, "max_tokens").Int())
	assert.Equal(t, int64(1), gjson.GetBytes(payload, "max_completion_tokens").Int())
	assert.False(t, gjson.GetBytes(payload, "stream").Bool())
	assert.False(t, gjson.GetBytes(payload, "stream_options").Exists())
	assert.False(t, gjson.GetBytes(payload, "min_tokens").Exists())

	// The decode body is untouched for handlers that do not modify it.
	assert.Equal(t, original, routingCtx.ReqBody)
}

func TestPreparePayload_TRTLLMDropsMaxCompletionTokens(t *testing.T) {
	routingCtx := &types.RoutingContext{
		ReqBody: []byte(`{"messages":[],"max_completion_tokens":64}`),
		Context: context.Background(),
	}

	payload, err := PreparePayload(routingCtx, &v1.Pod{}, "trtllm", engine.Resolve("trtllm"))
	require.NoError(t, err)

	assert.Equal(t, int64(1), gjson.GetBytes(payload, "max_tokens").Int())
	assert.False(t, gjson.GetBytes(payload, "max_completion_tokens").Exists(),
		"TRT-LLM does not accept max_completion_tokens")
	assert.Equal(t, "context_only", gjson.GetBytes(payload, "disaggregated_params.request_type").String())
}

func TestPreparePayload_HandlerErrorPropagates(t *testing.T) {
	routingCtx := &types.RoutingContext{
		ReqBody: []byte(`{"valid":"json"}`),
		Context: context.Background(),
	}
	handler := &fakeHandler{failAugment: true}

	_, err := PreparePayload(routingCtx, &v1.Pod{}, "fake", handler)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sentinel error from fakeHandler")
}

func TestPreparePayload_RejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{`{this is not valid json`, `[1,2,3]`, `"str"`, ``} {
		routingCtx := &types.RoutingContext{
			ReqBody: []byte(body),
			Context: context.Background(),
		}
		handler := &fakeHandler{}

		_, err := PreparePayload(routingCtx, &v1.Pod{}, "fake", handler)
		require.Error(t, err, "body %q", body)

		var invalidReqErr *engine.InvalidRequestError
		assert.True(t, errors.As(err, &invalidReqErr), "body %q must yield *InvalidRequestError", body)
		assert.False(t, handler.augmentCalled, "handler must not run on an invalid body")
	}
}

// TestPreparePayload_SGLangHandlerIntegration exercises the full dispatcher
// path: PreparePayload → SGLangHandler.AugmentPrefillRequest. It verifies that
// the random bootstrap_room is identical in the returned prefill payload and
// the side-effected routingCtx.ReqBody (decode body), and that messages/tools
// are byte-identical across all three bodies (original, prefill, decode).
func TestPreparePayload_SGLangHandlerIntegration(t *testing.T) {
	originalBody := []byte(`{"model":"test-model","messages":[{"role":"user","content":"Call the lookup tool"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup a value","parameters":{"type":"object","properties":{"query":{"type":"string","description":"Lookup query"}},"required":["query"]}}}],"max_tokens":256,"stream":true}`)

	routingCtx := &types.RoutingContext{
		ReqBody: originalBody,
		Context: context.Background(),
	}
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}}
	handler := engine.Resolve("sglang")

	prefillBody, err := PreparePayload(routingCtx, pod, "sglang", handler)
	require.NoError(t, err)

	decodeBody := routingCtx.ReqBody

	// bootstrap_room must be the same in prefill and decode (random but shared).
	// The random range includes 0, so we assert existence and type rather than > 0.
	prefillRoom := gjson.GetBytes(prefillBody, "bootstrap_room")
	decodeRoom := gjson.GetBytes(decodeBody, "bootstrap_room")
	assert.True(t, prefillRoom.Exists(), "prefill bootstrap_room must be set")
	assert.Equal(t, gjson.Number, prefillRoom.Type, "prefill bootstrap_room must be a number")
	assert.True(t, decodeRoom.Exists(), "decode bootstrap_room must be set")
	assert.Equal(t, prefillRoom.Int(), decodeRoom.Int(), "bootstrap_room must match between prefill and decode")

	// bootstrap_host must be the pod IP in both bodies.
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(prefillBody, "bootstrap_host").String())
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(decodeBody, "bootstrap_host").String())

	// messages and tools must be byte-identical across all three bodies.
	originalMessages := gjson.GetBytes(originalBody, "messages").Raw
	prefillMessages := gjson.GetBytes(prefillBody, "messages").Raw
	decodeMessages := gjson.GetBytes(decodeBody, "messages").Raw
	assert.Equal(t, originalMessages, prefillMessages, "messages must be byte-identical in prefill")
	assert.Equal(t, originalMessages, decodeMessages, "messages must be byte-identical in decode")

	originalTools := gjson.GetBytes(originalBody, "tools").Raw
	prefillTools := gjson.GetBytes(prefillBody, "tools").Raw
	decodeTools := gjson.GetBytes(decodeBody, "tools").Raw
	assert.Equal(t, originalTools, prefillTools, "tools must be byte-identical in prefill")
	assert.Equal(t, originalTools, decodeTools, "tools must be byte-identical in decode")

	// Prefill control fields must be applied.
	assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_tokens").Int())
	assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool())

	// Decode body should retain client generation params.
	assert.Equal(t, int64(256), gjson.GetBytes(decodeBody, "max_tokens").Int())
	assert.True(t, gjson.GetBytes(decodeBody, "stream").Bool())
}

// TestPreparePayload_VLLMAndTRTLLMPreserveNestedBytes is the vLLM / TRT-LLM
// counterpart of the SGLang integration test: nested objects must be
// byte-identical between the original request, the prefill payload and the
// decode body, and the prefill payload must be identical across repeated
// calls. A map[string]any round trip would fail both properties because the
// nested keys below are deliberately not in sorted order.
func TestPreparePayload_VLLMAndTRTLLMPreserveNestedBytes(t *testing.T) {
	originalBody := []byte(`{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u","detail":"low"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"]}}}],"temperature":0.7,"max_tokens":128,"stream":true,"stream_options":{"include_usage":true},"min_tokens":5}`)

	for _, llmEngine := range []string{"vllm", "trtllm"} {
		t.Run(llmEngine, func(t *testing.T) {
			pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}}
			handler := engine.Resolve(llmEngine)

			var first []byte
			for i := 0; i < 100; i++ {
				routingCtx := &types.RoutingContext{
					ReqBody: originalBody,
					Context: context.Background(),
				}
				prefillBody, err := PreparePayload(routingCtx, pod, llmEngine, handler)
				require.NoError(t, err)

				// Decode body is the client body, byte for byte.
				assert.Equal(t, originalBody, routingCtx.ReqBody, "decode body must be the untouched client body")

				for _, path := range []string{"messages", "tools", "model", "temperature"} {
					assert.Equal(t, gjson.GetBytes(originalBody, path).Raw, gjson.GetBytes(prefillBody, path).Raw,
						"%s must be byte-identical in prefill", path)
				}
				assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_tokens").Int())
				assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool())
				assert.False(t, gjson.GetBytes(prefillBody, "stream_options").Exists())
				assert.False(t, gjson.GetBytes(prefillBody, "min_tokens").Exists())

				if llmEngine == "trtllm" {
					// disagg_request_id changes per call; blank it and compare the rest.
					require.True(t, gjson.GetBytes(prefillBody, "disaggregated_params.disagg_request_id").Exists())
					prefillBody, err = sjson.SetBytes(prefillBody, "disaggregated_params.disagg_request_id", 0)
					require.NoError(t, err)
				}
				if i == 0 {
					first = prefillBody
					continue
				}
				assert.Equal(t, string(first), string(prefillBody), "prefill payload must be stable across calls")
			}
		})
	}
}

// TestPreparePayload_SGLangHandlerDuplicateFieldsRejected verifies that
// ValidateSGLangRequest — called in Route() — rejects duplicate controlled
// fields. AugmentPrefillRequest itself no longer re-validates, so the test
// exercises ValidateSGLangRequest directly.
func TestPreparePayload_SGLangHandlerDuplicateFieldsRejected(t *testing.T) {
	originalBody := []byte(`{"model":"m","messages":[],"bootstrap_host":"a","bootstrap_host":"b"}`)
	err := engine.ValidateSGLangRequest(originalBody)
	require.Error(t, err)

	var invalidReqErr *engine.InvalidRequestError
	assert.True(t, errors.As(err, &invalidReqErr), "must return *InvalidRequestError")
}
