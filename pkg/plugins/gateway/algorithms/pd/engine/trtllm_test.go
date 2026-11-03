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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

const trtClientBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u","detail":"low"}}]}],"disaggregated_params":{"request_type":"generation_only","stale":true},"stream":true}`

// bigInt is 2^53+1: it cannot be represented exactly as a float64, so it
// detects any path that decodes JSON numbers into float64.
const bigInt = "9007199254740993"

func TestTRTLLMHandler_AugmentPrefillRequest(t *testing.T) {
	h := &TRTLLMHandler{}
	in := []byte(trtClientBody)
	orig := append([]byte(nil), in...)

	out, err := h.AugmentPrefillRequest(&types.RoutingContext{}, &v1.Pod{}, in)
	require.NoError(t, err)
	assert.Equal(t, orig, in, "input must not be mutated")
	assert.Equal(t, gjson.GetBytes(in, "messages").Raw, gjson.GetBytes(out, "messages").Raw)

	dp := gjson.GetBytes(out, "disaggregated_params")
	require.True(t, dp.IsObject())
	assert.Equal(t, "context_only", dp.Get("request_type").String())
	assert.False(t, dp.Get("stale").Exists(), "client-supplied disaggregated_params must be replaced")
	id := dp.Get("disagg_request_id")
	assert.Equal(t, gjson.Number, id.Type)
	assert.GreaterOrEqual(t, id.Int(), TRTMinGlobalID)
	assert.Equal(t, id.Raw, gjson.GetBytes(out, "disaggregated_params.disagg_request_id").Raw)
}

func TestTRTLLMHandler_MergePrefillResponse(t *testing.T) {
	h := &TRTLLMHandler{}
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}}
	newCtx := func(path string) *types.RoutingContext {
		return &types.RoutingContext{
			RequestID: "trt",
			ReqPath:   path,
			ReqBody:   []byte(trtClientBody),
			Context:   context.Background(),
		}
	}

	t.Run("choices[0] params, completions routes prompt_token_ids to prompt", func(t *testing.T) {
		resp := []byte(`{"choices":[{"index":0,"disaggregated_params":{"request_type":"context_only","ctx_request_id":` + bigInt + `,"opaque_state":"abc"}}],"prompt_token_ids":[10,20,30]}`)
		ctx := newCtx("/v1/completions")
		require.NoError(t, h.MergePrefillResponse(ctx, resp, pod))

		assert.Equal(t, gjson.Get(trtClientBody, "messages").Raw, gjson.GetBytes(ctx.ReqBody, "messages").Raw)
		dp := gjson.GetBytes(ctx.ReqBody, "disaggregated_params")
		assert.Equal(t, "generation_only", dp.Get("request_type").String())
		assert.Equal(t, bigInt, dp.Get("ctx_request_id").Raw, "large ints must not lose precision")
		assert.Equal(t, "abc", dp.Get("opaque_state").String())
		assert.False(t, dp.Get("stale").Exists())
		assert.Equal(t, "[10,20,30]", gjson.GetBytes(ctx.ReqBody, "prompt").Raw)
		assert.False(t, gjson.GetBytes(ctx.ReqBody, "prompt_token_ids").Exists())
	})

	t.Run("top-level params win, chat keeps prompt_token_ids", func(t *testing.T) {
		resp := []byte(`{"disaggregated_params":{"request_type":"context_only","disagg_request_id":` + bigInt + `},"choices":[{"disaggregated_params":{"request_type":"other"}}],"prompt_token_ids":[1]}`)
		ctx := newCtx("/v1/chat/completions")
		require.NoError(t, h.MergePrefillResponse(ctx, resp, pod))

		assert.Equal(t, bigInt, gjson.GetBytes(ctx.ReqBody, "disaggregated_params.disagg_request_id").Raw)
		assert.Equal(t, "generation_only", gjson.GetBytes(ctx.ReqBody, "disaggregated_params.request_type").String())
		assert.Equal(t, "[1]", gjson.GetBytes(ctx.ReqBody, "prompt_token_ids").Raw)
		assert.False(t, gjson.GetBytes(ctx.ReqBody, "prompt").Exists())
	})

	t.Run("non-array prompt_token_ids is ignored", func(t *testing.T) {
		resp := []byte(`{"disaggregated_params":{"request_type":"context_only"},"prompt_token_ids":null}`)
		ctx := newCtx("/v1/completions")
		require.NoError(t, h.MergePrefillResponse(ctx, resp, pod))
		assert.False(t, gjson.GetBytes(ctx.ReqBody, "prompt").Exists())
	})

	t.Run("missing params is a no-op", func(t *testing.T) {
		ctx := newCtx("/v1/completions")
		require.NoError(t, h.MergePrefillResponse(ctx, []byte(`{"choices":[{"message":{"content":"x"}}]}`), pod))
		assert.Equal(t, trtClientBody, string(ctx.ReqBody))
	})

	t.Run("wrong type is an error and leaves ReqBody unchanged", func(t *testing.T) {
		ctx := newCtx("/v1/completions")
		err := h.MergePrefillResponse(ctx, []byte(`{"disaggregated_params":[1]}`), pod)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "disaggregated_params has unexpected type")
		assert.Equal(t, trtClientBody, string(ctx.ReqBody))
	})

	t.Run("invalid decode body is an error", func(t *testing.T) {
		ctx := newCtx("/v1/completions")
		ctx.ReqBody = []byte(`not json`)
		require.Error(t, h.MergePrefillResponse(ctx, []byte(`{"disaggregated_params":{}}`), pod))
	})
}
