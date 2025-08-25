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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLeastBusyTime(t *testing.T) {
	tests := []struct {
		name       string
		readyPods  []*v1.Pod
		podMetrics map[string]map[string]metrics.MetricValue
		expectErr  bool
		expectMsgs []string
	}{
		{
			name: "successful routing with least busy time",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p3", "3.3.3.3", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p4", "4.4.4.4", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.1}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
				"p3": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.8}},
				"p4": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.9}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name:       "no ready pods",
			readyPods:  []*v1.Pod{},
			podMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:  true,
		},
		{
			name: "multiple pods with same least busy time",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.2}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.7}},
				"p3": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.2}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "3.3.3.3:8000"},
		},
		{
			name: "one pod has no metrics - should select pod with metrics",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "all pods have no metrics - should use random fallback",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "2.2.2.2:8000"},
		},
		{
			name: "zero busy time ratio",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.0}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.3}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "high busy time ratios",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.95}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.99}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "NaN busy time values",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: math.NaN()}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
			},
			expectErr:  false,
			expectMsgs: []string{"2.2.2.2:8000"},
		},
		{
			name: "all pods with identical busy time",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p3", "3.3.3.3", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
				"p2": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
				"p3": {metrics.GPUBusyTimeRatio: &metrics.SimpleMetricValue{Value: 0.5}},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "2.2.2.2:8000", "3.3.3.3:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.NewWithPodsMetricsForTest(
				tt.readyPods,
				"m1",
				tt.podMetrics)
			podList := podsFromCache(c)

			leastBusyTimeRouter := leastBusyTimeRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := leastBusyTimeRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}

// newPod creates a new Pod object for testing purposes.
// It constructs a Pod with basic configuration including name, IP address, ready status, and labels.
// returns a pointer to the created Pod object
func newPod(name, ip string, ready bool, labels map[string]string) *v1.Pod {
	condition := v1.ConditionFalse
	if ready {
		condition = v1.ConditionTrue
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: v1.PodStatus{
			PodIP: ip,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: condition,
				},
			},
		},
	}
}
