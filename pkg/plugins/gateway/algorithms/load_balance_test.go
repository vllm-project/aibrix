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
	"math"
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

func TestLoadBalanceRoute_SelectsLowestEffectiveLoad(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
		makeLBPod("p3", "3.3.3.3"),
	}
	// Capacity is relative tokens/sec.
	// p1: 2 running / 1 = 2.0
	// p2: 4 running / 2 = 2.0
	// p3: 1 running / 1 = 1.0 — should win
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 4},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
		},
		"p3": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 1},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "3.3.3.3:8000", target)
}

func TestLoadBalanceRoute_FallsBackToUniformCapacityWhenNoTokenRate(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
		makeLBPod("p3", "3.3.3.3"),
	}
	// No token-rate metrics — capacity defaults to 1.0, so score = running requests
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
	// Both have identical score: 2 running / 2 tokens-per-sec = 1.0
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
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
	// Identical score (same running, capacity and GPU KV usage — the score ignores CPU cache), so
	// the tie is broken by combined GPU+CPU KV-cache usage instead of randomly. p2 has less CPU
	// cache pressure.
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
			metrics.KVCacheUsagePerc:            &metrics.SimpleMetricValue{Value: 0.3},
			metrics.CPUCacheUsagePerc:           &metrics.SimpleMetricValue{Value: 0.5},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
			metrics.KVCacheUsagePerc:            &metrics.SimpleMetricValue{Value: 0.3},
			metrics.CPUCacheUsagePerc:           &metrics.SimpleMetricValue{Value: 0.1},
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

func TestLoadBalanceRoute_ZeroTokenRateFallsBackToUniform(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	// A token rate of 0 should be treated as unavailable and fall back to capacity 1.0
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 3},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 0},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 1},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 0},
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
	// p1 has metrics, p2 has none — p2 defaults to 0 running requests → lowest score
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
	// p1 generates 4 (relative) tokens/s with 8 in-flight → score 2.0
	// p2 generates 1 (relative) tokens/s with 3 in-flight → score 3.0
	// p1 should win despite having more requests because it drains faster.
	pods := []*v1.Pod{
		makeLBPod("p1", "1.1.1.1"),
		makeLBPod("p2", "2.2.2.2"),
	}
	podMetrics := map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 8},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 4},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 3},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
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
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 4},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 2},
		},
		"p2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 6},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 3},
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
// pods by raw running-request count *before* scoring runs. The busy pod is given a token rate so
// high that, absent the gate, it would win on score despite having far more in-flight requests —
// proving the gate actually excludes it rather than the score coincidentally avoiding it.
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
	// AND 20-2=18 >= 8. Without the gate, busy's huge token rate (20/1000=0.02) would beat
	// the light pods' score (2/1=2.0) on pure ScoreAll.
	podMetrics := map[string]map[string]metrics.MetricValue{
		"light-1": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
		"light-2": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
		"light-3": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
		"busy": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 20},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1000},
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
	// 20 <= 2*(11+1)=24 and the gate never fires; busy's token rate would then
	// win on score (20/1000=0.02 vs idle 2/1=2.0).
	podMetrics := map[string]map[string]metrics.MetricValue{
		"idle": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 2},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1},
		},
		"busy": {
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: 20},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: 1000},
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

// lbPodMetrics builds a pod's metric set. capacity/kvUsage <= 0 are left unset (unreported).
func lbPodMetrics(running, capacity, kvUsage float64) map[string]metrics.MetricValue {
	m := map[string]metrics.MetricValue{
		metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: running},
	}
	if capacity > 0 {
		m[metrics.RealtimeOutputTokenRateEWMA] = &metrics.SimpleMetricValue{Value: capacity}
	}
	if kvUsage > 0 {
		m[metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: kvUsage}
	}
	return m
}

// TestLoadBalanceScoreAll_HeterogeneousExample checks the documented B40/A100/H20 example
// end to end: score = running / tokens-per-sec × (1 + 2(1−kvFree)²), lowest wins.
func TestLoadBalanceScoreAll_HeterogeneousExample(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("b40", "1.1.1.1"),
		makeLBPod("a100", "2.2.2.2"),
		makeLBPod("h20", "3.3.3.3"),
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"b40":  lbPodMetrics(30, 6000, 0.50), // 30/6000 × (1+2·0.50²) = 0.0075
		"a100": lbPodMetrics(20, 4000, 0.70), // 20/4000 × (1+2·0.70²) = 0.0099
		"h20":  lbPodMetrics(25, 3000, 0.40), // 25/3000 × (1+2·0.40²) = 0.0110
	})
	r := &loadBalanceRouter{cache: c}

	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	// Pod order is not stable across podsFromCache calls, so pair scores with the same list.
	podList := podsFromCache(c)
	scores, _, err := r.ScoreAll(ctx, podList)
	assert.NoError(t, err)
	byName := map[string]float64{}
	for i, pod := range podList.All() {
		byName[pod.Name] = scores[i]
	}
	assert.InDelta(t, 0.0075, byName["b40"], 1e-6)
	assert.InDelta(t, 0.0099, byName["a100"], 1e-6)
	assert.InDelta(t, 0.0110, byName["h20"], 1e-6)

	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "1.1.1.1:8000", target)
}

func TestLoadBalanceScore(t *testing.T) {
	critical := loadBalanceKVCriticalFree
	tests := []struct {
		name     string
		load     float64
		capacity float64
		kvFree   float64
		want     float64
	}{
		{"no kv pressure", 10, 2, 1.0, 5},
		{"half kv used", 10, 2, 0.5, 5 * 1.5},
		{"quadratic in kv used", 10, 2, 0.2, 5 * (1 + 2*0.8*0.8)},
		{"idle pod scores zero", 0, 2, 0.5, 0},
		{"exactly at critical is still usable", 10, 2, critical, 5 * (1 + 2*(1-critical)*(1-critical))},
		{"below critical is excluded", 0, 2, critical - 0.01, math.Inf(1)},
		{"fully used is excluded", 10, 2, 0, math.Inf(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadBalanceScore(tt.load, tt.capacity, tt.kvFree)
			if math.IsInf(tt.want, 1) {
				assert.True(t, math.IsInf(got, 1), "got %v", got)
				return
			}
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

// KV pressure alone must be able to flip the choice when load and capacity are equal.
func TestLoadBalanceRoute_KVPressureDiscouragesFullerCache(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("p1", "1.1.1.1"), makeLBPod("p2", "2.2.2.2")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"p1": lbPodMetrics(5, 1000, 0.80),
		"p2": lbPodMetrics(5, 1000, 0.30),
	})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target)
}

// A replica past the KV guardrail is skipped even when it is the least loaded and fastest.
func TestLoadBalanceRoute_KVGuardrailExcludesCriticalPod(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("full", "1.1.1.1"), makeLBPod("ok", "2.2.2.2")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"full": lbPodMetrics(1, 9000, 0.95), // 5% free < 10% critical
		"ok":   lbPodMetrics(20, 1000, 0.40),
	})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	for i := 0; i < 10; i++ {
		target, err := r.Route(ctx, podsFromCache(c))
		assert.NoError(t, err)
		assert.Equal(t, "2.2.2.2:8000", target)
	}
}

// If every replica is past the guardrail, routing must degrade to the one with the most KV
// headroom rather than fail the request.
func TestLoadBalanceRoute_AllPodsKVCriticalStillRoutes(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("p1", "1.1.1.1"), makeLBPod("p2", "2.2.2.2")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"p1": lbPodMetrics(1, 1000, 0.97),
		"p2": lbPodMetrics(9, 1000, 0.92),
	})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target, "most KV headroom wins when every pod is critical")
}

// A replica whose engine reports no KV usage gets no penalty and no guardrail.
func TestLoadBalanceScoreAll_MissingKVMetricIsNotPenalized(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("p1", "1.1.1.1")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"p1": lbPodMetrics(6, 3, 0),
	})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	scores, _, err := r.ScoreAll(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.InDelta(t, 2.0, scores[0], 1e-9)
}

// A NaN KV usage reading must be treated like a missing one. Left as NaN it would slip past the
// guardrail (NaN < critical is false) and produce a NaN score.
func TestLoadBalanceScoreAll_NaNKVMetricIsNotPenalized(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("p1", "1.1.1.1")}
	m := lbPodMetrics(6, 3, 0)
	m[metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: math.NaN()}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{"p1": m})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")

	assert.Equal(t, 1.0, r.kvFreeFraction(ctx, pods[0]))
	scores, _, err := r.ScoreAll(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.InDelta(t, 2.0, scores[0], 1e-9)
}

// A pod with no capacity estimate yet must compete as an average replica. With a flat 1.0
// fallback it would score in the thousands against tokens/sec-scale peers and never be picked, so
// it would never get the traffic needed to be measured.
func TestLoadBalanceRoute_UnmeasuredPodGetsMeanCapacity(t *testing.T) {
	pods := []*v1.Pod{makeLBPod("measured", "1.1.1.1"), makeLBPod("fresh", "2.2.2.2")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"measured": lbPodMetrics(4, 4000, 0),
		"fresh":    lbPodMetrics(2, 0, 0),
	})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")

	podList := podsFromCache(c)
	scores, _, err := r.ScoreAll(ctx, podList)
	assert.NoError(t, err)
	byName := map[string]float64{}
	for i, pod := range podList.All() {
		byName[pod.Name] = scores[i]
	}
	assert.InDelta(t, 4.0/4000, byName["measured"], 1e-9)
	assert.InDelta(t, 2.0/4000, byName["fresh"], 1e-9, "fresh pod is scored at the mean measured capacity")

	target, err := r.Route(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.Equal(t, "2.2.2.2:8000", target)
}

func TestLoadBalanceScoreAll_QueuedRequestsWeighted(t *testing.T) {
	orig := loadBalanceQueuedWeight
	loadBalanceQueuedWeight = 0.5
	t.Cleanup(func() { loadBalanceQueuedWeight = orig })

	pods := []*v1.Pod{makeLBPod("p1", "1.1.1.1")}
	pm := lbPodMetrics(4, 2, 0)
	pm[metrics.NumRequestsWaiting] = &metrics.SimpleMetricValue{Value: 6}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{"p1": pm})
	r := &loadBalanceRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	scores, _, err := r.ScoreAll(ctx, podsFromCache(c))
	assert.NoError(t, err)
	assert.InDelta(t, (4+0.5*6)/2.0, scores[0], 1e-9)
}
