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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// newChatTokenizer serves different ids for chat-template and plain-text
// tokenization, the way a real chat template changes the prompt.
func newChatTokenizer(t *testing.T) (tok tokenizer.Tokenizer, chatTokens []int, calls *atomic.Int32) {
	t.Helper()
	chatTokens, textTokens := make([]int, 64), make([]int, 64)
	for i := range chatTokens {
		chatTokens[i], textTokens[i] = 1000+i, 1+i
	}
	calls = &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tokens := textTokens
		if req.Messages != nil {
			tokens = chatTokens
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens, "count": len(tokens)})
	}))
	t.Cleanup(server.Close)
	tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{Engine: "vllm", Endpoint: server.URL, Model: "test-model"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tok.(interface{ Close() error }).Close() })
	return tok, chatTokens, calls
}

func newChatRoutingContext(t *testing.T) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", "hello", "chat-request", "")
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)
	t.Cleanup(ctx.Delete)
	return ctx
}

func TestKVSyncPrefixCacheTokenizesChatRequestsLikeRoute(t *testing.T) {
	tok, chatTokens, _ := newChatTokenizer(t)
	router, pods := newPrefixContextRouter(t, tok, true)

	routeCtx := newChatRoutingContext(t)
	_, err := router.Route(routeCtx, pods)
	require.NoError(t, err)
	other := pods.All()[0]
	if other.Name == routeCtx.TargetPod().Name {
		other = pods.All()[1]
	}
	require.NoError(t, router.PostRouteUpdate(newChatRoutingContext(t), pods, other))

	// Both pods hold the chat-template blocks, which is what the engine reports.
	matched, _ := router.kvSyncRouter.syncIndexer.MatchPrefix("test-model", -1, tokenizer.IntToByteArray(chatTokens),
		map[string]struct{}{"/context-pod-1": {}, "/context-pod-2": {}})
	assert.Equal(t, map[string]int{"/context-pod-1": 100, "/context-pod-2": 100}, matched)
	scores, _, err := router.ScoreAll(newChatRoutingContext(t), pods)
	require.NoError(t, err)
	assert.Equal(t, []float64{100, 100}, scores)
}

// TestBlendedPrefixCacheRouteTokenizesOnce drives the sequence the multi-strategy router runs
// for a blended prefix-cache request, ScoreAll then PostRouteUpdate on one routing context, and
// checks the request is tokenized once across both.
func TestBlendedPrefixCacheRouteTokenizesOnce(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		t.Run(map[bool]string{false: "local-index", true: "kv-sync"}[kvSync], func(t *testing.T) {
			tok, _, calls := newChatTokenizer(t)
			router, pods := newPrefixContextRouter(t, tok, kvSync)
			ctx := newChatRoutingContext(t)

			_, _, err := router.ScoreAll(ctx, pods)
			require.NoError(t, err)
			require.NoError(t, router.PostRouteUpdate(ctx, pods, pods.All()[0]))
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}
