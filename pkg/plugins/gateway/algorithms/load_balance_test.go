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

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeLBPod(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestLoadBalanceRoute_SelectsLowestPendingTime(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
		makeLBPod("p3", "3.3.3.3"),
	}
	// p1: 2 req / 1 drain = 2.0
	// p2: 4 req / 2 drain = 2.0
	// p3: 1 req / 1 drain = 1.0 — should win
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 4},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
		},
		"p3": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 1},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "3.3.3.3:8000", target)
}

func TestLoadBalanceRoute_FallsBackToUniformCapacityWhenNoDrainRate(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
		makeLBPod("p3", "3.3.3.3"),
	}
	// No drain rate metrics — capacity defaults to 1.0, so score = request_count
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 5}},
		"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
		"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 8}},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target)
}

func TestLoadBalanceRoute_NoPods(t *testing.T) {
	c := cache.NewWithPodsMetricsForTest([]*v1.Pod{}, "m1", nil)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	_, err := r.Route(ctx, podsFromCache(c))
	assert.Error(t, err)
}

func TestLoadBalanceRoute_TiesBrokenRandomly(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	// Both have identical score: 2 req / 2 drain = 1.0
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
		target, err := r.Route(ctx, podsFromCache(c))
		assert.NoError(t, err)
		seen[target] = true
	}
	assert.Contains(t, seen, "1.1.1.1:8000", "p1 should be selected at least once")
	assert.Contains(t, seen, "2.2.2.2:8000", "p2 should be selected at least once")
}

func TestLoadBalanceRoute_ZeroDrainRateFallsBackToUniform(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	// Drain rate of 0 should be treated as unavailable and fall back to capacity 1.0
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 3},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 0},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 1},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 0},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target)
}

func TestLoadBalanceRoute_NoMetricsTreatedAsZeroRequests(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	// p1 has metrics, p2 has none — p2 defaults to 0 req count → lowest pending time
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 5}},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target)
}

func TestLoadBalanceRoute_HeterogeneousGPUs(t *testing.T) {
	// Simulate a fast GPU (p1) and a slow GPU (p2).
	// p1 drains at 4 req/min with 8 in-flight → score 2.0
	// p2 drains at 1 req/min with 3 in-flight → score 3.0
	// p1 should win despite having more requests because it drains faster.
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 8},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 4},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 3},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "1.1.1.1:8000", target, "faster GPU should win even with more in-flight requests")
}

func TestLoadBalanceScoreAll_ReturnsScoreForEachPod(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 4},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 6},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 3},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	scores, scored, err := r.ScoreAll(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Len(t, scores, 2)
	assert.Len(t, scored, 2)
	for _, s := range scored {
		assert.True(t, s)
	}
	// Both pods have score 2.0 (4/2 and 6/3)
	for _, s := range scores {
		assert.InDelta(t, 2.0, s, 0.0001)
	}
}

func TestLoadBalancePolarity(t *testing.T) {
	r := &loadBalanceRouter{}
	assert.Equal(t, types.PolarityLeast, r.Polarity())
}
