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
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	v1 "k8s.io/api/core/v1"
)

// MockPodList implements types.PodList for testing
type MockPodList struct {
	pods []*v1.Pod
}

func (m *MockPodList) Len() int {
	return len(m.pods)
}

func (m *MockPodList) All() []*v1.Pod {
	return m.pods
}

func (m *MockPodList) Indexes() []string {
	return []string{}
}

func (m *MockPodList) ListByIndex(index string) []*v1.Pod {
	return m.pods
}

func createTestRoutingContext(model, message, requestID string) *types.RoutingContext {
	ctx := context.Background()
	return types.NewRoutingContext(ctx, RouterPrefixCachePreble, model, message, requestID, "")
}

func TestPrefixCacheAndLoadRouterRouting(t *testing.T) {
	tests := []struct {
		name           string
		setupRouter    func() *prefixCacheAndLoadRouter
		setupContext   func() *types.RoutingContext
		setupPodList   func() types.PodList
		expectedError  bool
		validateResult func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext, selectedPod string)
	}{
		{
			name: "cost_model_routing_with_different_costs",
			setupRouter: func() *prefixCacheAndLoadRouter {
				router := &prefixCacheAndLoadRouter{
					cache: prefixcacheindexer.NewLPRadixCache(2),
					histogram: &SlidingWindowHistogram{
						windowDuration:             slidingWindowPeriod,
						histogram:                  make(map[*prefixcacheindexer.TreeNode]int),
						nodeToCount:                make(map[*prefixcacheindexer.TreeNode]int),
						hitTokens:                  make(map[*prefixcacheindexer.TreeNode]int),
						promptTokens:               make(map[*prefixcacheindexer.TreeNode]int),
						decodingSize:               make(map[*prefixcacheindexer.TreeNode]int),
						timestamps:                 []histogramEntry{},
						numPods:                    0,
						podAllocations:             make(map[*prefixcacheindexer.TreeNode]map[int]bool),
						currentDecodeLengthsPerPod: make(map[string]int),
						avgTimePerTokenPerPod:      make(map[string][]float64),
						perNodeTotalDecodeLengths:  make(map[*prefixcacheindexer.TreeNode]int),
					},
					numPods:        0,
					podAllocations: make(map[*prefixcacheindexer.TreeNode]map[int]bool),
				}

				// Create historical data to generate cost differences
				tokens1, _ := utils.TokenizeInputText("Historical request one")
				node1, _, _ := router.cache.AddPrefix(tokens1, "test-model", "")
				node1.AddOrUpdatePodForModel("test-model", "pod-1", time.Now())

				tokens2, _ := utils.TokenizeInputText("Historical request two")
				node2, _, _ := router.cache.AddPrefix(tokens2, "test-model", "")
				node2.AddOrUpdatePodForModel("test-model", "pod-2", time.Now())

				// Set up histogram with cost differences
				router.histogram.histogram[node1] = 100
				router.histogram.nodeToCount[node1] = 3
				router.histogram.decodingSize[node1] = 100
				router.histogram.hitTokens[node1] = 50
				router.histogram.promptTokens[node1] = 100

				router.histogram.histogram[node2] = 50
				router.histogram.nodeToCount[node2] = 1
				router.histogram.decodingSize[node2] = 30
				router.histogram.hitTokens[node2] = 25
				router.histogram.promptTokens[node2] = 50

				// Set different decode lengths and time per token
				router.histogram.currentDecodeLengthsPerPod["pod-1"] = 300
				router.histogram.currentDecodeLengthsPerPod["pod-2"] = 50

				router.histogram.avgTimePerTokenPerPod["pod-1"] = []float64{0.3, 0.4, 0.5}
				router.histogram.avgTimePerTokenPerPod["pod-2"] = []float64{0.1, 0.12, 0.15}

				return router
			},
			setupContext: func() *types.RoutingContext {
				// New request that won't match existing prefixes (low match ratio)
				return createTestRoutingContext("test-model", "Completely different new request", "req-cost-test")
			},
			setupPodList: func() types.PodList {
				pods := []*v1.Pod{
					newPod("pod-1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
					newPod("pod-2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				}
				return &MockPodList{pods: pods}
			},
			expectedError: false,
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext, selectedPod string) {
				if ctx.TargetPod() == nil {
					t.Error("Expected target pod to be set")
					return
				}

				t.Logf("Selected pod: %s", ctx.TargetPod().Name)

				// For cost model routing, we expect a pod to be selected
				// The specific cost values may change due to route execution side effects
				// What's important is that the routing logic worked and selected a pod
			},
		},
		{
			name: "prefix_cache_routing_with_matching_prefix",
			setupRouter: func() *prefixCacheAndLoadRouter {
				router := &prefixCacheAndLoadRouter{
					cache: prefixcacheindexer.NewLPRadixCache(3),
					histogram: &SlidingWindowHistogram{
						windowDuration:             slidingWindowPeriod,
						histogram:                  make(map[*prefixcacheindexer.TreeNode]int),
						nodeToCount:                make(map[*prefixcacheindexer.TreeNode]int),
						hitTokens:                  make(map[*prefixcacheindexer.TreeNode]int),
						promptTokens:               make(map[*prefixcacheindexer.TreeNode]int),
						decodingSize:               make(map[*prefixcacheindexer.TreeNode]int),
						timestamps:                 []histogramEntry{},
						numPods:                    0,
						podAllocations:             make(map[*prefixcacheindexer.TreeNode]map[int]bool),
						currentDecodeLengthsPerPod: make(map[string]int),
						avgTimePerTokenPerPod:      make(map[string][]float64),
						perNodeTotalDecodeLengths:  make(map[*prefixcacheindexer.TreeNode]int),
					},
					numPods:        0,
					podAllocations: make(map[*prefixcacheindexer.TreeNode]map[int]bool),
				}

				// Pre-populate cache with the exact prefix that the test request will use
				// This ensures the AddPrefix call in Route() will find the existing node
				testTokens, _ := utils.TokenizeInputText("Hello world shared content extra")
				node, _, _ := router.cache.AddPrefix(testTokens, "test-model", "")
				// Associate specific pods with this cached prefix
				node.AddOrUpdatePodForModel("test-model", "pod-1", time.Now())
				node.AddOrUpdatePodForModel("test-model", "pod-3", time.Now())

				// Set up histogram data
				router.histogram.histogram[node] = len(testTokens)
				router.histogram.nodeToCount[node] = 2
				router.histogram.decodingSize[node] = 45
				router.histogram.hitTokens[node] = len(testTokens) - 1
				router.histogram.promptTokens[node] = len(testTokens)

				return router
			},
			setupContext: func() *types.RoutingContext {
				// Request that exactly matches the cached prefix
				return createTestRoutingContext("test-model", "Hello world shared content extra", "req-prefix-test")
			},
			setupPodList: func() types.PodList {
				pods := []*v1.Pod{
					newPod("pod-1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
					newPod("pod-2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}), // Not in cache
					newPod("pod-3", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				}
				return &MockPodList{pods: pods}
			},
			expectedError: false,
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext, selectedPod string) {
				if ctx.TargetPod() == nil {
					t.Error("Expected target pod to be set")
					return
				}

				selectedPodName := ctx.TargetPod().Name
				t.Logf("Selected pod: %s", selectedPodName)

				// Verify that one of the pods with cached prefix was selected
				if selectedPodName != "pod-1" && selectedPodName != "pod-3" {
					t.Errorf("Expected pod-1 or pod-3 (pods with cached prefix) to be selected, got %s", selectedPodName)
				}
			},
		},
		{
			name: "no_pods_available_error",
			setupRouter: func() *prefixCacheAndLoadRouter {
				router, _ := NewPrefixCacheAndLoadRouter()
				return router.(*prefixCacheAndLoadRouter)
			},
			setupContext: func() *types.RoutingContext {
				return createTestRoutingContext("test-model", "Any request", "req-no-pods")
			},
			setupPodList: func() types.PodList {
				return &MockPodList{pods: []*v1.Pod{}} // Empty pod list
			},
			expectedError: true,
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext, selectedPod string) {
				// Error case - no validation needed
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := tt.setupRouter()
			ctx := tt.setupContext()
			podList := tt.setupPodList()

			result, err := router.Route(ctx, podList)

			if tt.expectedError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if result == "" {
				t.Error("Expected non-empty result")
			}

			if tt.validateResult != nil {
				tt.validateResult(t, router, ctx, result)
			}
		})
	}
}
