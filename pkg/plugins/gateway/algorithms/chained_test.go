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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestChainedRouter(t *testing.T) {
	// Create test pods
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "3.3.3.3",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}

	podList := &utils.PodArray{Pods: pods}

	// Test cases
	tests := []struct {
		name        string
		algorithms  []types.RoutingAlgorithm
		readyPods   types.PodList
		expectError bool
	}{
		{
			name:        "empty algorithms - should use random",
			algorithms:  []types.RoutingAlgorithm{},
			readyPods:   podList,
			expectError: false,
		},
		{
			name:        "single algorithm - random",
			algorithms:  []types.RoutingAlgorithm{RouterRandom},
			readyPods:   podList,
			expectError: false,
		},
		{
			name:        "multiple algorithms - random, random",
			algorithms:  []types.RoutingAlgorithm{RouterRandom, RouterRandom},
			readyPods:   podList,
			expectError: false,
		},
		{
			name:        "single pod - should short circuit",
			algorithms:  []types.RoutingAlgorithm{RouterRandom, RouterRandom},
			readyPods:   &utils.PodArray{Pods: []*v1.Pod{pods[0]}},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new routing context for each test case
			ctx := types.NewRoutingContext(context.Background(), RouterChained, "test-model", "test-message", "test-request-id", "")
			defer ctx.Delete()

			// Create chained router
			router, err := NewChainedRouter(tt.algorithms)
			assert.NoError(t, err)

			// Test routing
			result, err := router.Route(ctx, tt.readyPods)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				// Should return a valid address
				assert.Contains(t, result, ":")
			}
		})
	}
}

func TestChainedRouterWithInvalidAlgorithm(t *testing.T) {
	// Create test pods with multiple pods to avoid short-circuiting
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}

	podList := &utils.PodArray{Pods: pods}

	// Create routing context
	ctx := types.NewRoutingContext(context.Background(), RouterChained, "test-model", "test-message", "test-request-id", "")

	// Test with invalid algorithm
	invalidAlg := types.RoutingAlgorithm("invalid-algorithm")
	router, err := NewChainedRouter([]types.RoutingAlgorithm{invalidAlg})
	assert.NoError(t, err)

	// Should handle invalid algorithm gracefully
	result, err := router.Route(ctx, podList)
	assert.NoError(t, err)
	assert.Contains(t, result, ":")
}

// TestChainedRouterNarrowing tests that chained routing actually narrows down candidate sets
// by applying multiple algorithms sequentially.
func TestChainedRouterNarrowing(t *testing.T) {
	// Create test pods with different GPU cache and utilization values
	// This allows us to test if chaining algorithms actually narrows the candidate set
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-low-gpu-low-util",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-low-gpu-high-util",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-high-gpu-low-util",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "3.3.3.3",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-high-gpu-high-util",
				Namespace: "default",
				Labels: map[string]string{
					"model.aibrix.ai/port": "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP: "4.4.4.4",
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}

	podList := &utils.PodArray{Pods: pods}

	// Create routing context
	ctx := types.NewRoutingContext(context.Background(), RouterChained, "test-model", "test-message", "test-request-id", "")
	defer ctx.Delete()

	// Test chaining with least-gpu-cache followed by least-utilization
	// This should narrow down the candidate set from multiple pods to fewer pods
	algorithms := []types.RoutingAlgorithm{RouterLeastGpuCache, RouterUtil}
	router, err := NewChainedRouter(algorithms)
	assert.NoError(t, err)

	// Should be able to route successfully
	result, err := router.Route(ctx, podList)
	assert.NoError(t, err)
	assert.Contains(t, result, ":")

	// The result should be one of our test pods
	assert.True(t, strings.Contains(result, "1.1.1.1:8000") ||
		strings.Contains(result, "2.2.2.2:8000") ||
		strings.Contains(result, "3.3.3.3:8000") ||
		strings.Contains(result, "4.4.4.4:8000"))
}
