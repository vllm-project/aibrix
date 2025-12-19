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
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
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
		cache:                 cache.NewForTest(),
		tokenizer:             tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
		httpClient:            &http.Client{},
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
		expectPrefill string
		expectDecode  string
		expectError   bool
		errorContains string
	}{
		{
			name: "basic successful filtering",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-test",
			expectDecode:  "decode-test",
			expectError:   false,
		},
		{
			name: "pods without roleset-name label are ignored",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "no-roleset", Labels: map[string]string{"role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-test",
			expectDecode:  "decode-test",
			expectError:   false,
		},
		{
			name: "pods without role-name label are ignored",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "no-role", Labels: map[string]string{"roleset-name": "test"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-test",
			expectDecode:  "decode-test",
			expectError:   false,
		},
		{
			name: "pods with empty labels are ignored",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "no-labels"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-test",
			expectDecode:  "decode-test",
			expectError:   false,
		},
		{
			name: "pods with unknown roles are ignored",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "unknown-role", Labels: map[string]string{"roleset-name": "test", "role-name": "unknown"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-test",
			expectDecode:  "decode-test",
			expectError:   false,
		},
		{
			name: "multi-node setup - only pods with PodGroupIndex=0 are selected",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-node0", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-node1", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-node0", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-node1", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "1"}}},
			},
			expectPrefill: "prefill-node0",
			expectDecode:  "decode-node0",
			expectError:   false,
		},
		{
			name: "backward compatibility - pods without PodGroupIndex are included",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-legacy", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-legacy", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-legacy",
			expectDecode:  "decode-legacy",
			expectError:   false,
		},
		{
			name: "mixed setup - legacy pods and multi-node pods",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-legacy", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-node0", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-legacy", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-node0", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
			},
			expectPrefill: "prefill-legacy", // First valid pod found
			expectDecode:  "decode-legacy",  // First valid pod found
			expectError:   false,
		},
		{
			name: "error - no prefill pods",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-test", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=0, decode=1",
		},
		{
			name: "error - no decode pods",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-test", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
			},
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=1, decode=0",
		},
		{
			name: "error - no valid pods at all",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "invalid1", Labels: map[string]string{"role-name": "other"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "invalid2", Labels: map[string]string{"roleset-name": "test"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "invalid3"}},
			},
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=0, decode=0",
		},
		{
			name: "error - only multi-node pods with PodGroupIndex=1 (no HTTP server)",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-node1", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-node1", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "1"}}},
			},
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=0, decode=0",
		},
		{
			name: "multiple pods of same role - first valid one is selected",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-1", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-2", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-1", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-2", Labels: map[string]string{"roleset-name": "test", "role-name": "decode"}}},
			},
			expectPrefill: "prefill-1",
			expectDecode:  "decode-1",
			expectError:   false,
		},
		{
			name: "edge case - PodGroupIndex with invalid values",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-invalid", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "invalid"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
			},
			expectPrefill: "prefill-valid",
			expectDecode:  "decode-valid",
			expectError:   false,
		},
		{
			name: "edge case - empty PodGroupIndex value",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-empty", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": ""}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
			},
			expectPrefill: "prefill-valid",
			expectDecode:  "decode-valid",
			expectError:   false,
		},
		{
			name: "edge case - negative PodGroupIndex",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-negative", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "-1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "prefill-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "prefill", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "decode-valid", Labels: map[string]string{"roleset-name": "test", "role-name": "decode", "stormservice.orchestration.aibrix.ai/pod-group-index": "0"}}},
			},
			expectPrefill: "prefill-valid",
			expectDecode:  "decode-valid",
			expectError:   false,
		},
		{
			name:          "empty pod list",
			pods:          []*v1.Pod{},
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=0, decode=0",
		},
		{
			name:          "nil pod list",
			pods:          nil,
			expectError:   true,
			errorContains: "prefill or decode pods are not ready: prefill=0, decode=0",
		},
	}

	r := pdRouter{
		cache:                 cache.NewForTest(),
		tokenizer:             tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "test", "model", "message", "test-request", "user")
			prefill, decode, err := r.filterPrefillDecodePods(ctx, tt.pods)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
				assert.Nil(t, prefill)
				assert.Nil(t, decode)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, prefill)
				assert.NotNil(t, decode)
				assert.Equal(t, tt.expectPrefill, prefill.Name)
				assert.Equal(t, tt.expectDecode, decode.Name)
			}
		})
	}
}

func TestScorePrefillPods(t *testing.T) {
	tests := []struct {
		name         string
		pods         []*v1.Pod
		message      string
		expectScores int // number of scores expected
	}{
		{
			name: "basic scoring with pods",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{PDRoleSetIdentifier: "roleset2"}}},
			},
			message:      "test message",
			expectScores: 2,
		},
		{
			name: "multiple pods same roleset",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: map[string]string{PDRoleSetIdentifier: "roleset2"}}},
			},
			message:      "test message",
			expectScores: 2, // Should have 2 rolesets
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create router with real dependencies
			r := &pdRouter{
				tokenizer:             tokenizer.NewCharacterTokenizer(),
				prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
				prefillRequestTracker: NewPrefillRequestTracker(),
			}

			// Create routing context
			ctx := &types.RoutingContext{
				Message: tt.message,
			}

			// Call the function
			scores, maxScore, prefixHashes := r.scorePrefillPods(ctx, tt.pods)

			// Verify basic functionality
			assert.Equal(t, tt.expectScores, len(scores), "number of scores should match")
			assert.GreaterOrEqual(t, maxScore, 0.0, "max score should be non-negative")
			assert.NotNil(t, prefixHashes, "prefix hashes should not be nil")
		})
	}
}

func TestScoreDecodePods(t *testing.T) {
	tests := []struct {
		name         string
		pods         []*v1.Pod
		expectScores int // number of scores expected
	}{
		{
			name: "basic decode scoring",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{PDRoleSetIdentifier: "roleset2"}}},
			},
			expectScores: 2,
		},
		{
			name: "multiple pods same roleset",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: map[string]string{PDRoleSetIdentifier: "roleset1"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: map[string]string{PDRoleSetIdentifier: "roleset2"}}},
			},
			expectScores: 2, // Should have 2 rolesets
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create router with real dependencies
			r := &pdRouter{}

			// Create routing context
			ctx := &types.RoutingContext{
				RequestID: "test-request",
			}

			// Call the function with minimal parameters
			scores, maxScore := r.scoreDecodePods(
				ctx,
				tt.pods,
				10.0,                 // maxRequestCount
				100.0,                // maxThroughput
				80.0,                 // maxFreeGPUUsage
				map[string]float64{}, // podRequestCounts
				map[string]float64{}, // podThroughputs
				map[string]float64{}, // podFreeGpuUsage
			)

			// Verify basic functionality
			assert.Equal(t, tt.expectScores, len(scores), "number of scores should match")
			assert.GreaterOrEqual(t, maxScore, 0.0, "max score should be non-negative")
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
			Context: context.Background(),
		}
	}

	createRouter := func(pods []*v1.Pod, metricsMap map[string]map[string]metrics.MetricValue) *pdRouter {
		tokenizerObj, err := tokenizer.NewTokenizer("character", nil)
		if err != nil {
			t.Fatal(err)
		}
		c := cache.NewWithPodsMetricsForTest(pods, "m1", metricsMap)
		return &pdRouter{
			prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
			cache:                 c,
			tokenizer:             tokenizerObj,
			prefillRequestTracker: NewPrefillRequestTracker(),
			httpClient:            &http.Client{},
		}
	}

	tests := []struct {
		name             string
		serverCode       int
		serverResp       string
		llmEngine        string
		expectError      bool
		errorMsg         string
		podMetrics       map[string]map[string]metrics.MetricValue
		expectedPodNames []string
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
		{
			name:        "async vllm prefill request, with imbalance load request",
			serverCode:  http.StatusOK,
			llmEngine:   "vllm",
			expectError: false,
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 6}},
				"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 12}},
			},
			expectedPodNames: []string{"p1"},
		},
		{
			name:        "async sglang prefill request, with imbalance load request",
			serverCode:  http.StatusOK,
			llmEngine:   "sglang",
			expectError: false,
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
			},
			expectedPodNames: []string{"p2"},
		},
		{
			name:        "async vllm prefill request, with no imbalance load request",
			serverCode:  http.StatusOK,
			llmEngine:   "vllm",
			expectError: false,
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 9}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 8}},
				"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 12}},
			},
			expectedPodNames: []string{"p1", "p2", "p3", "p4"},
		},
		{
			name:        "async sglang prefill request, with no imbalance load request",
			serverCode:  http.StatusOK,
			llmEngine:   "sglang",
			expectError: false,
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 6}},
				"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
				"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 4}},
				"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
			},
			expectedPodNames: []string{"p1", "p2", "p3", "p4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupTestServer(t, tt.serverCode, tt.serverResp, tt.llmEngine)
			defer ts.Close()

			prefillPods := []*v1.Pod{
				createPrefillPod("p1", tt.llmEngine),
				createPrefillPod("p2", tt.llmEngine),
				createPrefillPod("p3", tt.llmEngine),
				createPrefillPod("p4", tt.llmEngine),
			}

			routingCtx := createRoutingCtx()
			router := createRouter(prefillPods, tt.podMetrics)

			err := router.doPrefillRequest(routingCtx, prefillPods[0], tt.llmEngine)
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
				Context: context.Background(),
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
				Context:   context.Background(),
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
		Context:    context.Background(),
	}

	router := &pdRouter{
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		cache:                 cache.NewWithPodsForTest(prefillPods, "test-model"),
		tokenizer:             tokenizer.NewCharacterTokenizer(),
		prefillRequestTracker: NewPrefillRequestTracker(),
		httpClient:            &http.Client{},
	}

	err := router.doPrefillRequest(routingCtx, prefillPods[0], VLLMEngine)
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
				Context:   context.Background(),
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

func TestLoadImbalanceSelectPrefillPod(t *testing.T) {
	tests := []struct {
		name              string
		readyPods         []*v1.Pod
		podRequestCount   map[string]int32
		expectImbalance   bool
		expectTargetPod   string
		expectTargetInSet []string // For cases where multiple pods have same min count
	}{
		{
			name: "no imbalance - equal request counts",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 5,
				"pod2": 5,
				"pod3": 5,
			},
			expectImbalance: false,
			expectTargetPod: "",
		},
		{
			name: "no imbalance - difference within threshold",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 10,
				"pod2": 15,
				"pod3": 20,
			},
			expectImbalance: false,
			expectTargetPod: "",
		},
		{
			name: "imbalance detected - difference exceeds threshold",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 5,
				"pod2": 40,
				"pod3": 45,
			},
			expectImbalance: true,
			expectTargetPod: "pod1",
		},
		{
			name: "imbalance with multiple pods at minimum",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod4"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 2,
				"pod2": 2,
				"pod3": 50,
				"pod4": 45,
			},
			expectImbalance:   true,
			expectTargetInSet: []string{"pod1", "pod2"},
		},
		{
			name: "empty pod request count",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			},
			podRequestCount: map[string]int32{},
			expectImbalance: false,
			expectTargetPod: "",
		},
		{
			name: "single pod",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 10,
			},
			expectImbalance: false,
			expectTargetPod: "",
		},
		{
			name: "zero requests vs high requests",
			readyPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
			},
			podRequestCount: map[string]int32{
				"pod1": 0,
				"pod2": 50,
			},
			expectImbalance: true,
			expectTargetPod: "pod1",
		},
	}

	r := &pdRouter{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetPod, imbalance := r.loadImbalanceSelectPrefillPod(tt.readyPods, tt.podRequestCount)

			assert.Equal(t, tt.expectImbalance, imbalance, "imbalance detection should match expected")

			if tt.expectImbalance {
				assert.NotNil(t, targetPod, "target pod should not be nil when imbalance is detected")
				if tt.expectTargetPod != "" {
					assert.Equal(t, tt.expectTargetPod, targetPod.Name, "target pod should match expected")
				} else if len(tt.expectTargetInSet) > 0 {
					assert.Contains(t, tt.expectTargetInSet, targetPod.Name, "target pod should be one of the expected pods")
				}
			} else {
				assert.Nil(t, targetPod, "target pod should be nil when no imbalance is detected")
			}
		})
	}
}

func TestLoadImbalanceSelectDecodePod(t *testing.T) {
	tests := []struct {
		name                   string
		pods                   []*v1.Pod
		metricsMap             map[string]map[string]metrics.MetricValue
		expectTargetPod        string
		expectTargetInSet      []string
		expectMaxRequestCount  float64
		expectMaxThroughput    float64
		expectMaxFreeGPUUsage  float64
		expectPodRequestCounts map[string]float64
		expectPodThroughputs   map[string]float64
		expectPodFreeGpuUsage  map[string]float64
	}{
		{
			name: "no imbalance - balanced load",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port":   "8000",
					constants.ModelLabelName: "test-model",
				}),
				newPod("pod2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port":   "8000",
					constants.ModelLabelName: "test-model",
				}),
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				"pod1": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 5},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 100},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.5},
				},
				"pod2": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 7},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 120},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.6},
				},
			},
			expectTargetPod:        "",
			expectMaxRequestCount:  7,
			expectMaxThroughput:    120,
			expectMaxFreeGPUUsage:  50,
			expectPodRequestCounts: map[string]float64{"pod1": 5, "pod2": 7},
			expectPodThroughputs:   map[string]float64{"pod1": 100, "pod2": 120},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 50, "pod2": 40},
		},
		{
			name: "request count imbalance - select pod with minimum requests",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				"pod1": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 2},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 100},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.3},
				},
				"pod2": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 40},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 120},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.8},
				},
				"pod3": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 35},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 110},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.7},
				},
			},
			expectTargetPod:        "pod1",
			expectMaxRequestCount:  40,
			expectMaxThroughput:    120,
			expectMaxFreeGPUUsage:  70,
			expectPodRequestCounts: map[string]float64{"pod1": 2, "pod2": 40, "pod3": 35},
			expectPodThroughputs:   map[string]float64{"pod1": 100, "pod2": 120, "pod3": 110},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 70, "pod2": 20, "pod3": 30},
		},
		{
			name: "throughput imbalance - select pod with minimum throughput",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				"pod1": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 10},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 50},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.4},
				},
				"pod2": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 12},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 3000},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.5},
				},
			},
			expectTargetPod:        "pod1",
			expectMaxRequestCount:  12,
			expectMaxThroughput:    3000,
			expectMaxFreeGPUUsage:  60,
			expectPodRequestCounts: map[string]float64{"pod1": 10, "pod2": 12},
			expectPodThroughputs:   map[string]float64{"pod1": 50, "pod2": 3000},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 60, "pod2": 50},
		},
		{
			name: "zero requests - select pod with zero requests",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				"pod1": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 0},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 100},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.2},
				},
				"pod2": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 5},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 120},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.3},
				},
			},
			expectTargetPod:        "pod1",
			expectMaxRequestCount:  5,
			expectMaxThroughput:    120,
			expectMaxFreeGPUUsage:  80,
			expectPodRequestCounts: map[string]float64{"pod1": 0, "pod2": 5},
			expectPodThroughputs:   map[string]float64{"pod1": 100, "pod2": 120},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 80, "pod2": 70},
		},
		{
			name: "metrics error handling - default values",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				// Empty metrics map to trigger errors
			},
			expectTargetPod:        "pod1",
			expectMaxRequestCount:  1,
			expectMaxThroughput:    1,
			expectMaxFreeGPUUsage:  100,
			expectPodRequestCounts: map[string]float64{"pod1": 0},
			expectPodThroughputs:   map[string]float64{"pod1": 0},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 100},
		},
		{
			name: "high GPU usage - free GPU calculation",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}}},
			},
			metricsMap: map[string]map[string]metrics.MetricValue{
				"pod1": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 5},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 100},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 0.95},
				},
				"pod2": {
					metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 7},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 120},
					metrics.GPUCacheUsagePerc:               &metrics.SimpleMetricValue{Value: 1.0},
				},
			},
			expectTargetPod:        "",
			expectMaxRequestCount:  7,
			expectMaxThroughput:    120,
			expectMaxFreeGPUUsage:  5,
			expectPodRequestCounts: map[string]float64{"pod1": 5, "pod2": 7},
			expectPodThroughputs:   map[string]float64{"pod1": 100, "pod2": 120},
			expectPodFreeGpuUsage:  map[string]float64{"pod1": 5, "pod2": 0.1}, // Minimum 0.1 when <= 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create cache with test data
			cache := cache.NewWithPodsMetricsForTest(tt.pods, "test-model", tt.metricsMap)

			r := &pdRouter{
				cache: cache,
			}

			ctx := &types.RoutingContext{
				RequestID: "test-request",
				Model:     "test-model",
			}

			targetPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage := r.loadImbalanceSelectDecodePod(ctx, tt.pods)

			// Check target pod selection
			if tt.expectTargetPod != "" {
				assert.NotNil(t, targetPod, "target pod should not be nil")
				assert.Equal(t, tt.expectTargetPod, targetPod.Name, "target pod should match expected")
			} else if len(tt.expectTargetInSet) > 0 {
				assert.NotNil(t, targetPod, "target pod should not be nil")
				assert.Contains(t, tt.expectTargetInSet, targetPod.Name, "target pod should be one of the expected pods")
			} else {
				assert.Nil(t, targetPod, "target pod should be nil when no imbalance is detected")
			}

			// Check returned metrics
			assert.Equal(t, tt.expectMaxRequestCount, maxRequestCount, "max request count should match")
			assert.Equal(t, tt.expectMaxThroughput, maxThroughput, "max throughput should match")
			assert.Equal(t, tt.expectMaxFreeGPUUsage, maxFreeGPUUsage, "max free GPU usage should match")

			// Check pod metrics maps
			assert.Equal(t, tt.expectPodRequestCounts, podRequestCounts, "pod request counts should match")
			assert.Equal(t, tt.expectPodThroughputs, podThroughputs, "pod throughputs should match")
			assert.Equal(t, tt.expectPodFreeGpuUsage, podFreeGpuUsage, "pod free GPU usage should match")
		})
	}
}

func TestIsPodSuitableForPromptLength(t *testing.T) {
	tests := []struct {
		name         string
		podLabels    map[string]string
		promptLength int
		expected     bool
	}{
		{
			name: "no prompt length range configured",
			podLabels: map[string]string{
				"roleset-name": "test",
				"role-name":    "prefill",
			},
			promptLength: 1000,
			expected:     true,
		},
		{
			name: "prompt length exactly at min",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "1000",
				"prompt-max-length": "2000",
			},
			promptLength: 1000,
			expected:     true,
		},
		{
			name: "prompt length exactly at max",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "1000",
				"prompt-max-length": "2000",
			},
			promptLength: 2000,
			expected:     true,
		},
		{
			name: "prompt length in middle of range",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "1000",
				"prompt-max-length": "2000",
			},
			promptLength: 1500,
			expected:     true,
		},
		{
			name: "prompt length below min",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "1000",
				"prompt-max-length": "2000",
			},
			promptLength: 900,
			expected:     false,
		},
		{
			name: "prompt length above max",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "1000",
				"prompt-max-length": "2000",
			},
			promptLength: 2100,
			expected:     false,
		},
		{
			name: "prompt length min larger than max",
			podLabels: map[string]string{
				"roleset-name":      "test",
				"role-name":         "prefill",
				"prompt-min-length": "2000",
				"prompt-max-length": "1000",
			},
			promptLength: 1000,
			expected:     false,
		},
	}

	// Create a router instance
	router := &pdRouter{
		cache:                 cache.NewForTest(),
		tokenizer:             tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
		httpClient:            &http.Client{},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test pod
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-pod",
					Labels: tt.podLabels,
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionTrue},
					},
				},
			}

			// Call the function being tested
			result := router.isPodSuitableForPromptLength(pod, tt.promptLength)

			// Verify the result
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetPromptLength(t *testing.T) {
	router := &pdRouter{
		cache:                 cache.NewForTest(),
		tokenizer:             tokenizer.NewCharacterTokenizer(),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: NewPrefillRequestTracker(),
		httpClient:            &http.Client{},
	}

	tests := []struct {
		name        string
		requestBody string
		expected    int
	}{
		{
			name:        "empty messages array",
			requestBody: `{"messages": []}`,
			expected:    0,
		},
		{
			name:        "single message in messages",
			requestBody: `{"messages": [{"role": "user", "content": "Hello World"}]}`,
			expected:    len(func() []int { tokens, _ := utils.TokenizeInputText("Hello World"); return tokens }()),
		},
		{
			name:        "multiple messages in messages",
			requestBody: `{"messages": [{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "Hello World"}]}`,
			expected: len(func() []int {
				tokens, _ := utils.TokenizeInputText("You are a helpful assistant.Hello World")
				return tokens
			}()),
		},
		{
			name:        "prompt field",
			requestBody: `{"prompt": "Hello World"}`,
			expected:    len(func() []int { tokens, _ := utils.TokenizeInputText("Hello World"); return tokens }()),
		},
		{
			name:        "input field",
			requestBody: `{"input": "Hello World"}`,
			expected:    len(func() []int { tokens, _ := utils.TokenizeInputText("Hello World"); return tokens }()),
		},
		{
			name:        "invalid JSON",
			requestBody: `invalid json`,
			expected:    0,
		},
		{
			name:        "no text fields",
			requestBody: `{"model": "test-model"}`,
			expected:    0,
		},
		{
			name:        "messages with non-string content",
			requestBody: `{"messages": [{"role": "user", "content": {"text": "Hello"}}]}`,
			expected:    0,
		},
		{
			name:        "Chinese text",
			requestBody: `{"prompt": "你好世界"}`,
			expected:    len(func() []int { tokens, _ := utils.TokenizeInputText("你好世界"); return tokens }()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routingCtx := &types.RoutingContext{
				RequestID: "test-request",
				ReqBody:   []byte(tt.requestBody),
			}

			result := router.getPromptLength(routingCtx)

			assert.Equal(t, tt.expected, result)
		})
	}
}
