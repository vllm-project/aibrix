package routingalgorithms

import (
	"context"
	"testing"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	return []string{"default"}
}

func (m *MockPodList) ListByIndex(index string) []*v1.Pod {
	return m.pods
}

func createTestPodForRoute(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
		},
		Status: v1.PodStatus{
			PodIP: ip,
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
}

func createTestRoutingContext(model, message, requestID string) *types.RoutingContext {
	ctx := context.Background()
	return types.NewRoutingContext(ctx, RouterPrefixCachePreble, model, message, requestID, "")
}

func Test_prefixCacheAndLoadRouter_Route(t *testing.T) {
	tests := []struct {
		name           string
		setupRouter    func() *prefixCacheAndLoadRouter
		setupContext   func() *types.RoutingContext
		setupPodList   func() types.PodList
		expectedError  bool
		expectedPodIP  string
		validateResult func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext)
	}{
		{
			name: "successful routing with no prefix cache",
			setupRouter: func() *prefixCacheAndLoadRouter {
				return &prefixCacheAndLoadRouter{
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
			},
			setupContext: func() *types.RoutingContext {
				return createTestRoutingContext("test-model", "Hello world", "req-1")
			},
			setupPodList: func() types.PodList {
				pods := []*v1.Pod{
					createTestPodForRoute("pod-1", "10.0.0.1"),
					createTestPodForRoute("pod-2", "10.0.0.2"),
				}
				return &MockPodList{pods: pods}
			},
			expectedError: false,
			expectedPodIP: "10.0.0.1", // Should select first pod based on cost model
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext) {
				if ctx.TargetPod() == nil {
					t.Error("Expected target pod to be set")
				}
				if router.numPods != 2 {
					t.Errorf("Expected numPods to be 2, got %d", router.numPods)
				}
			},
		},
		{
			name: "no pods available",
			setupRouter: func() *prefixCacheAndLoadRouter {
				return &prefixCacheAndLoadRouter{
					cache: prefixcacheindexer.NewLPRadixCache(0),
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
			},
			setupContext: func() *types.RoutingContext {
				return createTestRoutingContext("test-model", "Hello world", "req-3")
			},
			setupPodList: func() types.PodList {
				return &MockPodList{pods: []*v1.Pod{}}
			},
			expectedError:  true,
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext) {},
		},
		{
			name: "multiple pods with cost-based selection",
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

				// Set different costs for pods
				router.histogram.currentDecodeLengthsPerPod["pod-1"] = 100
				router.histogram.currentDecodeLengthsPerPod["pod-2"] = 50 // Lower cost
				router.histogram.currentDecodeLengthsPerPod["pod-3"] = 200

				return router
			},
			setupContext: func() *types.RoutingContext {
				return createTestRoutingContext("test-model", "Hello world", "req-4")
			},
			setupPodList: func() types.PodList {
				pods := []*v1.Pod{
					createTestPodForRoute("pod-1", "10.0.0.1"),
					createTestPodForRoute("pod-2", "10.0.0.2"),
					createTestPodForRoute("pod-3", "10.0.0.3"),
				}
				return &MockPodList{pods: pods}
			},
			expectedError: false,
			expectedPodIP: "10.0.0.1", // Will select first pod when all costs are equal
			validateResult: func(t *testing.T, router *prefixCacheAndLoadRouter, ctx *types.RoutingContext) {
				if ctx.TargetPod() == nil {
					t.Error("Expected target pod to be set")
				}
				// When all pods have equal cost (0), algorithm selects the first pod
				if ctx.TargetPod().Name != "pod-1" {
					t.Errorf("Expected pod-1 to be selected when all costs are equal, got %s", ctx.TargetPod().Name)
				}
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

			if tt.expectedPodIP != "" && ctx.TargetPod() != nil {
				if ctx.TargetPod().Status.PodIP != tt.expectedPodIP {
					t.Errorf("Expected pod IP %s, got %s", tt.expectedPodIP, ctx.TargetPod().Status.PodIP)
				}
			}

			if tt.validateResult != nil {
				tt.validateResult(t, router, ctx)
			}
		})
	}
}
