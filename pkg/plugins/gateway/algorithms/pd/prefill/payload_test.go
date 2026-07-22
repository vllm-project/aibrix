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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// fakeRawPreparer is a test double that simultaneously implements EngineHandler
// and RawPrefillPayloadPreparer. It records whether each method was called and
// returns a sentinel payload, allowing us to prove that PreparePayload routes
// through the raw-body path.
type fakeRawPreparer struct {
	augmentCalled   bool
	prepareCalled   bool
	sentinelPayload []byte
}

func (f *fakeRawPreparer) Name() string  { return "fake-raw" }
func (f *fakeRawPreparer) IsAsync() bool { return false }

func (f *fakeRawPreparer) AugmentPrefillRequest(
	_ *types.RoutingContext, _ *v1.Pod, _ map[string]any,
) error {
	f.augmentCalled = true
	return nil
}

func (f *fakeRawPreparer) MergePrefillResponse(
	_ *types.RoutingContext, _ map[string]any, _ *v1.Pod,
) error {
	return nil
}

func (f *fakeRawPreparer) PreparePrefillPayload(
	routingCtx *types.RoutingContext, _ *v1.Pod,
) ([]byte, error) {
	f.prepareCalled = true
	if f.sentinelPayload != nil {
		// Simulate side-effecting ReqBody like a real handler would.
		routingCtx.ReqBody = []byte(`{"decode":"body"}`)
		return f.sentinelPayload, nil
	}
	return nil, fmt.Errorf("sentinel error from fakeRawPreparer")
}

func TestPreparePayload_RoutesToRawPreparer(t *testing.T) {
	// Use a body that would FAIL sonic.Unmarshal. If PreparePayload tried the
	// unmarshal path, the test would fail. Since the fake implements
	// RawPrefillPayloadPreparer, the raw path is used and the sentinel payload
	// is returned without ever touching sonic.
	invalidJSON := []byte(`{this is not valid json`)

	routingCtx := &types.RoutingContext{
		ReqBody: invalidJSON,
		Context: context.Background(),
	}

	sentinel := []byte(`{"sentinel":true}`)
	handler := &fakeRawPreparer{sentinelPayload: sentinel}

	payload, err := PreparePayload(routingCtx, &v1.Pod{}, "fake-raw", handler)
	require.NoError(t, err)

	assert.Equal(t, sentinel, payload, "must return the sentinel payload from the raw preparer")
	assert.True(t, handler.prepareCalled, "PreparePrefillPayload must be called")
	assert.False(t, handler.augmentCalled, "AugmentPrefillRequest must NOT be called when raw preparer is used")
}

func TestPreparePayload_RawPreparerErrorPropagates(t *testing.T) {
	routingCtx := &types.RoutingContext{
		ReqBody: []byte(`{"valid":"json"}`),
		Context: context.Background(),
	}

	// sentinelPayload is nil → PreparePrefillPayload returns an error.
	handler := &fakeRawPreparer{}

	_, err := PreparePayload(routingCtx, &v1.Pod{}, "fake-raw", handler)
	assert.Error(t, err)
	assert.True(t, handler.prepareCalled, "PreparePrefillPayload must be called")
	assert.False(t, handler.augmentCalled, "AugmentPrefillRequest must NOT be called")
}

// fakeNonRawHandler implements only EngineHandler (not RawPrefillPayloadPreparer),
// ensuring the fallback unmarshal/marshal path is exercised.
type fakeNonRawHandler struct {
	augmentCalled bool
}

func (f *fakeNonRawHandler) Name() string  { return "fake-non-raw" }
func (f *fakeNonRawHandler) IsAsync() bool { return false }

func (f *fakeNonRawHandler) AugmentPrefillRequest(
	_ *types.RoutingContext, _ *v1.Pod, completionRequest map[string]any,
) error {
	f.augmentCalled = true
	completionRequest["injected"] = "by-handler"
	return nil
}

func (f *fakeNonRawHandler) MergePrefillResponse(
	_ *types.RoutingContext, _ map[string]any, _ *v1.Pod,
) error {
	return nil
}

func TestPreparePayload_FallbackToUnmarshalPath(t *testing.T) {
	routingCtx := &types.RoutingContext{
		ReqBody: []byte(`{"messages":[],"max_tokens":256,"stream":true}`),
		Context: context.Background(),
	}

	handler := &fakeNonRawHandler{}

	payload, err := PreparePayload(routingCtx, &v1.Pod{}, "fake-non-raw", handler)
	require.NoError(t, err)

	assert.True(t, handler.augmentCalled, "AugmentPrefillRequest must be called for non-raw handlers")

	// Verify common prefill constraints were applied.
	assert.Contains(t, string(payload), `"max_tokens":1`)
	assert.Contains(t, string(payload), `"max_completion_tokens":1`)
	assert.Contains(t, string(payload), `"stream":false`)
	assert.NotContains(t, string(payload), "stream_options")
	assert.NotContains(t, string(payload), "min_tokens")
}

// TestPreparePayload_SGLangHandlerIntegration exercises the full dispatcher
// path: PreparePayload → SGLangHandler.PreparePrefillPayload. It verifies that
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

// TestPreparePayload_SGLangHandlerDuplicateFieldsRejected verifies that the
// RawPrefillPayloadPreparer path (PreparePayload → SGLangHandler.PreparePrefillPayload)
// rejects duplicate controlled fields and returns *InvalidRequestError without
// modifying routingCtx.ReqBody.
func TestPreparePayload_SGLangHandlerDuplicateFieldsRejected(t *testing.T) {
	originalBody := []byte(`{"model":"m","messages":[],"bootstrap_host":"a","bootstrap_host":"b"}`)
	routingCtx := &types.RoutingContext{
		ReqBody: originalBody,
		Context: context.Background(),
	}
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}}
	handler := engine.Resolve("sglang")

	_, err := PreparePayload(routingCtx, pod, "sglang", handler)
	require.Error(t, err)

	var invalidReqErr *engine.InvalidRequestError
	assert.True(t, errors.As(err, &invalidReqErr), "must return *InvalidRequestError")

	// ReqBody must not be modified on error.
	assert.Equal(t, originalBody, routingCtx.ReqBody, "ReqBody must be unchanged on error")
}
