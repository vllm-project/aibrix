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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// routingKnobsContext builds a routing context the way the gateway does: a context that
// carries the model config profile under test. ResolveRoutingKnobs reads only the
// profile's routingConfig, so the algorithm string does not matter here.
func routingKnobsContext(t *testing.T, routingConfig string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterNotSet, "model", "message", "req-routing-knobs", "user")
	t.Cleanup(ctx.Delete)
	if routingConfig != "" {
		ctx.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: json.RawMessage(routingConfig)}
	}
	return ctx
}

func TestResolveRoutingKnobsFromRoutingConfig(t *testing.T) {
	tests := []struct {
		name          string
		routingConfig string
		wantNil       bool
		check         func(t *testing.T, knobs *types.RoutingKnobs)
	}{
		{
			name:          "no routingConfig keeps every environment default",
			routingConfig: "",
			wantNil:       true,
		},
		{
			name:          "routingConfig without routing sections keeps every environment default",
			routingConfig: `{"promptLengthBucketing":true,"pd":{"abort":{"timeout":1}}}`,
			wantNil:       true,
		},
		{
			name:          "unparsable routingConfig keeps every environment default",
			routingConfig: `{"loadBalance":`,
			wantNil:       true,
		},
		{
			name:          "load-balance gate and score",
			routingConfig: `{"loadBalance":{"imbalanceFactor":3.5,"imbalanceMinGap":12,"queuedWeight":0,"kvPressureAlpha":0,"kvCriticalFree":0.05}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 3.5, knobs.LoadBalanceImbalanceFactorOrDefault(-1))
				assert.Equal(t, 12, knobs.LoadBalanceImbalanceMinGapOrDefault(-1))
				assert.Equal(t, 0.0, knobs.LoadBalanceQueuedWeightOrDefault(-1), "an explicit zero is an override, not an unset value")
				assert.Equal(t, 0.0, knobs.LoadBalanceKVPressureAlphaOrDefault(-1), "zero drops the penalty")
				assert.Equal(t, 0.05, knobs.LoadBalanceKVCriticalFreeOrDefault(-1))
			},
		},
		{
			name:          "prefix-cache standard-deviation factor",
			routingConfig: `{"prefixCache":{"standardDeviationFactor":3}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 3, knobs.PrefixCacheStandardDeviationFactorOrDefault(-1))
			},
		},
		{
			name:          "preble cost model",
			routingConfig: `{"preble":{"targetGPU":"A6000","decodingLength":64}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, "A6000", knobs.PrebleTargetGPUOrDefault("V100"))
				assert.Equal(t, 64, knobs.PrebleDecodingLengthOrDefault(-1))
			},
		},
		{
			name:          "vtc score",
			routingConfig: `{"vtc":{"maxPodLoad":200,"fairnessWeight":0,"utilizationWeight":2.5}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 200.0, knobs.VTCMaxPodLoadOrDefault(-1))
				assert.Equal(t, 0.0, knobs.VTCFairnessWeightOrDefault(-1), "zero drops the fairness term")
				assert.Equal(t, 2.5, knobs.VTCUtilizationWeightOrDefault(-1))
			},
		},
		{
			name:          "auto-blend weights",
			routingConfig: `{"autoBlend":{"loadBalanceWeight":2,"leastRequestWeight":3,"prefixCacheWeight":7,"prefixCacheLoadBalanceWeight":2}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 2, knobs.AutoBlendLoadBalanceWeightOrDefault(-1))
				assert.Equal(t, 3, knobs.AutoBlendLeastRequestWeightOrDefault(-1))
				assert.Equal(t, 7, knobs.AutoBlendPrefixCacheWeightOrDefault(-1))
				assert.Equal(t, 2, knobs.AutoBlendPrefixCacheLoadBalanceWeightOrDefault(-1))
			},
		},
		{
			name: "values the environment would reject keep the environment default",
			routingConfig: `{"loadBalance":{"imbalanceFactor":-1,"imbalanceMinGap":0,"queuedWeight":-0.5,"kvPressureAlpha":-1,"kvCriticalFree":1.5},
				"prefixCache":{"standardDeviationFactor":0},
				"preble":{"targetGPU":"H100","decodingLength":0},
				"vtc":{"maxPodLoad":0,"fairnessWeight":-1,"utilizationWeight":-1},
				"autoBlend":{"loadBalanceWeight":-1,"leastRequestWeight":-1,"prefixCacheWeight":0,"prefixCacheLoadBalanceWeight":-1}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 1.5, knobs.LoadBalanceImbalanceFactorOrDefault(1.5))
				assert.Equal(t, 2, knobs.LoadBalanceImbalanceMinGapOrDefault(2))
				assert.Equal(t, 3.5, knobs.LoadBalanceQueuedWeightOrDefault(3.5))
				assert.Equal(t, 4.5, knobs.LoadBalanceKVPressureAlphaOrDefault(4.5))
				assert.Equal(t, 5.5, knobs.LoadBalanceKVCriticalFreeOrDefault(5.5))
				assert.Equal(t, 6, knobs.PrefixCacheStandardDeviationFactorOrDefault(6))
				assert.Equal(t, "V100", knobs.PrebleTargetGPUOrDefault("V100"))
				assert.Equal(t, 7, knobs.PrebleDecodingLengthOrDefault(7))
				assert.Equal(t, 8.5, knobs.VTCMaxPodLoadOrDefault(8.5))
				assert.Equal(t, 9.5, knobs.VTCFairnessWeightOrDefault(9.5))
				assert.Equal(t, 10.5, knobs.VTCUtilizationWeightOrDefault(10.5))
				assert.Equal(t, 11, knobs.AutoBlendLoadBalanceWeightOrDefault(11))
				assert.Equal(t, 12, knobs.AutoBlendLeastRequestWeightOrDefault(12))
				assert.Equal(t, 13, knobs.AutoBlendPrefixCacheWeightOrDefault(13))
				assert.Equal(t, 14, knobs.AutoBlendPrefixCacheLoadBalanceWeightOrDefault(14))
			},
		},
		{
			name: "boundary values the environment accepts",
			routingConfig: `{"loadBalance":{"imbalanceFactor":0.5,"imbalanceMinGap":1,"kvCriticalFree":1},
				"prefixCache":{"standardDeviationFactor":1},
				"autoBlend":{"loadBalanceWeight":0,"leastRequestWeight":1000000,"prefixCacheWeight":1,"prefixCacheLoadBalanceWeight":0}}`,
			check: func(t *testing.T, knobs *types.RoutingKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 0.5, knobs.LoadBalanceImbalanceFactorOrDefault(-1))
				assert.Equal(t, 1, knobs.LoadBalanceImbalanceMinGapOrDefault(-1))
				assert.Equal(t, 1.0, knobs.LoadBalanceKVCriticalFreeOrDefault(-1), "the critical free-KV bound is inclusive")
				assert.Equal(t, 1, knobs.PrefixCacheStandardDeviationFactorOrDefault(-1))
				assert.Equal(t, 0, knobs.AutoBlendLoadBalanceWeightOrDefault(-1), "0 disables the auto-blend for the request")
				assert.Equal(t, 1000000, knobs.AutoBlendLeastRequestWeightOrDefault(-1))
				assert.Equal(t, 1, knobs.AutoBlendPrefixCacheWeightOrDefault(-1))
				assert.Equal(t, 0, knobs.AutoBlendPrefixCacheLoadBalanceWeightOrDefault(-1), "0 leaves prefix-cache scoring alone")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := routingKnobsContext(t, tt.routingConfig)
			ResolveRoutingKnobs(ctx)
			knobs := ctx.RoutingKnobs()
			if tt.wantNil {
				assert.Nil(t, knobs)
				return
			}
			require.NotNil(t, knobs)
			tt.check(t, knobs)
		})
	}
}

func TestResolveRoutingKnobsParksOnContext(t *testing.T) {
	plain := routingKnobsContext(t, "")
	ResolveRoutingKnobs(plain)
	assert.Nil(t, plain.RoutingKnobs(), "a request without profile knobs keeps the env defaults")

	profiled := routingKnobsContext(t, `{"prefixCache":{"standardDeviationFactor":4}}`)
	ResolveRoutingKnobs(profiled)
	knobs := profiled.RoutingKnobs()
	require.NotNil(t, knobs)
	assert.Equal(t, 4, knobs.PrefixCacheStandardDeviationFactorOrDefault(0))
	assert.Same(t, knobs, profiled.RoutingKnobs(), "the strategies and gates of one request read the same resolved values")

	// A section whose every value the environment would reject parks an override set
	// whose field is unset, so the accessor still falls back.
	rejected := routingKnobsContext(t, `{"prefixCache":{"standardDeviationFactor":0}}`)
	ResolveRoutingKnobs(rejected)
	assert.Equal(t, standardDeviationFactor, rejected.RoutingKnobs().PrefixCacheStandardDeviationFactorOrDefault(standardDeviationFactor))

	ResolveRoutingKnobs(nil) // nil routing context: must not panic
}

func TestLoadImbalanceGateHonorsProfileKnobs(t *testing.T) {
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

	plain := routingKnobsContext(t, "")
	ResolveRoutingKnobs(plain)
	assert.Len(t, ApplyLoadImbalanceGate(plain, c, pods), 3, "the env defaults exclude the busy replica")

	// meanOfOthers = (2+2+2)/3 = 2, so the env factor fires at 20 > 2*(2+1).
	// A request whose profile raises the factor keeps the busy replica a candidate.
	relaxed := routingKnobsContext(t, `{"loadBalance":{"imbalanceFactor":20}}`)
	ResolveRoutingKnobs(relaxed)
	assert.Len(t, ApplyLoadImbalanceGate(relaxed, c, pods), 4)

	wideGap := routingKnobsContext(t, `{"loadBalance":{"imbalanceMinGap":100}}`)
	ResolveRoutingKnobs(wideGap)
	assert.Len(t, ApplyLoadImbalanceGate(wideGap, c, pods), 4)

	assert.Len(t, ApplyLoadImbalanceGate(routingKnobsContext(t, ""), c, pods), 3, "another request's profile must not change this one")
}

func TestAutoBlendWeightsHonorProfileKnobs(t *testing.T) {
	withAutoBlendWeights(t, 1, 1)

	defaults := effectiveAutoBlendWeights(nil)
	assert.Equal(t, autoBlendLoadBalanceWeight, defaults.loadBalance)
	assert.Equal(t, autoBlendLeastRequestWeight, defaults.leastRequest)
	assert.Equal(t, autoBlendPrefixCacheWeight, defaults.prefixCache)
	assert.Equal(t, autoBlendPrefixCacheLoadBalanceWeight, defaults.prefixCacheLoadBalance)

	profiled := routingKnobsContext(t, `{"autoBlend":{"loadBalanceWeight":2,"leastRequestWeight":3,"prefixCacheWeight":7,"prefixCacheLoadBalanceWeight":2}}`)
	ResolveRoutingKnobs(profiled)
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
	off := routingKnobsContext(t, `{"autoBlend":{"loadBalanceWeight":0}}`)
	ResolveRoutingKnobs(off)
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

	decoding := 32
	profiled := createTestRoutingContext("model", "hello world", "req-preble-knobs")
	profiled.SetRoutingKnobs(&types.RoutingKnobs{PrebleDecodingLength: &decoding})
	require.NoError(t, router.PostRouteUpdate(profiled, podList, pod))
	assert.Equal(t, 32, router.histogram.currentDecodeLengthsPerPod[pod.Name])

	plain := createTestRoutingContext("model", "hello world", "req-preble-default")
	require.NoError(t, router.PostRouteUpdate(plain, podList, pod))
	assert.Equal(t, 32+decodingLength, router.histogram.currentDecodeLengthsPerPod[pod.Name], "a request without the knob keeps the environment default")
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
	wide.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: json.RawMessage(`{"prefixCache":{"standardDeviationFactor":4}}`)}
	ResolveRoutingKnobs(wide)
	chosen, err := router.Route(wide, podList)
	require.NoError(t, err)
	assert.Equal(t, p4Addr, chosen, "a generous sigma keeps the cache-warm replica a candidate")

	sharp := types.NewRoutingContext(context.Background(), RouterPrefixCache, model, "abcdefgh", "req-sharp", "")
	sharp.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: json.RawMessage(`{"prefixCache":{"standardDeviationFactor":1}}`)}
	ResolveRoutingKnobs(sharp)
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
	sharpSigma, wideSigma := 1, 4

	sharp := newCtx("req-prefill-sharp")
	sharp.SetRoutingKnobs(&types.RoutingKnobs{PrefixCacheStandardDeviationFactor: &sharpSigma})
	sharpScores, _, _ := r.scorePrefillPods(sharp, pods, nil)
	assert.Len(t, sharpScores, 3)
	assert.False(t, hotIsCandidate(sharpScores), "a sharp sigma drops the loaded prefill replica")

	wide := newCtx("req-prefill-wide")
	wide.SetRoutingKnobs(&types.RoutingKnobs{PrefixCacheStandardDeviationFactor: &wideSigma})
	wideScores, _, _ := r.scorePrefillPods(wide, pods, nil)
	assert.Len(t, wideScores, 4)
	assert.True(t, hotIsCandidate(wideScores), "a generous sigma keeps it a candidate")
}
