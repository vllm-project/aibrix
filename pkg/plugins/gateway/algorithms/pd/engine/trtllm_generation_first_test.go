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
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

type trtInfoProviderFunc func(context.Context, *v1.Pod) (TRTServerInfo, error)

func (f trtInfoProviderFunc) Get(ctx context.Context, pod *v1.Pod) (TRTServerInfo, error) {
	return f(ctx, pod)
}

func TestTRTScheduleStyle(t *testing.T) {
	for _, style := range []string{"", TRTContextFirst} {
		h, err := NewTRTLLMHandler(style, nil)
		require.NoError(t, err)
		assert.False(t, h.IsAsync())
	}
	_, err := NewTRTLLMHandler("parallel", nil)
	require.ErrorContains(t, err, "AIBRIX_TRT_SCHEDULE_STYLE")
	_, err = NewTRTLLMHandler(TRTGenerationFirst, nil)
	require.ErrorContains(t, err, "server_info provider")
	h, err := NewTRTLLMHandler(TRTGenerationFirst, trtInfoProviderFunc(func(context.Context, *v1.Pod) (TRTServerInfo, error) {
		return TRTServerInfo{}, nil
	}))
	require.NoError(t, err)
	assert.True(t, h.IsAsync())
	assert.False(t, Resolve(pd.EngineTRTLLM).IsAsync(), "a router-local mode must not mutate the registry")
}

func TestPrepareTRTGenerationFirstPreservesBodiesAndIDs(t *testing.T) {
	info := TRTServerInfo{ContextInfoEndpoint: "tcp://ctx:5555", ContextDPRank: 7, EncodedOpaqueState: "opaque"}
	for name, body := range map[string]string{
		"chat":             trtClientBody,
		"text completion":  `{"prompt":"hello","stream":false,"max_tokens":123,"temperature":0.25}`,
		"token completion": `{"prompt":[1,2,3],"stream":true,"stream_options":{"include_usage":true}}`,
		"tools":            `{"messages":[{"role":"user","content":"hi"}],"tools":[{"function":{"parameters":{"z":0,"a":1},"name":"f"},"type":"function"}],"seed":42,"n":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, id := range []int64{9007199254740993, 9223372036854775807} {
				input := []byte(body)
				p, d, err := prepareTRTGenerationFirst(input, info, id)
				require.NoError(t, err)
				assert.Equal(t, body, string(input))
				for _, key := range []string{"messages", "prompt", "tools", "stream", "stream_options", "max_tokens", "temperature", "seed", "n"} {
					assert.Equal(t, gjson.Get(body, key).Raw, gjson.GetBytes(p, key).Raw)
					assert.Equal(t, gjson.Get(body, key).Raw, gjson.GetBytes(d, key).Raw)
				}
				for _, b := range [][]byte{p, d} {
					params := gjson.GetBytes(b, "disaggregated_params")
					assert.Equal(t, strconv.FormatInt(id, 10), params.Get("disagg_request_id").Raw)
					assert.Equal(t, "1", params.Get("schedule_style").Raw)
					assert.False(t, params.Get("stale").Exists())
				}
				assert.Equal(t, "context_only", gjson.GetBytes(p, "disaggregated_params.request_type").String())
				params := gjson.GetBytes(d, "disaggregated_params")
				assert.Equal(t, "generation_only", params.Get("request_type").String())
				assert.Equal(t, strconv.FormatInt(id, 10), params.Get("ctx_request_id").Raw)
				assert.Equal(t, info.ContextInfoEndpoint, params.Get("ctx_info_endpoint").String())
				assert.EqualValues(t, 7, params.Get("ctx_dp_rank").Int())
				assert.Equal(t, "opaque", params.Get("encoded_opaque_state").String())
			}
		})
	}
}

func TestTRTGenerationFirstAugment(t *testing.T) {
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.7"}}
	h, err := NewTRTLLMHandler(TRTGenerationFirst, trtInfoProviderFunc(func(_ context.Context, selected *v1.Pod) (TRTServerInfo, error) {
		assert.Same(t, pod, selected)
		return TRTServerInfo{ContextInfoEndpoint: "tcp://10.0.0.7:1234", ContextDPRank: 2}, nil
	}))
	require.NoError(t, err)
	ctx := &types.RoutingContext{Context: context.Background(), ReqBody: []byte(trtClientBody)}
	p, err := h.AugmentPrefillRequest(ctx, pod, ctx.ReqBody)
	require.NoError(t, err)
	id := gjson.GetBytes(p, "disaggregated_params.disagg_request_id")
	assert.GreaterOrEqual(t, id.Int(), TRTMinGlobalID)
	assert.Equal(t, id.Raw, gjson.GetBytes(ctx.ReqBody, "disaggregated_params.disagg_request_id").Raw)
	assert.Empty(t, ctx.PDRequestID(), "TRT disagg IDs must not become SGLang abort rids")
	decodeBody := string(ctx.ReqBody)
	require.NoError(t, h.MergePrefillResponse(ctx, []byte(`{"prompt_token_ids":[999]}`), pod))
	assert.Equal(t, decodeBody, string(ctx.ReqBody), "late CTX response must never mutate the decode body")
}

func TestTRTGenerationFirstPreparationFailureIsAtomic(t *testing.T) {
	for _, provider := range []trtInfoProviderFunc{
		func(context.Context, *v1.Pod) (TRTServerInfo, error) {
			return TRTServerInfo{}, errors.New("worker unavailable")
		},
		func(context.Context, *v1.Pod) (TRTServerInfo, error) { return TRTServerInfo{ContextDPRank: -1}, nil },
	} {
		h, err := NewTRTLLMHandler(TRTGenerationFirst, provider)
		require.NoError(t, err)
		ctx := &types.RoutingContext{Context: context.Background(), ReqBody: []byte(trtClientBody)}
		_, err = h.AugmentPrefillRequest(ctx, &v1.Pod{}, ctx.ReqBody)
		require.Error(t, err)
		assert.Equal(t, trtClientBody, string(ctx.ReqBody))
	}
}
