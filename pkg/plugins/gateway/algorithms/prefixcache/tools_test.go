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

package prefixcache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// TestPrefixCache_UsesPrefixText checks that prefix matching hashes PrefixMatchText when
// set: requests sharing their messages but not their tools must not look like a match.
func TestPrefixCache_UsesPrefixText(t *testing.T) {
	c := cache.NewWithPodsMetricsForTest(
		getReadyPods(),
		"m1",
		map[string]map[string]metrics.MetricValue{
			"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
			"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
		})
	podList := podsFromCache(c)

	tokenizerObj, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)
	router := prefixCacheRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}

	message := strings.Repeat("shared conversation ", 8)
	newCtx := func(requestID, tools string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", message, requestID, "")
		ctx.PrefixMatchText = tools + " " + message
		return ctx
	}
	maxScore := func(ctx *types.RoutingContext) float64 {
		scores, _, err := router.ScoreAll(ctx, podList)
		require.NoError(t, err)
		best := 0.0
		for _, s := range scores {
			best = max(best, s)
		}
		return best
	}

	toolsA := `[{"function":{"name":"get_weather"},"type":"function"}]`
	toolsB := `[{"function":{"name":"web_search"},"type":"function"}]`
	_, err = router.Route(newCtx("r1", toolsA), podList)
	require.NoError(t, err)

	assert.Equal(t, 100.0, maxScore(newCtx("r2", toolsA)), "same tools and messages must fully match")
	// Only the blocks ahead of the differing tool name match; hashing Message alone
	// would have reported a full match.
	assert.Less(t, maxScore(newCtx("r3", toolsB)), 50.0, "different tools must not look like a full match")
}

func TestBuildTokenizeInputFromChatRequestTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","x-extra":1}}]}`)
	var chatReq types.ChatCompletionRequest
	require.NoError(t, json.Unmarshal(body, &chatReq))

	rawMsgs := rawChatMessages(body)
	assert.Equal(t, `[{"role":"user","content":"hi"}]`, string(rawMsgs))

	tools := rawChatTools(body)
	// The raw bytes are kept, including fields the OpenAI types do not model.
	assert.Equal(t, `[{"type":"function","function":{"name":"f","x-extra":1}}]`, string(tools))

	input, err := buildTokenizeInputFromChatRequest(&chatReq, rawMsgs, tools)
	require.NoError(t, err)
	assert.Equal(t, rawMsgs, input.Messages)
	assert.Equal(t, tools, input.Tools)

	// Fallback to chatReq.Messages when rawMsgs is nil
	input, err = buildTokenizeInputFromChatRequest(&chatReq, nil, tools)
	require.NoError(t, err)
	assert.JSONEq(t, string(rawMsgs), string(input.Messages))
	assert.Equal(t, tools, input.Tools)

	input, err = buildTokenizeInputFromChatRequest(&chatReq, rawMsgs, nil)
	require.NoError(t, err)
	assert.Nil(t, input.Tools)
}

func TestBuildTokenizeInputFromChatRequest_Validation(t *testing.T) {
	t.Run("empty messages", func(t *testing.T) {
		var chatReq types.ChatCompletionRequest
		_, err := buildTokenizeInputFromChatRequest(&chatReq, nil, nil)
		require.ErrorContains(t, err, "no messages in chat completion request")
	})

	t.Run("missing role", func(t *testing.T) {
		body := []byte(`{"messages":[{"content":"hi"}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		_, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.ErrorContains(t, err, "message at index 0 has no role")
	})

	t.Run("content null without tool calls", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		_, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.ErrorContains(t, err, "message at index 1 has nil content")
	})

	t.Run("content null with empty tool_calls array", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[]}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		_, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.ErrorContains(t, err, "message at index 1 has nil content")
	})

	t.Run("content null with empty function_call object", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"function_call":{}}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		_, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.ErrorContains(t, err, "message at index 1 has nil content")
	})

	t.Run("content omitted without tool calls", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant"}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		_, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.ErrorContains(t, err, "message at index 1 has nil content")
	})

	t.Run("valid with tool_calls and null content", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f"}}]}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		input, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.NoError(t, err)
		assert.NotEmpty(t, input.Messages)
	})

	t.Run("valid with function_call and null content", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		input, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.NoError(t, err)
		assert.NotEmpty(t, input.Messages)
	})

	t.Run("valid with tool_calls and omitted content", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f"}}]}]}`)
		var chatReq types.ChatCompletionRequest
		require.NoError(t, json.Unmarshal(body, &chatReq))
		input, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatMessages(body), nil)
		require.NoError(t, err)
		assert.NotEmpty(t, input.Messages)
	})
}

func TestRawChatMessages(t *testing.T) {
	cases := map[string]string{
		"absent":  `{"tools":[]}`,
		"null":    `{"messages":null}`,
		"object":  `{"messages":{}}`,
		"invalid": `{"messages":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, rawChatMessages([]byte(body)))
		})
	}
	assert.Equal(t, `[]`, string(rawChatMessages([]byte(`{"messages": [] }`))))
}

func TestRawChatTools(t *testing.T) {
	cases := map[string]string{
		"absent":  `{"messages":[]}`,
		"null":    `{"messages":[],"tools":null}`,
		"object":  `{"messages":[],"tools":{}}`,
		"invalid": `{"messages":[],"tools":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, rawChatTools([]byte(body)))
		})
	}
	assert.Equal(t, `[]`, string(rawChatTools([]byte(`{"tools": [] }`))))
}
