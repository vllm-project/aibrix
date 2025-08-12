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
			serverResp:  "success",
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

			ctx := types.NewRoutingContext(context.Background(), "test", "model", "message", "request", "user")
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

		w.WriteHeader(code)
		if resp != "" {
			_, _ = w.Write([]byte(resp))
		}
	}))

	_ = ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	return ts
}
