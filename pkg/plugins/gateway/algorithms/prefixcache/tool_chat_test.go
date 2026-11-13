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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// TestKVSyncToolChat verifies that KV-sync chat tokenization preserves tool-call history
// (tool_calls and tool_call_id) as well as nullable or omitted assistant content and unmodelled
// fields (such as reasoning_content), preventing unintended fallback to plain-text tokenization (#3307).
func TestKVSyncToolChat(t *testing.T) {
	cases := []struct {
		name     string
		messages string
	}{
		{"plain_control", `[{"role":"user","content":"Weather in Paris?"}]`},
		{"assistant_text", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","content":"Checking.","reasoning_content":"Checking current weather conditions.","tool_calls":[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_weather","content":"sunny"}]`},
		{"assistant_null", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_weather","content":"sunny"}]`},
		{"assistant_omitted", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","tool_calls":[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_weather","content":"sunny"}]`},
		{"multimodal_tool_history", `[{"role":"user","content":[{"type":"text","text":"What is in this image?"},{"type":"image_url","image_url":{"url":"https://example.com/pic.jpg"}}]},{"role":"assistant","content":"Let me inspect it.","tool_calls":[{"id":"call_vision","type":"function","function":{"name":"analyze_image","arguments":"{\"detail\":\"high\"}"}}]},{"role":"tool","tool_call_id":"call_vision","content":"A sunny beach"}]`},
		{"function_call_and_name", `[{"role":"user","name":"alice","content":"Weather in Paris?"},{"role":"assistant","name":"bot","content":null,"function_call":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"role":"function","name":"get_weather","content":"sunny"}]`},
	}
	for _, tc := range cases {
		for _, operation := range prefixContextOperations {
			t.Run(tc.name+"/"+operation.name, func(t *testing.T) {
				captured := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					captured <- body
					tokens := make([]int, 64)
					for i := range tokens {
						tokens[i] = i + 1
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens, "count": len(tokens)})
				}))
				t.Cleanup(server.Close)
				tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{
					Engine: "vllm", Endpoint: server.URL, Model: "test-model", Timeout: time.Second,
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = tok.(interface{ Close() error }).Close() })
				router, pods := newPrefixContextRouter(t, tok, true)
				ctx := newChatRoutingContext(t)
				ctx.ReqBody = []byte(`{"model":"test-model","messages":` + tc.messages + `,"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
				require.NoError(t, operation.call(router, ctx, pods))
				var body []byte
				select {
				case body = <-captured:
				case <-time.After(time.Second):
					t.Fatal("no tokenizer HTTP request recorded")
				}
				var got struct {
					Messages json.RawMessage `json:"messages"`
					Prompt   json.RawMessage `json:"prompt"`
					Tools    json.RawMessage `json:"tools"`
				}
				require.NoError(t, json.Unmarshal(body, &got))
				require.NotEmpty(t, got.Messages, "valid tool chat fell back to plain-text tokenization: %s", got.Prompt)
				assert.JSONEq(t, tc.messages, string(got.Messages), "chat-template input must preserve the tool-call history")
				assert.Contains(t, string(got.Tools), "get_weather", "top-level tool definitions are a separate, already supported path")
			})
		}
	}
}

// TestKVSyncToolChat_NegativeCases verifies that chat requests with invalid messages
// (such as content: null or content omitted without tool calls, or empty tool_calls: [])
// cleanly fall back to plain-text tokenization rather than causing routing errors.
func TestKVSyncToolChat_NegativeCases(t *testing.T) {
	cases := []struct {
		name     string
		messages string
	}{
		{"content_null_no_tools", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","content":null}]`},
		{"tool_calls_empty_array_content_null", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","content":null,"tool_calls":[]}]`},
		{"tool_calls_empty_array_content_omitted", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","tool_calls":[]}]`},
		{"content_omitted_no_tools", `[{"role":"user","content":"Weather in Paris?"},{"role":"assistant"}]`},
	}
	for _, tc := range cases {
		for _, operation := range prefixContextOperations {
			t.Run(tc.name+"/"+operation.name, func(t *testing.T) {
				captured := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					captured <- body
					tokens := make([]int, 64)
					for i := range tokens {
						tokens[i] = i + 1
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens, "count": len(tokens)})
				}))
				t.Cleanup(server.Close)
				tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{
					Engine: "vllm", Endpoint: server.URL, Model: "test-model", Timeout: time.Second,
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = tok.(interface{ Close() error }).Close() })
				router, pods := newPrefixContextRouter(t, tok, true)
				ctx := newChatRoutingContext(t)
				ctx.ReqBody = []byte(`{"model":"test-model","messages":` + tc.messages + `}`)
				require.NoError(t, operation.call(router, ctx, pods))
				var body []byte
				select {
				case body = <-captured:
				case <-time.After(time.Second):
					t.Fatal("no tokenizer HTTP request recorded")
				}
				var got struct {
					Messages json.RawMessage `json:"messages"`
					Prompt   json.RawMessage `json:"prompt"`
					Tools    json.RawMessage `json:"tools"`
				}
				require.NoError(t, json.Unmarshal(body, &got))
				assert.Empty(t, got.Messages, "invalid chat messages must fall back to plain-text tokenization")
				assert.NotEmpty(t, got.Prompt, "fallback must send prompt to text tokenization")
			})
		}
	}
}
