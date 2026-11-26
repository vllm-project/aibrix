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

package router

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	v1 "k8s.io/api/core/v1"
)

type lockCheckingCache struct {
	cache.Cache
	lock                  *sync.Mutex
	readsWhileLocked      atomic.Int64
	drainRateLookupsByPod sync.Map // podName -> *atomic.Int64
}

func (c *lockCheckingCache) checkLock() {
	if c.lock == nil {
		return
	}
	if c.lock.TryLock() {
		c.lock.Unlock()
		return
	}
	c.readsWhileLocked.Add(1)
}

func (c *lockCheckingCache) GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error) {
	c.checkLock()
	if metricName == metrics.RealtimeRunningRequestsDrainRate1m {
		val, _ := c.drainRateLookupsByPod.LoadOrStore(podName, &atomic.Int64{})
		val.(*atomic.Int64).Add(1)
	}
	return c.Cache.GetMetricValueByPod(podName, podNamespace, metricName)
}

func (c *lockCheckingCache) GetMetricValueByPodModel(podName, podNamespace, modelName, metricName string) (metrics.MetricValue, error) {
	c.checkLock()
	return c.Cache.GetMetricValueByPodModel(podName, podNamespace, modelName, metricName)
}

// TestFilterPrefillDecodePods_NoCacheReadsUnderSelectLock verifies Phase 1 of
// #3620: every read-only scraped metric lookup (decode throughput, KV usage,
// drain rate, readiness, KV capacity, and conductor prefill histogram) runs
// before selectStripe.Lock() is acquired, across all major policy pairings.
func TestFilterPrefillDecodePods_NoCacheReadsUnderSelectLock(t *testing.T) {
	const model = "lock-isolation-model"
	prefillPods := benchmarkPDPods(VLLMEngine, "prefill", 4, 3)
	decodePods := benchmarkPDPods(VLLMEngine, "decode", 4, 3)
	readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePods...)

	podMetrics := make(map[string]map[string]metrics.MetricValue, len(readyPods))
	for _, pod := range prefillPods {
		podMetrics[pod.Name] = map[string]metrics.MetricValue{
			metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{
				Sum:   0.5,
				Count: 10,
			},
		}
	}
	for i, pod := range decodePods {
		podMetrics[pod.Name] = map[string]metrics.MetricValue{
			metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: float64(1 + i%2)},
			metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 512},
			metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.25},
			metrics.KVCacheCapacityTokens:           &metrics.SimpleMetricValue{Value: float64(32768 * (1 + i%2))},
		}
	}
	baseCache := cache.NewWithPodsMetricsForTest(readyPods, model, podMetrics)
	prefixTable := prefixcacheindexer.NewPrefixHashTable()
	tokenTracker := pd.NewTokenLoadTracker()

	cases := []struct {
		name    string
		prefill func(c cache.Cache) pd.PrefillScorePolicy
		decode  pd.DecodeScorePolicy
	}{
		{
			name:    "prefix_cache+load_balancing",
			prefill: func(_ cache.Cache) pd.PrefillScorePolicy { return newPrefixCachePrefillPolicy(prefixTable) },
			decode:  pd.LoadBalancingDecodePolicy{},
		},
		{
			name: "hybrid_cache_load+token_load",
			prefill: func(_ cache.Cache) pd.PrefillScorePolicy {
				return newHybridCacheLoadPrefillPolicy(prefixTable, tokenTracker)
			},
			decode: pd.TokenLoadDecodePolicy{},
		},
		{
			name:    "conductor+conductor",
			prefill: func(c cache.Cache) pd.PrefillScorePolicy { return newConductorPrefillPolicy(prefixTable, c) },
			decode:  pd.ConductorDecodePolicy{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &lockCheckingCache{Cache: baseCache}
			r := &pdRouter{
				cache:                 spy,
				prefillPolicy:         tc.prefill(spy),
				decodePolicy:          tc.decode,
				prefixCacheIndexer:    prefixTable,
				prefillRequestTracker: pd.NewPrefillRequestTracker(),
				pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
				tokenLoadTracker:      tokenTracker,
				prefixUpdateCh:        make(chan prefixUpdateJob, 64),
				selectionCounts:       map[string]int64{},
			}
			spy.lock = r.selectLock(model)

			ctx := types.NewRoutingContext(context.Background(), RouterPD, model,
				strings.Repeat("lock isolation prompt ", 16), "req-"+tc.name, "user")
			t.Cleanup(ctx.Delete)
			ctx.Engine = VLLMEngine
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = randReqBody(1024)

			p, d, err := r.filterPrefillDecodePods(ctx, readyPods)
			require.NoError(t, err)
			require.NotNil(t, p)
			require.NotNil(t, d)
			assert.Zero(t, spy.readsWhileLocked.Load(),
				"no r.cache metric lookups may execute while holding selectStripe.Lock()")
			// Confirm post-unlock bookkeeping still ran cleanly.
			r.countersMu.RLock()
			assert.EqualValues(t, 1, r.selectionCounts[utils.GeneratePodKey(p.Namespace, p.Name)])
			assert.EqualValues(t, 1, r.selectionCounts[utils.GeneratePodKey(d.Namespace, d.Name)])
			r.countersMu.RUnlock()
		})
	}
}

// TestPrefetchDecodeMetrics_DrainRateSubsetEquivalence verifies that per-roleset
// drain-rate short-circuiting skips redundant lookups for rolesets missing drain
// rates while still preserving exact drain-rate fast-path behavior when prefill
// imbalance narrows decodePods to a roleset that DOES report drain rates.
func TestPrefetchDecodeMetrics_DrainRateSubsetEquivalence(t *testing.T) {
	const model = "drain-subset-model"
	prefillPods := benchmarkPDPods(VLLMEngine, "prefill", 2, 3)
	decodePods := benchmarkPDPods(VLLMEngine, "decode", 2, 3)
	readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePods...)

	// rs-0: NO drain rates on any decode pod.
	// rs-1: positive drain rates with >1.5 score ratio (fast pod vs slow pod).
	podMetrics := make(map[string]map[string]metrics.MetricValue, len(decodePods))
	for _, pod := range decodePods {
		rs := pod.Labels[PDRoleSetIdentifier]
		m := map[string]metrics.MetricValue{
			metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 4},
			metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 500},
			metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.2},
		}
		if rs == "vllm-rs-1" {
			// Make rep0 have a much higher drain rate (lower score = 4/10 = 0.4) than rep1/rep2 (4/1 = 4.0).
			if strings.HasSuffix(pod.Name, "rep0") {
				m[metrics.RealtimeRunningRequestsDrainRate1m] = &metrics.SimpleMetricValue{Value: 10}
			} else {
				m[metrics.RealtimeRunningRequestsDrainRate1m] = &metrics.SimpleMetricValue{Value: 1}
			}
		}
		podMetrics[pod.Name] = m
	}

	baseCache := cache.NewWithPodsMetricsForTest(readyPods, model, podMetrics)
	spy := &lockCheckingCache{Cache: baseCache}
	r := &pdRouter{
		cache:                 spy,
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		decodePolicy:          pd.LoadBalancingDecodePolicy{},
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		prefixUpdateCh:        make(chan prefixUpdateJob, 64),
		selectionCounts:       map[string]int64{},
	}

	ctx := types.NewRoutingContext(context.Background(), RouterPD, model, "hello", "req-drain", "user")
	t.Cleanup(ctx.Delete)
	ctx.Engine = VLLMEngine

	snaps := r.prefetchDecodeMetrics(ctx, decodePods, false, true)

	// In rs-0 (3 pods, all missing drain rate), short-circuiting must stop after 1 lookup.
	var rs0DrainLookups int64
	for _, pod := range decodePods {
		if pod.Labels[PDRoleSetIdentifier] == "vllm-rs-0" {
			if v, ok := spy.drainRateLookupsByPod.Load(pod.Name); ok {
				rs0DrainLookups += v.(*atomic.Int64).Load()
			}
		}
	}
	assert.EqualValues(t, 1, rs0DrainLookups, "missing drain rate in a roleset should short-circuit remaining pods in that roleset")

	runningCounts := make(map[string]int64, len(decodePods))
	for _, pod := range decodePods {
		runningCounts[utils.GeneratePodKey(pod.Namespace, pod.Name)] = 4
	}

	// Full fleet includes rs-0 (no drain rates), so drain-rate fast path must NOT fire.
	pickedFull, _, _, _, _, _, _ := r.loadImbalanceSelectDecodePodFromSnapshots(ctx, append([]*v1.Pod(nil), decodePods...), snaps, runningCounts, nil)
	assert.Nil(t, pickedFull, "full fleet with missing drain rates in rs-0 must not trigger drain-rate fast path")

	// Narrowed to rs-1 (all have drain rates), drain-rate fast path MUST pick rep0.
	rs1Decodes := utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, "vllm-rs-1")
	pickedRS1, _, _, _, _, _, _ := r.loadImbalanceSelectDecodePodFromSnapshots(ctx, rs1Decodes, snaps, runningCounts, nil)
	require.NotNil(t, pickedRS1, "narrowing to rs-1 with complete drain rates must still trigger drain-rate fast path")
	assert.True(t, strings.HasSuffix(pickedRS1.Name, "rep0"), "expected highest-drain-rate pod rep0, got %s", pickedRS1.Name)
}

// TestDecodeKVCapacitiesFromSnapshots_MatchesDirect ensures prefetched raw KV
// capacities produce identical mean/capacity scaling factors to decodeKVCapacities
// even after decodePods is narrowed to a subset.
func TestDecodeKVCapacitiesFromSnapshots_MatchesDirect(t *testing.T) {
	const model = "kv-cap-subset-model"
	decodePods := benchmarkPDPods(VLLMEngine, "decode", 2, 2)
	podMetrics := map[string]map[string]metrics.MetricValue{
		decodePods[0].Name: {metrics.KVCacheCapacityTokens: &metrics.SimpleMetricValue{Value: 10000}},
		decodePods[1].Name: {metrics.KVCacheCapacityTokens: &metrics.SimpleMetricValue{Value: 30000}},
		decodePods[2].Name: {metrics.KVCacheCapacityTokens: &metrics.SimpleMetricValue{Value: 50000}},
		// decodePods[3] intentionally omits capacity to exercise mean fallback (factor = 1).
	}
	c := cache.NewWithPodsMetricsForTest(decodePods, model, podMetrics)
	r := &pdRouter{cache: c}
	ctx := types.NewRoutingContext(context.Background(), RouterPD, model, "", "req-cap", "user")
	t.Cleanup(ctx.Delete)

	snaps := r.prefetchDecodeMetrics(ctx, decodePods, true, false)

	// Full set equivalence.
	assert.Equal(t, r.decodeKVCapacities(true, decodePods), decodeKVCapacitiesFromSnapshots(true, decodePods, snaps))

	// Narrowed subset (rs-1 only: 50000 + unknown) must recompute mean over rs-1, not the full fleet.
	rs1 := utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, "vllm-rs-1")
	assert.Equal(t, r.decodeKVCapacities(true, rs1), decodeKVCapacitiesFromSnapshots(true, rs1, snaps))
}

// TestKeyedPrefillScorers_MatchScorePod verifies every built-in prefill scorer's
// ScorePodByKey matches ScorePod identically.
func TestKeyedPrefillScorers_MatchScorePod(t *testing.T) {
	const model = "keyed-scorer-model"
	pods := benchmarkPDPods(VLLMEngine, "prefill", 2, 2)
	podMetrics := map[string]map[string]metrics.MetricValue{
		pods[0].Name: {metrics.RequestPrefillTimeSeconds: &metrics.HistogramMetricValue{Sum: 0.4, Count: 8}},
	}
	c := cache.NewWithPodsMetricsForTest(pods, model, podMetrics)
	prefixTable := prefixcacheindexer.NewPrefixHashTable()
	tok := prefixcache.NewTokenizer()
	tokenTracker := pd.NewTokenLoadTracker()

	prompt := "keyed scorer parity prompt"
	tokens, err := tok.TokenizeInputText(prompt)
	require.NoError(t, err)
	prefixTable.AddPrefix(prefixTable.GetPrefixHashes(tokens), model, utils.GeneratePodKey(pods[0].Namespace, pods[0].Name))
	tokenTracker.AcquirePrefill("seed", utils.GeneratePodKey(pods[1].Namespace, pods[1].Name), 1200)

	readyMap := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		readyMap[utils.GeneratePodKey(p.Namespace, p.Name)] = struct{}{}
	}
	ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-keyed", "user")
	t.Cleanup(ctx.Delete)

	policies := []pd.PrefillScorePolicy{
		pd.NewPrefixCachePrefillPolicy(tok, prefixTable),
		pd.NewLeastRequestPrefillPolicy(),
		pd.NewConductorPrefillPolicy(tok, prefixTable, c, pd.NewConductorPrefillPolicyConfig()),
		pd.NewTokenLoadPrefillPolicy(tokenTracker),
		pd.NewHybridCacheLoadPrefillPolicy(tok, prefixTable, tokenTracker),
	}

	for _, pol := range policies {
		t.Run(pol.Name(), func(t *testing.T) {
			scorer, err := pol.Prepare(ctx, pods, readyMap)
			require.NoError(t, err)
			keyed, ok := scorer.(pd.KeyedPrefillScorer)
			require.True(t, ok, "policy %s scorer must implement KeyedPrefillScorer", pol.Name())
			for i, p := range pods {
				key := utils.GeneratePodKey(p.Namespace, p.Name)
				reqCnt := float64(i + 1)
				assert.InDelta(t, scorer.ScorePod(p, reqCnt, 4), keyed.ScorePodByKey(p, key, reqCnt, 4), 1e-12)
			}
		})
	}
}
