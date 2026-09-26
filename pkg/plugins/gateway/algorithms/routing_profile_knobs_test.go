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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// routingOverridesContext builds a routing context the way the gateway does: a
// context that carries the model config profile under test, with its
// routingConfig already parsed (configprofiles.ParseRoutingConfig).
// ResolveRoutingOverrides reads only the profile's typed routing config, so the
// algorithm string does not matter here.
func routingOverridesContext(t *testing.T, routingConfig string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterNotSet, "model", "message", "req-routing-overrides", "user")
	t.Cleanup(ctx.Delete)
	if routingConfig != "" {
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RoutingConfig: json.RawMessage(routingConfig),
			Routing:       configprofiles.ParseRoutingConfig(json.RawMessage(routingConfig)),
		}
	}
	return ctx
}

// probeRoutingDefaults is the process default table the resolver tests run
// against, so "kept the process default" is asserted against a known value
// instead of whatever the environment of the test run configures.
func probeRoutingDefaults() *types.RoutingOverrides {
	return &types.RoutingOverrides{
		LoadBalance: types.LoadBalanceOverrides{
			ImbalanceFactor: 2,
			ImbalanceMinGap: 8,
			QueuedWeight:    0,
			KVPressureAlpha: 2,
			KVCriticalFree:  0.1,
		},
		PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 2},
		Preble:      types.PrebleOverrides{TargetGPU: "V100", DecodingLength: 45},
		VTC:         types.VTCOverrides{MaxPodLoad: 100, FairnessWeight: 1, UtilizationWeight: 1},
		AutoBlend: types.AutoBlendOverrides{
			LoadBalanceWeight:            1,
			LeastRequestWeight:           1,
			PrefixCacheWeight:            5,
			PrefixCacheLoadBalanceWeight: 4,
		},
		PD: types.PDOverrides{
			Abort: types.PDAbortOverrides{Timeout: 3 * time.Second, RetryDelay: 2 * time.Second},
			Spreads: types.PDSpreadOverrides{
				PrefillLoadImbalanceMinSpread:      16,
				DecodeLoadImbalanceMinSpread:       16,
				DecodeThroughputImbalanceMinSpread: 2048,
				DecodeScoreRatioThreshold:          1.5,
			},
			DecodeLB:              types.PDDecodeLBOverrides{WeightRunning: 1, WeightThroughput: 1},
			TokenLoad:             types.PDTokenLoadOverrides{KVWeight: 0.3, RequestCost: 3500, TTL: time.Minute, SessionTTL: 30 * time.Minute},
			HybridCacheLoadFactor: 0.5,
			MinMatchPct:           0,
			BucketServe:           false,
			BucketServeMode:       string(pd.BucketModeRPS),
			PrefillRequestTimeout: 30 * time.Second,
		},
	}
}

// withDefaultOverrides swaps the process default table for one test and
// restores the environment-derived one afterwards.
func withDefaultOverrides(t *testing.T, defaults *types.RoutingOverrides) {
	t.Helper()
	restore := types.DefaultRoutingOverrides()
	types.SetDefaultRoutingOverrides(defaults)
	t.Cleanup(func() { types.SetDefaultRoutingOverrides(restore) })
}

func TestResolveRoutingOverridesFromRoutingConfig(t *testing.T) {
	withDefaultOverrides(t, probeRoutingDefaults())

	tests := []struct {
		name          string
		routingConfig string
		wantNil       bool
		check         func(t *testing.T, ov *types.RoutingOverrides)
	}{
		{
			name:          "no routingConfig parks nothing",
			routingConfig: "",
			wantNil:       true,
		},
		{
			name:          "routingConfig without routing sections parks nothing",
			routingConfig: `{"promptTokensGte":100,"prefillScorePolicy":"prefix_cache"}`,
			wantNil:       true,
		},
		{
			name:          "unparsable routingConfig parks nothing",
			routingConfig: `{"loadBalance":`,
			wantNil:       true,
		},
		{
			name:          "load-balance gate and score",
			routingConfig: `{"loadBalance":{"imbalanceFactor":3.5,"imbalanceMinGap":12,"queuedWeight":0,"kvPressureAlpha":0,"kvCriticalFree":0.05}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				lb := ov.LoadBalance
				assert.Equal(t, 3.5, lb.ImbalanceFactor)
				assert.Equal(t, 12, lb.ImbalanceMinGap)
				assert.Equal(t, 0.0, lb.QueuedWeight, "an explicit zero is an override, not an unset value")
				assert.Equal(t, 0.0, lb.KVPressureAlpha, "zero drops the penalty")
				assert.Equal(t, 0.05, lb.KVCriticalFree)
			},
		},
		{
			name:          "prefix-cache, preble, vtc and auto-blend",
			routingConfig: `{"prefixCache":{"standardDeviationFactor":3},"preble":{"targetGPU":"A6000","decodingLength":64},"vtc":{"maxPodLoad":200,"fairnessWeight":0,"utilizationWeight":2.5},"autoBlend":{"loadBalanceWeight":2,"leastRequestWeight":3,"prefixCacheWeight":7,"prefixCacheLoadBalanceWeight":2}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Equal(t, 3, ov.PrefixCache.StandardDeviationFactor)
				assert.Equal(t, "A6000", ov.Preble.TargetGPU)
				assert.Equal(t, 64, ov.Preble.DecodingLength)
				assert.Equal(t, 200.0, ov.VTC.MaxPodLoad)
				assert.Equal(t, 0.0, ov.VTC.FairnessWeight, "zero drops the fairness term")
				assert.Equal(t, 2.5, ov.VTC.UtilizationWeight)
				assert.Equal(t, 2, ov.AutoBlend.LoadBalanceWeight)
				assert.Equal(t, 3, ov.AutoBlend.LeastRequestWeight)
				assert.Equal(t, 7, ov.AutoBlend.PrefixCacheWeight)
				assert.Equal(t, 2, ov.AutoBlend.PrefixCacheLoadBalanceWeight)
			},
		},
		{
			name:          "unset sections keep the process defaults",
			routingConfig: `{"prefixCache":{"standardDeviationFactor":9}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Equal(t, 9, ov.PrefixCache.StandardDeviationFactor)
				want := probeRoutingDefaults()
				assert.Equal(t, want.LoadBalance, ov.LoadBalance)
				assert.Equal(t, want.AutoBlend, ov.AutoBlend)
				assert.Equal(t, want.PD, ov.PD)
			},
		},
		{
			name:          "scoped token tracker knobs and token weights",
			routingConfig: `{"vtc":{"inputTokenWeight":3,"outputTokenWeight":4,"tokenTrackerWindowSize":7,"tokenTrackerTimeUnit":"seconds","tokenTrackerMinTokens":250,"tokenTrackerMaxTokens":9000}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Equal(t, 3.0, ov.VTC.InputTokenWeight)
				assert.Equal(t, 4.0, ov.VTC.OutputTokenWeight)
				assert.Equal(t, 7, ov.VTC.TokenTracker.WindowSize)
				assert.Equal(t, "seconds", ov.VTC.TokenTracker.TimeUnit)
				assert.Equal(t, 250.0, ov.VTC.TokenTracker.MinTokens)
				assert.Equal(t, 9000.0, ov.VTC.TokenTracker.MaxTokens)
			},
		},
		{
			name:          "prompt-length bucketing",
			routingConfig: `{"promptLengthBucketing":true}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.True(t, ov.PD.PromptLengthBucketing)
			},
		},
		{
			name: "PD knobs",
			routingConfig: `{"pd":{"decodeAbortTimeout":0,"decodeAbortRetryDelay":0,"prefillRequestTimeout":90,
				"prefillLoadImbalanceMinSpread":4,"decodeLoadImbalanceMinSpread":8.5,"decodeThroughputImbalanceMinSpread":1024,
				"decodeScoreRatioThreshold":2.5,"decodeLBWeightRunning":3,"decodeLBWeightThroughput":4,
				"hybridCacheLoadFactor":0.25,"minMatchPct":30,"tokenLoadKVWeight":0.7,"tokenLoadRequestCost":100,
				"tokenLoadTTLSeconds":0,"tokenLoadSessionTTLSeconds":42}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Equal(t, time.Duration(0), ov.PD.Abort.Timeout, "an explicit zero disables decode aborts")
				assert.Equal(t, time.Duration(0), ov.PD.Abort.RetryDelay, "an explicit zero sends a single abort attempt")
				assert.Equal(t, 90*time.Second, ov.PD.PrefillRequestTimeout)
				assert.Equal(t, int32(4), ov.PD.Spreads.PrefillLoadImbalanceMinSpread)
				assert.Equal(t, 8.5, ov.PD.Spreads.DecodeLoadImbalanceMinSpread)
				assert.Equal(t, 1024.0, ov.PD.Spreads.DecodeThroughputImbalanceMinSpread)
				assert.Equal(t, 2.5, ov.PD.Spreads.DecodeScoreRatioThreshold)
				assert.Equal(t, 3.0, ov.PD.DecodeLB.WeightRunning)
				assert.Equal(t, 4.0, ov.PD.DecodeLB.WeightThroughput)
				assert.Equal(t, 0.25, ov.PD.HybridCacheLoadFactor)
				assert.Equal(t, 30.0, ov.PD.MinMatchPct)
				assert.Equal(t, 0.7, ov.PD.TokenLoad.KVWeight)
				assert.Equal(t, 100.0, ov.PD.TokenLoad.RequestCost)
				assert.Equal(t, time.Duration(0), ov.PD.TokenLoad.TTL, "an explicit zero disables the charge sweep")
				assert.Equal(t, 42*time.Second, ov.PD.TokenLoad.SessionTTL)
			},
		},
		{
			name: "values the environment would reject keep the process default",
			routingConfig: `{"loadBalance":{"imbalanceFactor":-1,"imbalanceMinGap":0,"queuedWeight":-0.5,"kvPressureAlpha":-1,"kvCriticalFree":1.5},
				"prefixCache":{"standardDeviationFactor":0},
				"preble":{"targetGPU":"H100","decodingLength":0},
				"vtc":{"maxPodLoad":0,"fairnessWeight":-1,"utilizationWeight":-1,
					"inputTokenWeight":0,"outputTokenWeight":-1,"tokenTrackerWindowSize":0,"tokenTrackerTimeUnit":"fortnights",
					"tokenTrackerMinTokens":-1,"tokenTrackerMaxTokens":0},
				"autoBlend":{"loadBalanceWeight":-1,"leastRequestWeight":-1,"prefixCacheWeight":0,"prefixCacheLoadBalanceWeight":-1},
				"pd":{"decodeAbortTimeout":-1,"decodeAbortRetryDelay":-1,"prefillRequestTimeout":0,"prefillLoadImbalanceMinSpread":0,
					"decodeLoadImbalanceMinSpread":-1,"decodeThroughputImbalanceMinSpread":0,"decodeScoreRatioThreshold":-1,
					"decodeLBWeightRunning":0,"decodeLBWeightThroughput":-1,"hybridCacheLoadFactor":1.5,"minMatchPct":101,
					"tokenLoadKVWeight":0,"tokenLoadRequestCost":-1,"tokenLoadTTLSeconds":-1,"tokenLoadSessionTTLSeconds":-1}}`,
			wantNil: true,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Nil(t, ov, "a profile whose every value is rejected parks no overrides at all")
			},
		},
		{
			name: "boundary values the environment accepts",
			routingConfig: `{"loadBalance":{"imbalanceFactor":0.5,"imbalanceMinGap":1,"kvCriticalFree":1},
				"prefixCache":{"standardDeviationFactor":1},
				"autoBlend":{"loadBalanceWeight":0,"leastRequestWeight":1000000,"prefixCacheWeight":1,"prefixCacheLoadBalanceWeight":0},
				"pd":{"hybridCacheLoadFactor":0,"minMatchPct":100,"decodeLBWeightRunning":0.5}}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.Equal(t, 0.5, ov.LoadBalance.ImbalanceFactor)
				assert.Equal(t, 1, ov.LoadBalance.ImbalanceMinGap)
				assert.Equal(t, 1.0, ov.LoadBalance.KVCriticalFree, "the critical free-KV bound is inclusive")
				assert.Equal(t, 1, ov.PrefixCache.StandardDeviationFactor)
				assert.Equal(t, 0, ov.AutoBlend.LoadBalanceWeight, "0 disables the auto-blend for the request")
				assert.Equal(t, 1000000, ov.AutoBlend.LeastRequestWeight)
				assert.Equal(t, 1, ov.AutoBlend.PrefixCacheWeight)
				assert.Equal(t, 0, ov.AutoBlend.PrefixCacheLoadBalanceWeight, "0 leaves prefix-cache scoring alone")
				assert.Equal(t, 0.0, ov.PD.HybridCacheLoadFactor, "0 keeps the full token load")
				assert.Equal(t, 100.0, ov.PD.MinMatchPct)
				assert.Equal(t, 0.5, ov.PD.DecodeLB.WeightRunning)
			},
		},
		{
			name:          "bucket-serve switch and mode",
			routingConfig: `{"bucketServe":true,"bucketServeMode":"throughput"}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.True(t, ov.PD.BucketServe)
				assert.Equal(t, string(pd.BucketModeThroughput), ov.PD.BucketServeMode)
			},
		},
		{
			name:          "a bucket-serve mode the planner does not know keeps the process default",
			routingConfig: `{"bucketServe":true,"bucketServeMode":"Throughput"}`,
			check: func(t *testing.T, ov *types.RoutingOverrides) {
				assert.True(t, ov.PD.BucketServe, "the switch is a plain boolean and still lands")
				assert.Equal(t, string(pd.BucketModeRPS), ov.PD.BucketServeMode, "only the names the planner knows are accepted")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := routingOverridesContext(t, tt.routingConfig)
			ResolveRoutingOverrides(ctx)
			overrides := ctx.RoutingOverrides()
			if tt.wantNil {
				assert.Same(t, types.DefaultRoutingOverrides(), overrides, "the request reads the process defaults")
				if tt.check != nil {
					tt.check(t, resolveRoutingOverrides(ctx.ConfigProfile.Routing))
				}
				return
			}
			require.NotNil(t, overrides)
			assert.NotSame(t, types.DefaultRoutingOverrides(), overrides, "an applied value parks a request-scoped table")
			tt.check(t, overrides)
		})
	}
}

// TestVTCTokenTrackerTimeUnitOnlyTakesKnownNames checks that the time unit knob
// lands only for a name the tracker actually has, instead of being normalized
// to minutes behind the profile's back.
func TestVTCTokenTrackerTimeUnitOnlyTakesKnownNames(t *testing.T) {
	withDefaultOverrides(t, probeRoutingDefaults())

	for _, unit := range []string{"minutes", "seconds", "milliseconds"} {
		ctx := routingOverridesContext(t, `{"vtc":{"tokenTrackerTimeUnit":"`+unit+`"}}`)
		ResolveRoutingOverrides(ctx)
		overrides := ctx.RoutingOverrides()
		require.NotNil(t, overrides)
		assert.Equal(t, unit, overrides.VTC.TokenTracker.TimeUnit)
	}

	for _, unit := range []string{"Minutes", "SECONDS", "fortnights"} {
		ctx := routingOverridesContext(t, `{"vtc":{"tokenTrackerTimeUnit":"`+unit+`"}}`)
		ResolveRoutingOverrides(ctx)
		assert.Same(t, types.DefaultRoutingOverrides(), ctx.RoutingOverrides(),
			"the unit %q is not one the tracker knows, so the profile keeps the process default", unit)
	}
}

func TestResolveRoutingOverridesLogsDroppedValuesOnce(t *testing.T) {
	count := func() int {
		n := 0
		droppedKnobWarnings.Range(func(_, _ any) bool {
			n++
			return true
		})
		return n
	}
	// Start from an empty dedup table: another test in this package may already
	// have logged the same knob values, and this test counts distinct entries.
	droppedKnobWarnings.Range(func(key, _ any) bool {
		droppedKnobWarnings.Delete(key)
		return true
	})
	before := count()

	drop := configprofiles.ParseRoutingConfig(json.RawMessage(`{"loadBalance":{"imbalanceFactor":-1}}`))
	assert.Nil(t, resolveRoutingOverrides(drop))
	assert.Nil(t, resolveRoutingOverrides(drop), "the same request config resolves the same way")
	another := configprofiles.ParseRoutingConfig(json.RawMessage(`{"loadBalance":{"imbalanceFactor":-2}}`))
	assert.Nil(t, resolveRoutingOverrides(another))

	assert.Equal(t, before+2, count(), "one warning per distinct knob value, not per request")
}

func TestResolveRoutingOverridesKeepsUnsetValuesFromTheProcessDefaults(t *testing.T) {
	withDefaultOverrides(t, probeRoutingDefaults())

	// A profile that sets one load-balance knob keeps every other knob at the
	// process default, including the ones of other families.
	ctx := routingOverridesContext(t, `{"loadBalance":{"imbalanceMinGap":100}}`)
	ResolveRoutingOverrides(ctx)
	overrides := ctx.RoutingOverrides()
	require.NotNil(t, overrides)
	assert.Equal(t, 100, overrides.LoadBalance.ImbalanceMinGap)
	assert.Equal(t, probeRoutingDefaults().LoadBalance.ImbalanceFactor, overrides.LoadBalance.ImbalanceFactor)
	assert.Equal(t, probeRoutingDefaults().PrefixCache, overrides.PrefixCache)
	assert.Equal(t, probeRoutingDefaults().PD, overrides.PD)

	// A request whose profile sets nothing reads the process default table
	// itself: the strategies and gates of one request share one resolution.
	plain := routingOverridesContext(t, "")
	ResolveRoutingOverrides(plain)
	assert.Same(t, types.DefaultRoutingOverrides(), plain.RoutingOverrides())

	profiled := routingOverridesContext(t, `{"prefixCache":{"standardDeviationFactor":4}}`)
	ResolveRoutingOverrides(profiled)
	assert.Equal(t, 4, profiled.RoutingOverrides().PrefixCache.StandardDeviationFactor)
	assert.Same(t, profiled.RoutingOverrides(), profiled.RoutingOverrides(), "one resolved table per request")

	ResolveRoutingOverrides(nil) // nil routing context: must not panic
}

func TestLoadImbalanceGateHonorsProfileOverrides(t *testing.T) {
	pods := []*v1.Pod{
		makeLBPod("light-1", "1.1.1.1"),
		makeLBPod("light-2", "2.2.2.2"),
		makeLBPod("light-3", "3.3.3.3"),
		makeLBPod("busy", "4.4.4.4"),
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"light-1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
		"light-2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
		"light-3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
		"busy":    {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 20}},
	})

	plain := routingOverridesContext(t, "")
	ResolveRoutingOverrides(plain)
	assert.Len(t, ApplyLoadImbalanceGate(plain, c, pods), 3, "the process defaults exclude the busy replica")

	// meanOfOthers = (2+2+2)/3 = 2, so the default factor fires at 20 > 2*(2+1).
	// A request whose profile raises the factor keeps the busy replica a candidate.
	relaxed := routingOverridesContext(t, `{"loadBalance":{"imbalanceFactor":20}}`)
	ResolveRoutingOverrides(relaxed)
	assert.Len(t, ApplyLoadImbalanceGate(relaxed, c, pods), 4)

	wideGap := routingOverridesContext(t, `{"loadBalance":{"imbalanceMinGap":100}}`)
	ResolveRoutingOverrides(wideGap)
	assert.Len(t, ApplyLoadImbalanceGate(wideGap, c, pods), 4)

	assert.Len(t, ApplyLoadImbalanceGate(routingOverridesContext(t, ""), c, pods), 3, "another request's profile must not change this one")
}

func TestAutoBlendWeightsHonorProfileOverrides(t *testing.T) {
	withAutoBlendWeights(t, 1, 1)

	defaults := effectiveAutoBlendWeights(nil)
	assert.Equal(t, 1, defaults.loadBalance, "a request without a profile reads the process default table")
	assert.Equal(t, 1, defaults.leastRequest)
	assert.Equal(t, types.DefaultRoutingOverrides().AutoBlend.PrefixCacheWeight, defaults.prefixCache)
	assert.Equal(t, types.DefaultRoutingOverrides().AutoBlend.PrefixCacheLoadBalanceWeight, defaults.prefixCacheLoadBalance)

	profiled := routingOverridesContext(t, `{"autoBlend":{"loadBalanceWeight":2,"leastRequestWeight":3,"prefixCacheWeight":7,"prefixCacheLoadBalanceWeight":2}}`)
	ResolveRoutingOverrides(profiled)
	weights := effectiveAutoBlendWeights(profiled)
	assert.Equal(t, 2, weights.loadBalance)
	assert.Equal(t, 3, weights.leastRequest)
	assert.Equal(t, 7, weights.prefixCache)
	assert.Equal(t, 2, weights.prefixCacheLoadBalance)

	prefixCacheCfg, err := ParseMultiRouterConfig("prefix-cache")
	require.NoError(t, err)
	blended, ok := appendLoadBalanceBlend("prefix-cache", prefixCacheCfg, weights)
	assert.True(t, ok)
	assert.Equal(t, "prefix-cache:7,load-balance:2", blended, "a bare prefix-cache request blends with the profile ratio")

	otherCfg, err := ParseMultiRouterConfig("least-latency")
	require.NoError(t, err)
	blended, ok = appendLoadBalanceBlend("least-latency", otherCfg, weights)
	assert.True(t, ok)
	assert.Equal(t, "least-latency,load-balance:2,least-request:3", blended)

	// A profile weight of 0 disables the hidden blend for its own requests only.
	off := routingOverridesContext(t, `{"autoBlend":{"loadBalanceWeight":0}}`)
	ResolveRoutingOverrides(off)
	_, ok = appendLoadBalanceBlend("least-latency", otherCfg, effectiveAutoBlendWeights(off))
	assert.False(t, ok)
	_, ok = appendLoadBalanceBlend("least-latency", otherCfg, effectiveAutoBlendWeights(nil))
	assert.True(t, ok)
}

func TestPrebleDecodingLengthKnobReachesTheHistogram(t *testing.T) {
	router := newTestRouter(1024, nil)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "preble-1", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	podList := &MockPodList{pods: []*v1.Pod{pod}}

	profiled := createTestRoutingContext("model", "hello world", "req-preble-overrides")
	profiled.SetRoutingOverrides(&types.RoutingOverrides{Preble: types.PrebleOverrides{DecodingLength: 32}})
	require.NoError(t, router.PostRouteUpdate(profiled, podList, pod))
	assert.Equal(t, 32, router.histogram.currentDecodeLengthsPerPod[pod.Name])

	plain := createTestRoutingContext("model", "hello world", "req-preble-default")
	require.NoError(t, router.PostRouteUpdate(plain, podList, pod))
	assert.Equal(t, 32+decodingLength, router.histogram.currentDecodeLengthsPerPod[pod.Name], "a request without the knob keeps the process default")
}

func TestPrefixCacheSigmaKnobControlsCandidateFiltering(t *testing.T) {
	const model = "m1"

	pods := getReadyPods()
	metricsMap := map[string]map[string]metrics.MetricValue{}
	for _, pod := range pods {
		metricsMap[pod.Name] = map[string]metrics.MetricValue{
			metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0},
		}
	}
	c := cache.NewWithPodsMetricsForTest(pods, model, metricsMap)
	tokenizerObj, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)
	router := prefixCacheRouter{cache: c, tokenizer: tokenizerObj, prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable()}
	podList := podsFromCache(c)

	bump := func(podName string, times int) {
		t.Helper()
		pod, found := utils.FilterPodByName(podName, pods)
		require.True(t, found)
		for i := 0; i < times; i++ {
			ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, model, "", fmt.Sprintf("%s-bump-%d", podName, i), "")
			ctx.SetTargetPod(pod)
			c.AddRequestCount(ctx, ctx.RequestID, model)
			ctx.Delete()
		}
	}

	// Three loaded replicas make the idle fourth the deterministic fallback: this
	// route records the prompt prefix on it.
	bump("p1", 1)
	bump("p2", 1)
	bump("p3", 1)
	prime := types.NewRoutingContext(context.Background(), RouterPrefixCache, model, "abcdefgh", "req-prime", "")
	p4Addr, err := router.Route(prime, podList)
	require.NoError(t, err)
	require.Equal(t, "4.4.4.4:8000", p4Addr)

	// Counts are now [1,1,1,10]: p4 holds the prefix but sits about 2.5 deviations
	// above the pool mean, so the sigma of the request decides its candidacy.
	bump("p4", 10)

	wide := types.NewRoutingContext(context.Background(), RouterPrefixCache, model, "abcdefgh", "req-wide", "")
	wide.ConfigProfile = &types.ResolvedConfigProfile{Routing: configprofiles.ParseRoutingConfig(json.RawMessage(`{"prefixCache":{"standardDeviationFactor":4}}`))}
	ResolveRoutingOverrides(wide)
	chosen, err := router.Route(wide, podList)
	require.NoError(t, err)
	assert.Equal(t, p4Addr, chosen, "a generous sigma keeps the cache-warm replica a candidate")

	sharp := types.NewRoutingContext(context.Background(), RouterPrefixCache, model, "abcdefgh", "req-sharp", "")
	sharp.ConfigProfile = &types.ResolvedConfigProfile{Routing: configprofiles.ParseRoutingConfig(json.RawMessage(`{"prefixCache":{"standardDeviationFactor":1}}`))}
	ResolveRoutingOverrides(sharp)
	skipped, err := router.Route(sharp, podList)
	require.NoError(t, err)
	assert.NotEqual(t, p4Addr, skipped, "a sharp sigma skips the cache-warm but loaded replica")
}

func TestPrefillSigmaKnobControlsPrefillCandidacy(t *testing.T) {
	newCtx := func(requestID string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "hello world", requestID, "user")
		t.Cleanup(ctx.Delete)
		return ctx
	}

	pods := []*v1.Pod{
		makePDPod("prefill-1", "roleset-1", "", nil),
		makePDPod("prefill-2", "roleset-2", "", nil),
		makePDPod("prefill-3", "roleset-3", "", nil),
		makePDPod("prefill-hot", "roleset-hot", "", nil),
	}
	tracker := pd.NewPrefillRequestTracker()
	addRequests(tracker, "prefill-1", 1)
	addRequests(tracker, "prefill-2", 1)
	addRequests(tracker, "prefill-3", 1)
	addRequests(tracker, "prefill-hot", 10)

	r := &pdRouter{
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefillRequestTracker: tracker,
	}
	hotIsCandidate := func(scores map[string]*Scores) bool {
		for _, score := range scores {
			if score.Pod.Name == "prefill-hot" {
				return true
			}
		}
		return false
	}

	// Counts are [1,1,1,10]: the hot prefill replica sits several deviations above
	// the pool mean, so the sigma of the request decides its candidacy, the same
	// knob the prefix-cache strategies read.
	sharp := newCtx("req-prefill-sharp")
	sharp.SetRoutingOverrides(&types.RoutingOverrides{PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 1}})
	sharpScores, _, _ := r.scorePrefillPods(sharp, pods, nil)
	assert.Len(t, sharpScores, 3)
	assert.False(t, hotIsCandidate(sharpScores), "a sharp sigma drops the loaded prefill replica")

	wide := newCtx("req-prefill-wide")
	wide.SetRoutingOverrides(&types.RoutingOverrides{PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 4}})
	wideScores, _, _ := r.scorePrefillPods(wide, pods, nil)
	assert.Len(t, wideScores, 4)
	assert.True(t, hotIsCandidate(wideScores), "a generous sigma keeps it a candidate")
}
