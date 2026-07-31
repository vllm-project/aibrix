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

func TestLeastKvCache(t *testing.T) {
	tests := []struct {
		name       string
		readyPods  []*v1.Pod
		podMetrics map[string]map[string]metrics.MetricValue
		expectErr  bool
		expectMsgs []string
	}{
		{
			name: "successful routing with least kv cache",
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
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.2},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.3},
				},
				"p2": {
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.1},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.5},
				},
				"p3": {
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.6},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.6},
				},
				"p4": {
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.6},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.8},
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
			name: "multiple pods with same least kv caches",
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
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.2},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.3},
				},
				"p2": {
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.5},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.5},
				},
				"p3": {
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.3},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.2},
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
					metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.2},
					metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.3},
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

			leastKvCacheRouter := leastKvCacheRouter{
				cache: c,
			}

			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "message", "request", "user")
			targetPod, err := leastKvCacheRouter.Route(ctx, podList)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Contains(t, tt.expectMsgs, targetPod)
			}
		})
	}
}

func TestLeastKvCache_ScoreAll(t *testing.T) {
	podA := newPod("pA", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pB", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podC := newPod("pC", "3.3.3.3", true, map[string]string{"model.aibrix.ai/port": "8000"})

	c := cache.NewWithPodsModelMetricsForTest(
		[]*v1.Pod{podA, podB, podC},
		"m1",
		map[string]map[string]metrics.MetricValue{
			"pA": {
				metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.1},
				metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1},
			},
			"pB": {
				metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.5},
				metrics.CPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.0},
			},
		})

	r := leastKvCacheRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "", "req", "")

	podList := podsFromCache(c)
	scores, scored, err := r.ScoreAll(ctx, podList)
	assert.NoError(t, err)
	assert.Equal(t, 3, len(scores))

	pods := podList.All()
	// Create a map to verify results independently of slice ordering
	podScores := make(map[string]float64)
	podScored := make(map[string]bool)
	for i, p := range pods {
		podScores[p.Name] = scores[i]
		podScored[p.Name] = scored[i]
	}

	assert.True(t, podScored["pA"])
	assert.InDelta(t, 0.2, podScores["pA"], 0.001)

	assert.True(t, podScored["pB"])
	assert.InDelta(t, 0.5, podScores["pB"], 0.001)

	assert.False(t, podScored["pC"])

	// Check polarity
	assert.Equal(t, types.PolarityLeast, r.Polarity())
}
