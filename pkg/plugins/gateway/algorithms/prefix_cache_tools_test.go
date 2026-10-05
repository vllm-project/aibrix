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

package routingalgorithms

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

	tools := rawChatTools(body)
	// The raw bytes are kept, including fields the OpenAI types do not model.
	assert.Equal(t, `[{"type":"function","function":{"name":"f","x-extra":1}}]`, string(tools))

	input, err := buildTokenizeInputFromChatRequest(&chatReq, tools)
	require.NoError(t, err)
	assert.Equal(t, tools, input.Tools)

	input, err = buildTokenizeInputFromChatRequest(&chatReq, nil)
	require.NoError(t, err)
	assert.Nil(t, input.Tools)
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
