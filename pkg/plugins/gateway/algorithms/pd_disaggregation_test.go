/*
Copyright 2024 The Aibrix Team.

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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPDRouter_Route(t *testing.T) {
	tests := []struct {
		name        string
		readyPods   []*v1.Pod
		serverCode  int
		serverResp  string
		llmEngine   string
		expectError bool
		expectMsg   string
	}{
		{
			name: "successful routing with both prefill and decode pods",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}, Name: "prefill-1"}, Status: v1.PodStatus{PodIP: "127.0.0.1",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}, Name: "decode-1"}, Status: v1.PodStatus{PodIP: "127.0.0.2",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			},
			serverCode:  http.StatusOK,
			llmEngine:   "vllm",
			expectError: false,
			expectMsg:   "127.0.0.2:8000",
		},
		{
			name: "missing prefill pod",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role-name": "decode"}, Name: "decode-1"}, Status: v1.PodStatus{PodIP: "127.0.0.2"}},
			},
			serverCode:  http.StatusOK,
			serverResp:  "",
			llmEngine:   "vllm",
			expectError: true,
			expectMsg:   "",
		},
	}

	r := pdRouter{
		cache:              cache.NewForTest(),
		tokenizer:          tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupTestServer(t, tt.serverCode, tt.serverResp, tt.llmEngine)
			defer ts.Close()

			ctx := types.NewRoutingContext(context.Background(), "test", "model", "message", "test-request", "user")
			ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"test"}],"stream":true}`)

			result, err := r.Route(ctx, &utils.PodArray{Pods: tt.readyPods})

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectMsg, result)
			}
		})
	}
}

func TestFilterPrefillDecodePods(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*v1.Pod
		expectPrefill int
		expectDecode  int
	}{
		{
			name: "filter correct roles",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{}},
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role-name": "decode"}}},
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role-name": "other"}}},
				{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role-name": "prefill", PodGroupIndex: "1"}}},
			},
			expectPrefill: 1,
			expectDecode:  1,
		},
	}

	r := pdRouter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefill, decode, err := r.filterPrefillDecodePods(tt.pods)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectPrefill, len(prefill))
			assert.Equal(t, tt.expectDecode, len(decode))
		})
	}
}

func TestDoPrefillRequest(t *testing.T) {
	// Common test data
	createPrefillPod := func(name, engine string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					LLMEngineIdentifier: engine,
					PDRoleIdentifier:    "prefill",
				},
			},
			Status: v1.PodStatus{
				PodIP: "127.0.0.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		}
	}

	createRoutingCtx := func() *types.RoutingContext {
		return &types.RoutingContext{
			RequestID: "test-request",
			Model:     "test-model",
			ReqPath:   "/v1/chat/completions",
			ReqBody:   []byte(`{"messages":[{"role":"user","content":"test"}],"stream":true}`),
			ReqHeaders: map[string]string{
				"Authorization": "Bearer test-1234",
			},
		}
	}

	createRouter := func(pods []*v1.Pod) *pdRouter {
		tokenizerObj, err := tokenizer.NewTokenizer("character", nil)
		if err != nil {
			t.Fatal(err)
		}
		return &pdRouter{
			prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
			cache:              cache.NewWithPodsForTest(pods, "m1"),
			tokenizer:          tokenizerObj,
		}
	}

	tests := []struct {
		name        string
		serverCode  int
		serverResp  string
		llmEngine   string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "successful vllm prefill request",
			serverCode:  http.StatusOK,
			llmEngine:   "vllm",
			expectError: false,
		},
		{
			name:        "failed prefill request",
			serverCode:  http.StatusInternalServerError,
			serverResp:  "server error",
			llmEngine:   "vllm",
			expectError: true,
			errorMsg:    "http prefill request failed with status 500",
		},
		{
			name:        "async sglang prefill request",
			serverCode:  http.StatusOK,
			llmEngine:   "sglang",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupTestServer(t, tt.serverCode, tt.serverResp, tt.llmEngine)
			defer ts.Close()

			prefillPods := []*v1.Pod{
				createPrefillPod("p1", tt.llmEngine),
				createPrefillPod("p2", tt.llmEngine),
			}

			routingCtx := createRoutingCtx()
			router := createRouter(prefillPods)

			_, err := router.doPrefillRequest(routingCtx, prefillPods, tt.llmEngine)
			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				assert.NoError(t, err)
				if tt.llmEngine == "sglang" {
					time.Sleep(100 * time.Millisecond) // Wait for async goroutine
				}
			}
		})
	}
}

func TestPreparePrefillPayload(t *testing.T) {
	tests := []struct {
		name      string
		llmEngine string
		reqBody   string
		checkKV   bool
	}{
		{
			name:      "vllm engine adds kv_transfer_params",
			llmEngine: VLLMEngine,
			reqBody:   `{"messages":[{"role":"user","content":"test"}],"stream":true}`,
			checkKV:   true,
		},
		{
			name:      "sglang engine no kv_transfer_params",
			llmEngine: SGLangEngine,
			reqBody:   `{"messages":[{"role":"user","content":"test"}],"stream":true}`,
			checkKV:   false,
		},
		{
			name:      "other engine no kv_transfer_params",
			llmEngine: "other",
			reqBody:   `{"messages":[{"role":"user","content":"test"}],"stream":true}`,
			checkKV:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &pdRouter{}
			pod := &v1.Pod{
				Status: v1.PodStatus{PodIP: "127.0.0.1"},
			}
			routingCtx := &types.RoutingContext{
				ReqBody: []byte(tt.reqBody),
			}

			payload, err := router.preparePrefillPayload(routingCtx, pod, tt.llmEngine)
			assert.NoError(t, err)

			var result map[string]any
			err = json.Unmarshal(payload, &result)
			assert.NoError(t, err)

			// Check basic prefill parameters
			assert.Equal(t, float64(1), result["max_tokens"])
			assert.Equal(t, float64(1), result["max_completion_tokens"])
			assert.Equal(t, false, result["stream"])
			_, exists := result["stream_options"]
			assert.False(t, exists)

			// Check KV transfer params
			kvParams, hasKV := result["kv_transfer_params"]
			if tt.checkKV {
				assert.True(t, hasKV, "vLLM should have kv_transfer_params")
				kvMap := kvParams.(map[string]any)
				assert.Equal(t, true, kvMap["do_remote_decode"])
				assert.Equal(t, false, kvMap["do_remote_prefill"])
			} else {
				assert.False(t, hasKV, "non-vLLM engines should not have kv_transfer_params")
			}
		})
	}
}

func TestUpdateRoutingContextWithKVTransferParams(t *testing.T) {
	tests := []struct {
		name         string
		responseData map[string]any
		originalBody string
		expectError  bool
		expectKV     bool
	}{
		{
			name: "successful kv params update",
			responseData: map[string]any{
				"kv_transfer_params": map[string]any{
					"remote_engine_id": "engine123",
					"remote_block_ids": []string{"block1", "block2"},
				},
			},
			originalBody: `{"messages":[{"role":"user","content":"test"}]}`,
			expectError:  false,
			expectKV:     true,
		},
		{
			name:         "no kv params in response",
			responseData: map[string]any{"other": "data"},
			originalBody: `{"messages":[{"role":"user","content":"test"}]}`,
			expectError:  false,
			expectKV:     false,
		},
		{
			name:         "invalid json body",
			responseData: map[string]any{"kv_transfer_params": map[string]any{}},
			originalBody: `invalid json`,
			expectError:  true,
			expectKV:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &pdRouter{}
			pod := &v1.Pod{
				Status: v1.PodStatus{PodIP: "192.168.1.100"},
			}
			routingCtx := &types.RoutingContext{
				RequestID: "test-request",
				ReqBody:   []byte(tt.originalBody),
			}

			err := router.updateRoutingContextWithKVTransferParams(routingCtx, tt.responseData, pod)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			if tt.expectKV {
				// Parse updated request body
				var updatedRequest map[string]any
				err = json.Unmarshal(routingCtx.ReqBody, &updatedRequest)
				assert.NoError(t, err)

				// Check KV transfer params were added
				kvParams, exists := updatedRequest["kv_transfer_params"]
				assert.True(t, exists)

				// Check remote_host was added
				kvMap := kvParams.(map[string]any)
				assert.Equal(t, "192.168.1.100", kvMap["remote_host"])
			}
		})
	}
}

func TestVLLMIntegrationWithTestServer(t *testing.T) {
	// Integration test: verify vLLM prefill request extracts KV params from test server
	ts := setupTestServer(t, http.StatusOK, "", VLLMEngine) // Empty resp means use default vLLM response
	defer ts.Close()

	prefillPods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prefill-test",
			Labels: map[string]string{
				LLMEngineIdentifier: VLLMEngine,
				PDRoleIdentifier:    "prefill",
			},
		},
		Status: v1.PodStatus{
			PodIP: "127.0.0.1",
			Conditions: []v1.PodCondition{{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			}},
		},
	}}

	routingCtx := &types.RoutingContext{
		RequestID:  "integration-test",
		Model:      "test-model",
		ReqPath:    "/v1/chat/completions",
		ReqBody:    []byte(`{"messages":[{"role":"user","content":"test"}]}`),
		ReqHeaders: map[string]string{"Authorization": "Bearer test"},
	}

	router := &pdRouter{
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
		cache:              cache.NewWithPodsForTest(prefillPods, "test-model"),
		tokenizer:          tokenizer.NewCharacterTokenizer(),
	}

	_, err := router.doPrefillRequest(routingCtx, prefillPods, VLLMEngine)
	assert.NoError(t, err)

	// Verify that routing context was updated with KV transfer params from test server
	var updatedRequest map[string]any
	err = json.Unmarshal(routingCtx.ReqBody, &updatedRequest)
	assert.NoError(t, err)

	kvParams, exists := updatedRequest["kv_transfer_params"]
	assert.True(t, exists, "KV transfer params should be extracted from test server response")

	kvMap := kvParams.(map[string]any)
	assert.Equal(t, "127.0.0.1", kvMap["remote_host"], "remote_host should be set to prefill pod IP")
	assert.Equal(t, "test-engine-123", kvMap["remote_engine_id"], "remote_engine_id should match test server response")
	assert.Equal(t, true, kvMap["do_remote_decode"], "do_remote_decode should be true from test server")
	assert.Equal(t, false, kvMap["do_remote_prefill"], "do_remote_prefill should be false from test server")

	// Verify remote_block_ids is present
	blockIds, ok := kvMap["remote_block_ids"].([]any)
	assert.True(t, ok, "remote_block_ids should be an array")
	assert.Len(t, blockIds, 2, "should have 2 block IDs from test server")
	assert.Equal(t, "block1", blockIds[0])
	assert.Equal(t, "block2", blockIds[1])
}

func TestVLLMKVTransferProcessing(t *testing.T) {
	// Test that updateRoutingContextWithKVTransferParams works correctly for vLLM
	tests := []struct {
		name     string
		response map[string]any
		checkKV  bool
	}{
		{
			name: "vllm with kv_transfer_params",
			response: map[string]any{
				"kv_transfer_params": map[string]any{
					"remote_engine_id": "test-engine",
				},
			},
			checkKV: true,
		},
		{
			name: "vllm without kv_transfer_params",
			response: map[string]any{
				"other_data": "value",
			},
			checkKV: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &pdRouter{}
			pod := &v1.Pod{Status: v1.PodStatus{PodIP: "192.168.1.1"}}
			routingCtx := &types.RoutingContext{
				RequestID: "test-req",
				ReqBody:   []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			}

			// Call the update function (this is only called for vLLM in real flow)
			err := router.updateRoutingContextWithKVTransferParams(routingCtx, tt.response, pod)
			assert.NoError(t, err)

			if tt.checkKV {
				// Body should be updated when KV params are present
				var updatedRequest map[string]any
				err = json.Unmarshal(routingCtx.ReqBody, &updatedRequest)
				assert.NoError(t, err)

				kvParams, exists := updatedRequest["kv_transfer_params"]
				assert.True(t, exists)
				if kvMap, ok := kvParams.(map[string]any); ok {
					assert.Equal(t, "192.168.1.1", kvMap["remote_host"])
					assert.Equal(t, "test-engine", kvMap["remote_engine_id"])
				}
			} else {
				// Body should remain unchanged when no KV params are present
				var updatedRequest map[string]any
				err = json.Unmarshal(routingCtx.ReqBody, &updatedRequest)
				assert.NoError(t, err)
				_, exists := updatedRequest["kv_transfer_params"]
				assert.False(t, exists, "KV params should not be added when not in response")
			}
		})
	}
}

// Common test utilities
func setupTestServer(t *testing.T, code int, resp string, llmEngine string) *httptest.Server {
	l, err := net.Listen("tcp", "127.0.0.1:8000")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read request body: %v", err)
		}

		var completionRequest map[string]any
		if err := json.Unmarshal(body, &completionRequest); err != nil {
			assert.NoError(t, err)
		}

		assert.Equal(t, float64(1), completionRequest["max_tokens"])
		assert.Equal(t, float64(1), completionRequest["max_completion_tokens"])
		assert.Equal(t, false, completionRequest["stream"])
		_, exists := completionRequest["stream_options"]
		assert.False(t, exists, "completionRequest should not have 'stream_options' key")

		if llmEngine == SGLangEngine {
			assert.Equal(t, "127.0.0.1", completionRequest["bootstrap_host"])
			assert.Equal(t, float64(8998), completionRequest["bootstrap_port"])
		}

		// Check KV transfer params only for vLLM
		kvParams, hasKV := completionRequest["kv_transfer_params"]
		if llmEngine == VLLMEngine {
			assert.True(t, hasKV, "vLLM should have kv_transfer_params")
			kvMap := kvParams.(map[string]any)
			assert.Equal(t, true, kvMap["do_remote_decode"])
			assert.Equal(t, false, kvMap["do_remote_prefill"])
		} else {
			assert.False(t, hasKV, "non-vLLM engines should not have kv_transfer_params")
		}

		// Check X-Request-Id header is set (should match the request ID from routing context)
		xRequestId := r.Header.Get("X-Request-Id")
		assert.NotEmpty(t, xRequestId, "X-Request-Id header should be set")
		// For most tests it's "test-request", but integration tests use different IDs
		assert.True(t, xRequestId == "test-request" || xRequestId == "integration-test", "X-Request-Id should be valid request ID")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if resp != "" {
			_, _ = w.Write([]byte(resp))
		} else {
			if llmEngine == VLLMEngine {
				response := map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{"content": "test response"},
					}},
					"kv_transfer_params": map[string]any{
						"do_remote_decode":  true,
						"do_remote_prefill": false,
						"remote_engine_id":  "test-engine-123",
						"remote_block_ids":  []string{"block1", "block2"},
						"remote_host":       "127.0.0.1", // please use this ip for testing.
						"remote_port":       "8080",
					},
				}
				respBytes, _ := json.Marshal(response)
				_, _ = w.Write(respBytes)
			} else {
				// For other engines, return simple success
				respBytes, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "test response"}}}})
				_, _ = w.Write(respBytes)
			}
		}
	}))

	_ = ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	return ts
}
