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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

func TestThroughput(t *testing.T) {
	tests := []struct {
		name       string
		readyPods  []*v1.Pod
		podMetrics map[string]map[string]metrics.MetricValue
		expectErr  bool
		expectMsgs []string
	}{
		{
			name: "successful routing with least throughput",
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
				"p1": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 1},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 2},
				},
				"p2": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 2},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 1},
				},
				"p3": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 3},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 3},
				},
				"p4": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 4},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 4},
				},
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
			name: "multiple pods with same least throughputs",
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
				"p1": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 1},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 4},
				},
				"p2": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 5},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 5},
				},
				"p3": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 2},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 1},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000", "3.3.3.3:8000"},
		},
		{
			name: "one pod has no metrics",
			readyPods: []*v1.Pod{
				newPod("p1", "1.1.1.1", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
				newPod("p2", "2.2.2.2", true, map[string]string{
					"model.aibrix.ai/port": "8000",
				}),
			},
			podMetrics: map[string]map[string]metrics.MetricValue{
				"p1": {
					metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 1},
					metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 2},
				},
			},
			expectErr:  false,
			expectMsgs: []string{"1.1.1.1:8000"},
		},
		{
			name: "all pods have no metrics",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cache.NewWithPodsModelMetricsForTest(
				tt.readyPods,
				"m1",
				tt.podMetrics)
			podList := podsFromCache(c)

			throughputRouter := throughputRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := throughputRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}
