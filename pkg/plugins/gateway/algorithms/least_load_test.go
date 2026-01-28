/*
Copyright 2025 The Aibrix Team.

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

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// mockLoadProvider implements cache.LoadProvider interface for testing
type mockLoadProvider struct {
	utilizations                   map[string]float64
	consumptions                   map[string]float64
	perPodUtilizationError         map[string]error // per-pod utilization errors
	perPodglobalConsumptionErroror map[string]error // per-pod consumption errors
	globalUtilizationError         error            // global error testing
	globalConsumptionError         error            // global error testing
}

func (m *mockLoadProvider) GetUtilization(_ *types.RoutingContext, pod *v1.Pod) (float64, error) {
	// Check per-pod error
	if err, exists := m.perPodUtilizationError[pod.Name]; exists {
		return 0, err
	}
	// Check global error
	if m.globalUtilizationError != nil {
		return 0, m.globalUtilizationError
	}
	return m.utilizations[pod.Name], nil
}

func (m *mockLoadProvider) GetConsumption(_ *types.RoutingContext, pod *v1.Pod) (float64, error) {
	// Check per-pod error
	if err, exists := m.perPodglobalConsumptionErroror[pod.Name]; exists {
		return 0, err
	}
	// Check global error
	if m.globalConsumptionError != nil {
		return 0, m.globalConsumptionError
	}
	return m.consumptions[pod.Name], nil
}

// mockCappedLoadProvider implements cache.CappedLoadProvider interface for testing
type mockCappedLoadProvider struct {
	mockLoadProvider
	cap float64
}

func (m *mockCappedLoadProvider) Cap() float64 {
	return m.cap
}

type mockPodList struct {
	pods    []*v1.Pod
	indexes map[string][]*v1.Pod
}

func (m *mockPodList) All() []*v1.Pod {
	return m.pods
}

func (m *mockPodList) Len() int {
	return len(m.pods)
}

func (m *mockPodList) Indexes() []string {
	keys := make([]string, 0, len(m.indexes))
	for k := range m.indexes {
		keys = append(keys, k)
	}
	return keys
}

func (m *mockPodList) ListByIndex(index string) []*v1.Pod {
	if pods, ok := m.indexes[index]; ok {
		return pods
	}
	return nil
}

func (m *mockPodList) ListPortsForPod() map[string][]int {
	return nil
}

func newMockPodList(pods []*v1.Pod, indexes map[string][]*v1.Pod) *mockPodList {
	if indexes == nil {
		indexes = make(map[string][]*v1.Pod)
	}
	return &mockPodList{
		pods:    pods,
		indexes: indexes,
	}
}

func TestLeastLoadRouter_PushMode(t *testing.T) {
	tests := []struct {
		name                   string
		pods                   []*v1.Pod
		indexes                map[string][]*v1.Pod
		utilizations           map[string]float64
		globalUtilizationError error
		perPodUtilizationError map[string]error
		expectAddr             string
		expectErr              bool
	}{
		{
			name: "successful routing to least loaded pod",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod3", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.1,
				"pod3": 0.5,
			},
			expectAddr: "2.2.2.2:8000",
			expectErr:  false,
		},
		{
			name: "equal load distribution",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.5,
				"pod2": 0.5,
			},
			expectAddr: "2.2.2.2:8000",
			expectErr:  false,
		},
		{
			name:      "no pods available",
			pods:      []*v1.Pod{},
			expectErr: true,
		},
		{
			name: "pod without IP",
			pods: []*v1.Pod{
				newPod("pod1", "", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.5,
			},
			expectAddr: "2.2.2.2:8000",
			expectErr:  false,
		},
		{
			name: "utilization error",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			globalUtilizationError: cache.ErrorLoadCapacityReached,
			expectErr:              true,
		},
		{
			name: "some pods with utilization error",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod3", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod2": 0.7,
				"pod3": 0.3,
			},
			perPodUtilizationError: map[string]error{
				"pod1": cache.ErrorLoadCapacityReached,
			},
			expectAddr: "3.3.3.3:8000", // Should select pod3 (lowest util among healthy pods)
			expectErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &mockLoadProvider{
				utilizations:           tt.utilizations,
				perPodUtilizationError: tt.perPodUtilizationError,
				globalUtilizationError: tt.globalUtilizationError,
			}

			router, err := NewLeastLoadRouter(provider)
			assert.NoError(t, err)

			ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
			podList := newMockPodList(tt.pods, tt.indexes)

			addr, err := router.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectAddr, addr)
			}
		})
	}
}

func TestLeastLoadRouter_PullMode(t *testing.T) {
	tests := []struct {
		name                   string
		pods                   []*v1.Pod
		indexes                map[string][]*v1.Pod
		utilizations           map[string]float64
		consumptions           map[string]float64
		cap                    float64
		globalUtilizationError error
		globalConsumptionError error
		expectAddr             string
		expectErr              bool
	}{
		{
			name: "successful routing within capacity",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.1,
			},
			consumptions: map[string]float64{
				"pod1": 0.2,
				"pod2": 0.1,
			},
			cap:        1.0,
			expectAddr: "2.2.2.2",
			expectErr:  false,
		},
		{
			name: "all pods over capacity",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.8,
				"pod2": 0.9,
			},
			consumptions: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.2,
			},
			cap:       1.0,
			expectErr: true,
		},
		{
			name: "some pods within capacity",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod3", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.8,
				"pod2": 0.4,
				"pod3": 0.9,
			},
			consumptions: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.1,
				"pod3": 0.2,
			},
			cap:        1.0,
			expectAddr: "2.2.2.2",
			expectErr:  false,
		},
		{
			name: "consumption error",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.5,
			},
			consumptions:           map[string]float64{},
			globalConsumptionError: cache.ErrorSLOFailureRequest,
			cap:                    1.0,
			expectErr:              true,
		},
		{
			name: "equal load with different consumptions",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.3,
				"pod2": 0.3,
			},
			consumptions: map[string]float64{
				"pod1": 0.2,
				"pod2": 0.1,
			},
			cap:        1.0,
			expectAddr: "2.2.2.2", // Should choose pod with lower consumption
			expectErr:  false,
		},
		{
			name: "at capacity limit",
			pods: []*v1.Pod{
				newPod("pod1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			utilizations: map[string]float64{
				"pod1": 0.8,
			},
			consumptions: map[string]float64{
				"pod1": 0.2,
			},
			cap:        1.0,
			expectAddr: "1.1.1.1",
			expectErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &mockCappedLoadProvider{
				mockLoadProvider: mockLoadProvider{
					utilizations:           tt.utilizations,
					consumptions:           tt.consumptions,
					globalUtilizationError: tt.globalUtilizationError,
					globalConsumptionError: tt.globalConsumptionError,
				},
				cap: tt.cap,
			}

			router, err := NewLeastLoadPullingRouter(provider)
			assert.NoError(t, err)

			ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
			podList := newMockPodList(tt.pods, tt.indexes)

			addr, err := router.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, addr, tt.expectAddr)
			}
		})
	}
}
