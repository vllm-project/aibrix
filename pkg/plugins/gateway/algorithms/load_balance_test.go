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
	"github.com/vllm-project/aibrix/pkg/utils"
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

func TestLoadBalanceRoute_TiesBrokenByLeastKvCache(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	// Both have identical pending_time score: 2 req / 2 drain = 1.0, so the tie is broken
	// by combined GPU+CPU KV-cache usage instead of randomly. p2 has less cache pressure.
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
			metrics.KVCacheUsagePerc:                   &metrics.SimpleMetricValue{Value: 0.8},
			metrics.CPUCacheUsagePerc:                  &metrics.SimpleMetricValue{Value: 0.1},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 2},
			metrics.KVCacheUsagePerc:                   &metrics.SimpleMetricValue{Value: 0.1},
			metrics.CPUCacheUsagePerc:                  &metrics.SimpleMetricValue{Value: 0.1},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	for i := 0; i < 20; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
		target, err := r.Route(ctx, podsFromCache(c))
		assert.NoError(t, err)
		assert.Equal(t, "2.2.2.2:8000", target, "p2 should always win the tie via lower KV-cache usage")
	}
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

// TestLoadBalanceRoute_LoadImbalanceGateRestrictsCandidates verifies the load-imbalance gate
// (moved here from the prefix-cache routers) narrows the candidate set to the least-loaded
// pods by raw running-request count *before* pending-time scoring runs. The busy pod is given
// a drain rate so high that, absent the gate, it would win on pending_time despite having far
// more in-flight requests — proving the gate actually excludes it rather than pending_time
// coincidentally avoiding it.
//
// The gate itself is applied by the gateway centrally (see gateway.go's selectTargetPod), not
// by Route() anymore, so this test applies it explicitly first to mirror that call site.
func TestLoadBalanceRoute_LoadImbalanceGateRestrictsCandidates(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("light-1", "1.1.1.1"),
		makeLBPod("light-2", "2.2.2.2"),
		makeLBPod("light-3", "3.3.3.3"),
		makeLBPod("busy", "4.4.4.4"),
	}
	// meanOfOthers (excluding busy) = (2+2+2)/3 = 2.0; gate fires since 20 > 2.0*(2.0+1)=6.0
	// AND 20-2=18 >= 8. Without the gate, busy's huge drain rate (20/1000=0.02) would beat
	// the light pods' pending_time (2/1=2.0) on pure ScoreAll.
	podMetrics := map[string]map[string]metrics.MetricValue{
		"light-1": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
		"light-2": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
		"light-3": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
		"busy": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 20},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1000},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	gated := ApplyLoadImbalanceGate(ctx, c, pods)
	target, err := r.Route(ctx, &utils.PodArray{Pods: gated})
	assert.NoError(t, err)
	assert.NotEqual(t, "4.4.4.4:8000", target, "gate should exclude the busy pod even though it scores lowest")
	assert.Contains(t, []string{"1.1.1.1:8000", "2.2.2.2:8000", "3.3.3.3:8000"}, target)
}

// Two-replica clusters skip the relative factor check (it never holds for n=2 with
// factor=2) and fire on the absolute gap alone, so a hot pod still sheds to the idle replica.
func TestLoadBalanceRoute_TwoPodLoadImbalanceGateRestrictsToIdle(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("idle", "1.1.1.1"),
		makeLBPod("busy", "2.2.2.2"),
	}
	// gap=18 >= minGap=8. Without the n=2 special case the factor check is
	// 20 <= 2*(11+1)=24 and the gate never fires; busy's drain rate would then
	// win on pending_time (20/1000=0.02 vs idle 2/1=2.0).
	podMetrics := map[string]map[string]metrics.MetricValue{
		"idle": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1},
		},
		"busy": {
			metrics.RealtimeNumRequestsRunning:         &metrics.SimpleMetricValue{Value: 20},
			metrics.RealtimeRunningRequestsDrainRate1m: &metrics.SimpleMetricValue{Value: 1000},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	gated := ApplyLoadImbalanceGate(ctx, c, pods)
	target, err := r.Route(ctx, &utils.PodArray{Pods: gated})
	assert.NoError(t, err)
	assert.Equal(t, "1.1.1.1:8000", target, "gate should restrict to the idle replica")
}
