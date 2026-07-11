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
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- parsePDAlgorithmConfig ---

func TestParsePDAlgorithmConfig_Nil(t *testing.T) {
	cfg := parsePDAlgorithmConfig(nil)
	assert.Equal(t, 0, cfg.PromptLenBucketMinLength)
	assert.Equal(t, math.MaxInt32, cfg.PromptLenBucketMaxLength)
	assert.False(t, cfg.Combined)
}

func TestParsePDAlgorithmConfig_Empty(t *testing.T) {
	cfg := parsePDAlgorithmConfig(json.RawMessage(`{}`))
	assert.Equal(t, 0, cfg.PromptLenBucketMinLength)
	assert.Equal(t, math.MaxInt32, cfg.PromptLenBucketMaxLength)
	assert.False(t, cfg.Combined)
}

func TestParsePDAlgorithmConfig_AllFields(t *testing.T) {
	raw := json.RawMessage(`{"promptLenBucketMinLength":10,"promptLenBucketMaxLength":100,"combined":true}`)
	cfg := parsePDAlgorithmConfig(raw)
	assert.Equal(t, 10, cfg.PromptLenBucketMinLength)
	assert.Equal(t, 100, cfg.PromptLenBucketMaxLength)
	assert.True(t, cfg.Combined)
}

func TestParsePDAlgorithmConfig_ZeroMaxBecomesMaxInt32(t *testing.T) {
	// When promptLenBucketMaxLength is 0 (unset in JSON), it should default to MaxInt32.
	raw := json.RawMessage(`{"promptLenBucketMinLength":5}`)
	cfg := parsePDAlgorithmConfig(raw)
	assert.Equal(t, 5, cfg.PromptLenBucketMinLength)
	assert.Equal(t, math.MaxInt32, cfg.PromptLenBucketMaxLength)
}

func TestParsePDAlgorithmConfig_NegativeMinClampedToZero(t *testing.T) {
	raw := json.RawMessage(`{"promptLenBucketMinLength":-10,"promptLenBucketMaxLength":50}`)
	cfg := parsePDAlgorithmConfig(raw)
	assert.Equal(t, 0, cfg.PromptLenBucketMinLength)
	assert.Equal(t, 50, cfg.PromptLenBucketMaxLength)
}

func TestParsePDAlgorithmConfig_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not valid json`)
	cfg := parsePDAlgorithmConfig(raw)
	// Fallback to defaults on parse error.
	assert.Equal(t, 0, cfg.PromptLenBucketMinLength)
	assert.Equal(t, math.MaxInt32, cfg.PromptLenBucketMaxLength)
}

func TestParsePDAlgorithmConfig_ScorePolicies(t *testing.T) {
	raw := json.RawMessage(`{"prefillScorePolicy":"least_request","decodeScorePolicy":"load_balancing"}`)
	cfg := parsePDAlgorithmConfig(raw)
	assert.Equal(t, "least_request", cfg.PrefillScorePolicy)
	assert.Equal(t, "load_balancing", cfg.DecodeScorePolicy)
}

// --- isPodSuitableForPromptLength ---

func newRouterForBucketingTests() *pdRouter {
	return &pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		httpClient:            &http.Client{},
		selectionCounts:       map[string]int64{},
	}
}

func podWithBucketAnnotation(name, roleset, role string, minLen, maxLen int, combined bool) *v1.Pod {
	anno := pdConfigAnnotation(minLen, maxLen, combined)
	return makePDPod(name, roleset, role, map[string]string{constants.ModelAnnoConfig: anno})
}

func TestIsPodSuitableForPromptLength_NoAnnotation(t *testing.T) {
	r := newRouterForBucketingTests()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := makePDPod("p", "rs", "prefill", nil)
	// Pod without annotation is suitable for any length.
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 0))
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 9999))
}

func TestIsPodSuitableForPromptLength_WithinRange(t *testing.T) {
	r := newRouterForBucketingTests()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := podWithBucketAnnotation("p", "rs", "prefill", 10, 50, false)
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 10)) // at min boundary
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 30)) // in middle
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 50)) // at max boundary
}

func TestIsPodSuitableForPromptLength_OutsideRange(t *testing.T) {
	r := newRouterForBucketingTests()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := podWithBucketAnnotation("p", "rs", "prefill", 10, 50, false)
	assert.False(t, r.isPodSuitableForPromptLength(ctx, pod, 9))  // below min
	assert.False(t, r.isPodSuitableForPromptLength(ctx, pod, 51)) // above max
}

func TestIsPodSuitableForPromptLength_MinGreaterThanMax(t *testing.T) {
	r := newRouterForBucketingTests()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	// Malformed config: min > max → always unsuitable.
	anno := `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":100,"promptLenBucketMaxLength":10,"combined":false}}}}`
	pod := makePDPod("p", "rs", "prefill", map[string]string{constants.ModelAnnoConfig: anno})
	assert.False(t, r.isPodSuitableForPromptLength(ctx, pod, 50))
	assert.False(t, r.isPodSuitableForPromptLength(ctx, pod, 100))
}

func TestIsPodSuitableForPromptLength_ZeroRangeDefaultsToAny(t *testing.T) {
	r := newRouterForBucketingTests()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	// min=0 and max=MaxInt32 (unset) → suitable for any prompt length.
	pod := podWithBucketAnnotation("p", "rs", "prefill", 0, 0, false) // max 0 → will become MaxInt32
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 0))
	assert.True(t, r.isPodSuitableForPromptLength(ctx, pod, 99999))
}

// --- isCombinedPod ---

func TestIsCombinedPod_NilProfile(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := makePDPod("p", "rs", "combined", nil) // no annotation → no profile
	assert.False(t, isCombinedPod(ctx, pod))
}

func TestIsCombinedPod_FalseInAnnotation(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := podWithBucketAnnotation("p", "rs", "prefill", 0, 100, false)
	assert.False(t, isCombinedPod(ctx, pod))
}

func TestIsCombinedPod_TrueInAnnotation(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	pod := podWithBucketAnnotation("p", "rs", "all", 0, 0, true)
	assert.True(t, isCombinedPod(ctx, pod))
}

// --- shouldPickCombined ---

// makePodWithRequestRateMetrics creates a pod with the given model label and
// sets up per-model metrics that calculatePodScoreBasedOffRequestRate reads.
func makePodWithRequestRateMetrics(name, namespace, modelName string, waitingReqs, drainRate float64) (*v1.Pod, map[string]metrics.MetricValue) {
	vec := model.Vector{&model.Sample{
		Metric: model.Metric{"__name__": "drain_rate_1m"},
		Value:  model.SampleValue(drainRate),
	}}
	var drainResult model.Value = vec
	return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{constants.ModelLabelName: modelName},
			},
		}, map[string]metrics.MetricValue{
			metrics.NumRequestsWaiting:          &metrics.SimpleMetricValue{Value: waitingReqs},
			metrics.NumPrefillPreallocQueueReqs: &metrics.SimpleMetricValue{Value: 0},
			metrics.NumDecodePreallocQueueReqs:  &metrics.SimpleMetricValue{Value: 0},
			metrics.DrainRate1m:                 &metrics.PrometheusMetricValue{Result: &drainResult},
		}
}

func TestShouldPickCombined_PrefillHighLoadCombinedLow(t *testing.T) {
	// prefill score > 1.0 (high), combined score < 0.25 (low) → should pick combined
	modelName := testModelName

	prefill, prefillMetrics := makePodWithRequestRateMetrics("prefill-1", "default", modelName, 10, 5)   // score=2.0 > 1.0
	decode, decodeMetrics := makePodWithRequestRateMetrics("decode-1", "default", modelName, 1, 4)       // score=0.25
	combined, combinedMetrics := makePodWithRequestRateMetrics("combined-1", "default", modelName, 0, 8) // score=0.0 < 0.25

	annotoCombined := pdConfigAnnotation(0, 0, true)
	combined.Annotations = map[string]string{constants.ModelAnnoConfig: annotoCombined}

	allPods := []*v1.Pod{prefill, decode, combined}
	metricsMap := map[string]map[string]metrics.MetricValue{
		prefill.Name:  prefillMetrics,
		decode.Name:   decodeMetrics,
		combined.Name: combinedMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	result := r.shouldPickCombined(ctx, []*v1.Pod{prefill}, []*v1.Pod{decode}, []*v1.Pod{combined})
	assert.True(t, result)
}

func TestShouldPickCombined_DecodeHighLoadCombinedLow(t *testing.T) {
	// decode score > 1.0, combined score < 0.25, prefill score below high threshold → pick combined
	modelName := testModelName

	prefill, prefillMetrics := makePodWithRequestRateMetrics("prefill-1", "default", modelName, 0, 8)    // score=0.0 (low)
	decode, decodeMetrics := makePodWithRequestRateMetrics("decode-1", "default", modelName, 10, 5)      // score=2.0 > 1.0
	combined, combinedMetrics := makePodWithRequestRateMetrics("combined-1", "default", modelName, 0, 8) // score=0.0 < 0.25

	annotoCombined := pdConfigAnnotation(0, 0, true)
	combined.Annotations = map[string]string{constants.ModelAnnoConfig: annotoCombined}

	allPods := []*v1.Pod{prefill, decode, combined}
	metricsMap := map[string]map[string]metrics.MetricValue{
		prefill.Name:  prefillMetrics,
		decode.Name:   decodeMetrics,
		combined.Name: combinedMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	result := r.shouldPickCombined(ctx, []*v1.Pod{prefill}, []*v1.Pod{decode}, []*v1.Pod{combined})
	assert.True(t, result)
}

func TestShouldPickCombined_CombinedHighLoad(t *testing.T) {
	// combined score >= 0.25 → do not route to combined even if prefill is high
	modelName := testModelName

	prefill, prefillMetrics := makePodWithRequestRateMetrics("prefill-1", "default", modelName, 10, 5)    // score=2.0
	decode, decodeMetrics := makePodWithRequestRateMetrics("decode-1", "default", modelName, 1, 4)        // score=0.25
	combined, combinedMetrics := makePodWithRequestRateMetrics("combined-1", "default", modelName, 10, 5) // score=2.0 (high)

	allPods := []*v1.Pod{prefill, decode, combined}
	metricsMap := map[string]map[string]metrics.MetricValue{
		prefill.Name:  prefillMetrics,
		decode.Name:   decodeMetrics,
		combined.Name: combinedMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	result := r.shouldPickCombined(ctx, []*v1.Pod{prefill}, []*v1.Pod{decode}, []*v1.Pod{combined})
	assert.False(t, result)
}

func TestShouldPickCombined_AllLowLoad(t *testing.T) {
	// neither prefill nor decode is high-load → do not route to combined
	modelName := testModelName

	prefill, prefillMetrics := makePodWithRequestRateMetrics("prefill-1", "default", modelName, 1, 4)    // score=0.25 (low)
	decode, decodeMetrics := makePodWithRequestRateMetrics("decode-1", "default", modelName, 1, 4)       // score=0.25 (low)
	combined, combinedMetrics := makePodWithRequestRateMetrics("combined-1", "default", modelName, 0, 8) // score=0 (low)

	allPods := []*v1.Pod{prefill, decode, combined}
	metricsMap := map[string]map[string]metrics.MetricValue{
		prefill.Name:  prefillMetrics,
		decode.Name:   decodeMetrics,
		combined.Name: combinedMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	result := r.shouldPickCombined(ctx, []*v1.Pod{prefill}, []*v1.Pod{decode}, []*v1.Pod{combined})
	assert.False(t, result)
}

func TestShouldPickCombined_NoCombinedPods(t *testing.T) {
	// no combined pods → always false
	modelName := testModelName

	prefill, prefillMetrics := makePodWithRequestRateMetrics("prefill-1", "default", modelName, 10, 5) // high load
	decode, decodeMetrics := makePodWithRequestRateMetrics("decode-1", "default", modelName, 10, 5)    // high load

	allPods := []*v1.Pod{prefill, decode}
	metricsMap := map[string]map[string]metrics.MetricValue{
		prefill.Name: prefillMetrics,
		decode.Name:  decodeMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	result := r.shouldPickCombined(ctx, []*v1.Pod{prefill}, []*v1.Pod{decode}, []*v1.Pod{})
	assert.False(t, result)
}

// --- scoreCombinedPods ---

func TestScoreCombinedPods_SinglePod(t *testing.T) {
	modelName := testModelName
	combined, combinedMetrics := makePodWithRequestRateMetrics("combined-1", "default", modelName, 2, 4) // score=0.5
	allPods := []*v1.Pod{combined}
	metricsMap := map[string]map[string]metrics.MetricValue{combined.Name: combinedMetrics}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	best := r.scoreCombinedPods(ctx, []*v1.Pod{combined})
	assert.NotNil(t, best)
	assert.Equal(t, "combined-1", best.Name)
}

func TestScoreCombinedPods_PicksLowest(t *testing.T) {
	// combined-low has score=0.1 (2/20), combined-high has score=2.0 (10/5)
	modelName := testModelName
	lowPod, lowMetrics := makePodWithRequestRateMetrics("combined-low", "default", modelName, 2, 20)    // score=0.1
	highPod, highMetrics := makePodWithRequestRateMetrics("combined-high", "default", modelName, 10, 5) // score=2.0

	allPods := []*v1.Pod{lowPod, highPod}
	metricsMap := map[string]map[string]metrics.MetricValue{
		lowPod.Name:  lowMetrics,
		highPod.Name: highMetrics,
	}

	r := &pdRouter{cache: cache.NewWithPodsMetricsForTest(allPods, modelName, metricsMap)}
	ctx := types.NewRoutingContext(context.Background(), "pd", modelName, "msg", "req", "user")

	best := r.scoreCombinedPods(ctx, []*v1.Pod{lowPod, highPod})
	assert.NotNil(t, best)
	assert.Equal(t, "combined-low", best.Name)
}

func TestScoreCombinedPods_NoMetricsFallsToZeroScore(t *testing.T) {
	// When no metrics are registered, drainRate=0 → score=0/0=NaN.
	// IEEE 754: NaN < MaxFloat64 is false, so no pod wins the min-score comparison
	// and scoreCombinedPods returns nil.
	r := &pdRouter{cache: cache.NewForTest()}
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")

	pod1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "c1", Labels: map[string]string{constants.ModelLabelName: "model"}}}
	pod2 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "c2", Labels: map[string]string{constants.ModelLabelName: "model"}}}

	best := r.scoreCombinedPods(ctx, []*v1.Pod{pod1, pod2})
	assert.Nil(t, best, "no metrics → NaN score does not beat MaxFloat64, returns nil")
}

// --- filterPrefillDecodePods bucketing integration ---

func TestFilterPrefillDecodePods_BucketingSelectsMatchingRoleset(t *testing.T) {
	// Verify that when two rolesets have non-overlapping buckets, the request
	// is routed to the roleset whose bucket covers the actual prompt length.
	// We use one wide-range bucket [0, 99999] and one unreachable bucket [100000, 200000]
	// so the short test prompt definitely hits the wide bucket regardless of tokenizer.
	old := aibrixPromptLengthBucketing
	aibrixPromptLengthBucketing = true
	defer func() { aibrixPromptLengthBucketing = old }()

	r := pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		httpClient:            &http.Client{},
		selectionCounts:       map[string]int64{},
	}

	// rs-match: covers any realistic prompt length
	annoMatch := pdConfigAnnotation(0, 99999, false)
	// rs-nomatch: unreachably large range for any test prompt
	annoNoMatch := pdConfigAnnotation(100000, 200000, false)

	prefillMatch := makePDPod("prefill-match", "rs-match", "prefill", map[string]string{constants.ModelAnnoConfig: annoMatch})
	decodeMatch := makePDPod("decode-match", "rs-match", "decode", map[string]string{constants.ModelAnnoConfig: annoMatch})
	prefillNoMatch := makePDPod("prefill-nomatch", "rs-nomatch", "prefill", map[string]string{constants.ModelAnnoConfig: annoNoMatch})
	decodeNoMatch := makePDPod("decode-nomatch", "rs-nomatch", "decode", map[string]string{constants.ModelAnnoConfig: annoNoMatch})
	allPods := []*v1.Pod{prefillMatch, decodeMatch, prefillNoMatch, decodeNoMatch}

	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "hello world", "req-1", "user")
	p, d, err := r.filterPrefillDecodePods(ctx, allPods)
	assert.NoError(t, err)
	assert.Equal(t, "prefill-match", p.Name)
	assert.Equal(t, "decode-match", d.Name)
}

func TestFilterPrefillDecodePods_BucketingDisabledIgnoresAnnotations(t *testing.T) {
	old := aibrixPromptLengthBucketing
	aibrixPromptLengthBucketing = false
	defer func() { aibrixPromptLengthBucketing = old }()

	r := pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		httpClient:            &http.Client{},
		selectionCounts:       map[string]int64{},
	}

	// Narrow bucket that would not match any reasonable prompt length.
	annoNarrow := pdConfigAnnotation(99999, 100000, false)
	prefill := makePDPod("prefill-1", "rs1", "prefill", map[string]string{constants.ModelAnnoConfig: annoNarrow})
	decode := makePDPod("decode-1", "rs1", "decode", map[string]string{constants.ModelAnnoConfig: annoNarrow})

	// With bucketing disabled, annotations are ignored and the pod is selected normally.
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "hello", "req", "user")
	p, d, err := r.filterPrefillDecodePods(ctx, []*v1.Pod{prefill, decode})
	assert.NoError(t, err)
	assert.NotNil(t, p)
	assert.NotNil(t, d)
}

func TestFilterPrefillDecodePods_MultipleBucketsCombinedFallback(t *testing.T) {
	// With two unreachably high-range PD buckets and a combined pod,
	// any short test prompt falls outside both buckets and routes to combined.
	// Using [99999, 199999] ensures no realistic test message hits either bucket
	// regardless of tokenizer (tiktoken or character-based).
	old := aibrixPromptLengthBucketing
	aibrixPromptLengthBucketing = true
	defer func() { aibrixPromptLengthBucketing = old }()

	r := pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefixCacheIndexer:    prefixcacheindexer.NewPrefixHashTable(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		httpClient:            &http.Client{},
		selectionCounts:       map[string]int64{},
	}

	annoHigh1 := pdConfigAnnotation(99999, 149999, false)
	annoHigh2 := pdConfigAnnotation(150000, 199999, false)
	annoCombined := pdConfigAnnotation(0, 0, true)

	prefillHigh1 := makePDPod("prefill-high1", "rs-high1", "prefill", map[string]string{constants.ModelAnnoConfig: annoHigh1})
	decodeHigh1 := makePDPod("decode-high1", "rs-high1", "decode", map[string]string{constants.ModelAnnoConfig: annoHigh1})
	prefillHigh2 := makePDPod("prefill-high2", "rs-high2", "prefill", map[string]string{constants.ModelAnnoConfig: annoHigh2})
	decodeHigh2 := makePDPod("decode-high2", "rs-high2", "decode", map[string]string{constants.ModelAnnoConfig: annoHigh2})
	combined := makePDPod("combined-1", "rs-comb", "combined", map[string]string{constants.ModelAnnoConfig: annoCombined})

	allPods := []*v1.Pod{prefillHigh1, decodeHigh1, prefillHigh2, decodeHigh2, combined}

	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "say test", "req-fallback", "user")
	p, d, err := r.filterPrefillDecodePods(ctx, allPods)
	assert.NoError(t, err)
	assert.Nil(t, p, "combined routing should not set a prefill pod")
	assert.NotNil(t, d)
	assert.Equal(t, "combined-1", d.Name)
}
