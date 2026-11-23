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
	"math"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var RouterLoadBalance types.RoutingAlgorithm = "load-balance"

// podRunningRequestImbalanceFactor triggers the load-imbalance gate when
// max > factor*(mean+1). Used only for 3+ pods; two pods skip this check
// because factor=2 reduces to max > max+2, which never holds.
// podRunningRequestImbalanceMinGap is the minimum absolute gap required to trigger.
//
// TODO: this compares raw running-request counts, not capacity-normalized load. In a
// heterogeneous pool a replica that is simply faster than its peers is expected to carry more
// concurrent requests without being more loaded (the same capacity signal loadBalanceScore
// uses), but this gate has no notion of capacity and can flag such a replica as a hotspot and
// exclude it from routing before any strategy's capacity-aware scoring gets a chance to run.
// Move this to compare capacity-normalized load instead of raw counts.
var (
	podRunningRequestImbalanceFactor = utils.LoadEnvFloat("AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR", 2.0)
	podRunningRequestImbalanceMinGap = utils.LoadEnvInt("AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP", 8)
)

// Score knobs, see loadBalanceScore. The defaults are the "V1" formula: running requests only
// (queued weight 0, since the gateway's running count already includes requests queued inside the
// engine), a quadratic KV-pressure penalty of strength 2, and a guardrail at 10% free KV that is
// hard only when load-balance routes alone — in a multi-strategy blend it is a strong penalty,
// not an exclusion (see the "Interaction with the rest of the gateway" section of load_balance.md).
var (
	loadBalanceQueuedWeight    = utils.LoadEnvFloat("AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT", 0.0)
	loadBalanceKVPressureAlpha = utils.LoadEnvFloat("AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA", 2.0)
	loadBalanceKVCriticalFree  = utils.LoadEnvFloat("AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE", 0.10)
)

var (
	loadImbalanceEvents     *prometheus.CounterVec
	loadImbalanceEventsOnce sync.Once

	kvSyncEnabledOnce  sync.Once
	kvSyncEnabledValue bool
)

// kvSyncEnabled reports whether KV event sync is enabled for this deployment, per the same
// env var prefixcache's NewPrefixCacheRouter reads. The value is a process-wide
// deployment setting decided once at startup, so it's cached rather than re-read from the
// environment on every request.
func kvSyncEnabled() bool {
	kvSyncEnabledOnce.Do(func() {
		kvSyncEnabledValue = utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)
	})
	return kvSyncEnabledValue
}

// recordLoadImbalance increments the load-imbalance gate counter for model. The gate itself
// is applied centrally by the gateway ahead of whichever strategy actually routes the request
// (see ApplyLoadImbalanceGate), so the metric is named generically (not prefix_cache_*)
// rather than being a prefix-caching-specific concern. using_kv_sync reflects this
// deployment's KV-sync configuration so the label still splits the same way it did when this
// counter (then prefix_cache_load_imbalance_total) was recorded separately by the plain and
// KV-sync prefix-cache routers.
func recordLoadImbalance(model string) {
	loadImbalanceEventsOnce.Do(func() {
		loadImbalanceEvents = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "load_imbalance_total",
				Help:      "Total number of requests where pod load was imbalanced",
			},
			[]string{"model", "using_kv_sync"},
		)
		if err := prometheus.Register(loadImbalanceEvents); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				klog.ErrorS(err, "failed to register load imbalance metric")
			}
		}
	})
	if loadImbalanceEvents != nil {
		loadImbalanceEvents.WithLabelValues(model, strconv.FormatBool(kvSyncEnabled())).Inc()
	}
}

func init() {
	Register(RouterLoadBalance, NewLoadBalanceRouter)
}

type loadBalanceRouter struct {
	cache cache.Cache
}

func NewLoadBalanceRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return &loadBalanceRouter{cache: c}, nil
}

// NewLoadBalanceRouterWithCache constructs load-balance with an explicit cache.
func NewLoadBalanceRouterWithCache(c cache.Cache) (types.Router, error) {
	return &loadBalanceRouter{cache: c}, nil
}

// Cache returns the cache this router reads running-request counts from, so a multi-strategy
// blend can break score ties with the same counts (see tieBreakCache in router.go).
func (r *loadBalanceRouter) Cache() cache.Cache { return r.cache }

// ScoreAll returns the effective load of each pod (see loadBalanceScore):
//
//	(running + λ·queued) / EWMA(output tokens/sec) × (1 + α·(1−kvFree)²)
//
// Lower means the pod has more headroom for this request. A pod below the critical free-KV
// fraction scores +Inf so it is passed over until it recovers.
//
// Running requests come from GetPodsRunningRequests (the live cross-gateway count), not
// GetMetricValueByPod(RealtimeNumRequestsRunning): that metric slot is a periodically
// synced cache and, between scrape ticks, only reflects this gateway's local view.
func (r *loadBalanceRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	counts, err := ctx.RunningRequests(pods, r.cache.GetPodsRunningRequests)
	scores, scored := r.scoreWithCounts(ctx, pods, counts, err)
	return scores, scored, nil
}

// CandidateScores implements types.CandidateScorer: the ScoreAll formula with the running
// counts taken from memory (this gateway's own counts plus the last gateway snapshot) instead
// of Redis. The capacity and KV inputs are scraped metrics either way. It does not go through
// ctx.RunningRequests, so these estimates never stand in for the exact counts later readers of
// the same decision ask for.
func (r *loadBalanceRouter) CandidateScores(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	counts, ok := routeutil.ApproxRunningRequests(r.cache, readyPodList.All())
	if !ok {
		return nil, nil, routeutil.ErrNoRunningRequestsEstimate
	}
	scores, scored := r.scoreWithCounts(ctx, readyPodList.All(), counts, nil)
	return scores, scored, nil
}

func (r *loadBalanceRouter) scoreWithCounts(ctx *types.RoutingContext, pods []*v1.Pod, counts map[string]int64, err error) ([]float64, []bool) {
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))
	capacities := r.capacities(pods)

	// The request's resolved overrides carry the profile's values on top of
	// the process defaults (see ResolveRoutingOverrides).
	lb := ctx.RoutingOverrides().LoadBalance
	queuedWeight := lb.QueuedWeight
	kvPressureAlpha := lb.KVPressureAlpha
	kvCriticalFree := lb.KVCriticalFree

	for i, pod := range pods {
		running := 0.0
		if err == nil && counts != nil {
			running = float64(counts[utils.GeneratePodKey(pod.Namespace, pod.Name)])
		}
		load := running
		if queuedWeight > 0 {
			queued := routeutil.GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, ctx.Model, metrics.NumRequestsWaiting)
			load += queuedWeight * queued
		}
		kvFree := r.kvFreeFraction(ctx, pod)
		if math.IsNaN(kvFree) {
			scores[i] = math.Inf(1)
		} else {
			scores[i] = loadBalanceScore(load, capacityDivisor(capacities[i]), kvFree, kvPressureAlpha, kvCriticalFree)
		}
		scored[i] = true

		if klog.V(4).Enabled() {
			klog.V(4).InfoS("load_balance_score",
				"request_id", ctx.RequestID,
				"pod", pod.Name,
				"running_requests", running,
				"capacity", capacities[i],
				"capacity_exponent", effectiveCapacityExponent(),
				"kv_free", formatLoadBalanceScore(kvFree),
				"score", formatLoadBalanceScore(scores[i]))
		}
	}

	return scores, scored
}

// loadBalanceScore is the effective load of a replica: the work already committed to it, divided by
// how fast it drains work, inflated as its KV cache fills. capacity is the divisor (see
// capacityDivisor) and kvFree is in [0, 1]. Below the critical free-KV fraction the replica is unusable and the score is +Inf.
// The penalty strength and that fraction are the environment defaults unless the request's
// model config profile overrides them (see types.RoutingOverrides).
//
// That +Inf only guarantees exclusion when load-balance is the router actually selecting the pod
// (loadBalanceRouter.Route). When load-balance is one voice in a multi-strategy blend (its default
// mode — see appendLoadBalanceBlend), multiStrategyRouter.normalizeScoresArray treats +Inf the same
// as "not scored" and maps it to a plain 0 for load-balance's weighted component: a strong penalty,
// not an exclusion. Another blended strategy that scores the same pod highly (e.g. a prefix-cache
// hit) can still win the route for it.
func loadBalanceScore(load, capacity, kvFree, kvPressureAlpha, kvCriticalFree float64) float64 {
	if kvFree < kvCriticalFree {
		return math.Inf(1)
	}
	kvUsed := 1 - kvFree
	return load / capacity * (1 + kvPressureAlpha*kvUsed*kvUsed)
}

// formatLoadBalanceScore makes a score safe for klog's JSON formatter, which cannot
// encode +Inf (the KV-guardrail score).
func formatLoadBalanceScore(score float64) any {
	if math.IsInf(score, 0) || math.IsNaN(score) {
		return strconv.FormatFloat(score, 'g', -1, 64)
	}
	return score
}

// Polarity indicates lower effective load is better.
func (r *loadBalanceRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

// Route selects the pod with minimum effective load. Ties are broken using least combined
// GPU+CPU KV-cache usage (falling back to random when cache metrics are unavailable), the
// same way prefix-cache breaks ties in prefix-match percentage using request count.
//
// The load-imbalance hotspot gate (ApplyLoadImbalanceGate) is not applied here: the gateway
// applies it once, centrally, ahead of whichever strategy actually routes the request (see
// gateway.go's selectTargetPod), so readyPodList arrives already narrowed when load is severely
// skewed. Applying it again here would double the metric-fetch work and double-count the
// load_imbalance_total counter for every request.
func (r *loadBalanceRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", ErrorNoAvailablePod
	}

	scores, _, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	minScore := math.Inf(1)
	var candidates []*v1.Pod
	for i, pod := range pods {
		s := scores[i]
		if s < minScore {
			minScore = s
			candidates = []*v1.Pod{pod}
		} else if s == minScore {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) == 0 {
		// Every pod is past the KV guardrail (+Inf). Refusing to route would turn cluster-wide
		// KV pressure into an outage, so fall back to the tie-break below over all pods, which
		// picks the one with the most KV headroom.
		candidates = pods
	}

	// Reuse the least-kv-cache strategy's own scorer to break the tie, rather than a
	// bespoke lookup: any registered types.PodScorer works here as a tie-breaker.
	targetPod, err := routeutil.RouteByScore(ctx, &utils.PodArray{Pods: candidates}, least.NewLeastKvCacheScorer(r.cache))
	if err != nil {
		return "", err
	}

	if klog.V(4).Enabled() {
		klog.V(4).InfoS("load_balance_selected",
			"request_id", ctx.RequestID,
			"target_pod", targetPod.Name,
			"score", formatLoadBalanceScore(minScore))
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

// capacities returns each pod's capacity: the output tokens per second of one request's time on
// the pod (RealtimeOutputTokensPerRequestSecondEWMA), or the pod's total output tokens per second
// when AIBRIX_LOAD_BALANCE_CAPACITY_SIGNAL selects it (see capacityEstimate). The gateway learns
// relative hardware speed from this rather than from pod labels, so a mixed B40/A100/H20 pool
// needs no configuration.
//
// The per-request rate does not grow with the share of traffic a pod receives. The total rate
// does: below saturation a pod that is sent more requests completes more tokens, looks faster
// and is sent more still (#3450).
//
// A pod with no positive estimate yet (just started, or no completed output tokens in the last
// window) is given the mean of the pods that have one, so it competes as an average replica. A
// fixed fallback such as 1.0 would be off by the tokens/sec scale (thousands) against measured pods
// and shut the new pod out of the traffic it needs to be measured. With no estimate anywhere every
// pod gets 1.0, which makes the score plain load × KV penalty.
//
// The same uniform 1.0 applies while any candidate is still warming up (see
// capacityWarmingUp): its estimate covers less than one rate window, so it is not yet comparable
// with the other pods' estimates.
func (r *loadBalanceRouter) capacities(pods []*v1.Pod) []float64 {
	caps := make([]float64, len(pods))
	if r.capacityWarmingUp(pods, time.Now()) {
		for i := range caps {
			caps[i] = 1.0
		}
		return caps
	}
	now := time.Now()
	sum, known := 0.0, 0
	for i, pod := range pods {
		if rate, ok := r.capacityEstimate(pod, now); ok {
			caps[i] = rate
			sum += rate
			known++
		}
	}

	fallback := 1.0
	if known > 0 {
		fallback = sum / float64(known)
	}
	for i := range caps {
		if caps[i] == 0 {
			caps[i] = fallback
		}
	}
	return caps
}

const (
	loadBalanceCapacitySignalEnv = "AIBRIX_LOAD_BALANCE_CAPACITY_SIGNAL"
	// capacitySignalPerRequest reads RealtimeOutputTokensPerRequestSecondEWMA: output tokens per
	// second of one request's time on the pod. It does not grow with the share of traffic a pod
	// receives (#3450).
	capacitySignalPerRequest = "per-request"
	// capacitySignalTotal reads RealtimeOutputTokenRateEWMA: the pod's total output tokens per
	// second, the signal before #3491. Below saturation it is the traffic the pod receives.
	capacitySignalTotal = "total"

	// loadBalanceCapacityMaxAgeDefault is how old a per-request estimate may be. The metric keeps its
	// last value while a pod completes too few requests to update it (an idle pod, or one below
	// AIBRIX_OUTPUT_RATE_MIN_SAMPLES requests per window), so an older estimate describes a
	// load the pod no longer has. Such a pod is scored like one without an estimate. It is the
	// length of the history the estimate is computed from (window1mHistoryMaxAge in pkg/cache).
	loadBalanceCapacityMaxAgeDefault = 3 * time.Minute
)

// loadBalanceCapacityMaxAge is loadBalanceCapacityMaxAgeDefault; a variable so tests can shorten it.
var loadBalanceCapacityMaxAge = loadBalanceCapacityMaxAgeDefault

// loadBalanceCapacitySignal selects the metric load-balance divides a pod's load by.
var loadBalanceCapacitySignal = loadLoadBalanceCapacitySignal()

const (
	loadBalanceCapacityExponentEnv = "AIBRIX_LOAD_BALANCE_CAPACITY_EXPONENT"
	// With the per-request rate as capacity, the score is load / capacity^2.
	//
	// load / capacity alone sends a slow replica a request whenever the fast one carries
	// capacity-ratio times as many, which keeps the slow replica busy at its own pace. On a pair
	// where one replica was about 4.5x faster per request, that cost 16-24% of the throughput
	// the total-rate signal gave, and put the slow replica's latency into the p95 (#3491).
	//
	// The total-rate signal did better there because a pod's total rate is its requests in
	// flight times its per-request rate: the speed is in it twice, once through how many
	// requests the pod was sent. Squaring the per-request rate gives the same preference for the
	// faster replica without the traffic a pod happened to receive in it. Measured on that pair
	// with 4, 8 and 16 clients, load / capacity^2 completed 22-33% more requests than the total
	// rate did the same day, and two identical replicas stayed within 47-53% of each other.
	//
	// Replicas with equal estimates score by load alone at any exponent.
	loadBalanceCapacityExponentDefault = 2.0
)

// loadBalanceCapacityExponent is the exponent applied to the per-request capacity. It is not
// applied to the total-rate signal (see effectiveCapacityExponent).
var loadBalanceCapacityExponent = utils.LoadEnvFloat(loadBalanceCapacityExponentEnv, loadBalanceCapacityExponentDefault)

// effectiveCapacityExponent is the exponent in use: loadBalanceCapacityExponent for the
// per-request signal, and 1 for the total-rate signal, which is scored as it was before #3491.
func effectiveCapacityExponent() float64 {
	if loadBalanceCapacitySignal == capacitySignalTotal {
		return 1
	}
	return loadBalanceCapacityExponent
}

// capacityDivisor is what a pod's load is divided by: its capacity raised to
// effectiveCapacityExponent.
func capacityDivisor(capacity float64) float64 {
	if exponent := effectiveCapacityExponent(); exponent != 1 {
		return math.Pow(capacity, exponent)
	}
	return capacity
}

// loadLoadBalanceCapacitySignal reads loadBalanceCapacitySignalEnv: capacitySignalPerRequest
// unless it is set to capacitySignalTotal. Any other value is logged and ignored.
func loadLoadBalanceCapacitySignal() string {
	switch raw := os.Getenv(loadBalanceCapacitySignalEnv); raw {
	case "", capacitySignalPerRequest:
		return capacitySignalPerRequest
	case capacitySignalTotal:
		klog.Infof("set %s: %s, load-balance capacity is the pod's total output-token rate", loadBalanceCapacitySignalEnv, raw)
		return capacitySignalTotal
	default:
		klog.Warningf("invalid %s: %q, using %s", loadBalanceCapacitySignalEnv, raw, capacitySignalPerRequest)
		return capacitySignalPerRequest
	}
}

// capacityEstimate returns the pod's capacity from the selected signal. ok is false when the
// pod has no positive estimate, or, for the per-request signal, when the estimate was last
// updated more than loadBalanceCapacityMaxAge ago. A cache that cannot report when a value was
// updated (it does not implement cache.MetricRecordCache) is trusted as it is.
func (r *loadBalanceRouter) capacityEstimate(pod *v1.Pod, now time.Time) (rate float64, ok bool) {
	if loadBalanceCapacitySignal == capacitySignalTotal {
		v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeOutputTokenRateEWMA)
		if err != nil || v == nil {
			return 0, false
		}
		rate = v.GetSimpleValue()
		return rate, rate > 0
	}
	if records, hasRecords := r.cache.(cache.MetricRecordCache); hasRecords {
		record, err := records.GetMetricRecordByPod(pod.Name, pod.Namespace, metrics.RealtimeOutputTokensPerRequestSecondEWMA)
		if err != nil || record.Value == nil {
			return 0, false
		}
		if !record.Timestamp.IsZero() && now.Sub(record.Timestamp) > loadBalanceCapacityMaxAge {
			return 0, false
		}
		rate = record.Value.GetSimpleValue()
		return rate, rate > 0
	}
	v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeOutputTokensPerRequestSecondEWMA)
	if err != nil || v == nil {
		return 0, false
	}
	rate = v.GetSimpleValue()
	return rate, rate > 0
}

const (
	loadBalanceCapacityWarmupEnv = "AIBRIX_LOAD_BALANCE_CAPACITY_WARMUP"
	// One full window of the rate behind RealtimeOutputTokenRateEWMA (window1mTarget in
	// pkg/cache/utils.go). Keep the two in step.
	loadBalanceCapacityWarmupDefault = time.Minute
)

// loadBalanceCapacityWarmup is how long after a pod becomes routable its capacity estimate is
// not trusted. Until a pod has a full window of history, its output-token rate is divided by the
// time since its own first snapshot, not by the window. Pods that became routable at different
// times therefore divide the same traffic by different lengths of time, and the pod with the
// shorter history looks faster, attracts more traffic, and its estimate then grows with the
// traffic it gets. In #3450 a split set in the first ~10 s after a rollout held at 73:27. Once
// every candidate has a full window the estimates are comparable. On by default; 0 disables it.
//
// The per-request rate does not have that dependence on the length of a pod's history, but the
// warm-up stays: a fresh engine's first requests are slower than its steady state, so its first
// estimates read low. In the scale-out runs on #3491 a pod added under load reached an even
// share in its first minute with the warm-up and stayed 2-3 points under it without.
var loadBalanceCapacityWarmup = loadLoadBalanceCapacityWarmup()

// loadLoadBalanceCapacityWarmup reads loadBalanceCapacityWarmupEnv. utils.LoadEnvDuration rejects
// non-positive values and falls back to the (positive) default, so an explicit "0" has to be
// handled here for it to be an off switch (same as least-request's ramp window).
func loadLoadBalanceCapacityWarmup() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(loadBalanceCapacityWarmupEnv)); err == nil && d == 0 {
		klog.Infof("set %s: 0, load-balance capacity warm-up disabled", loadBalanceCapacityWarmupEnv)
		return 0
	}
	return utils.LoadEnvDuration(loadBalanceCapacityWarmupEnv, loadBalanceCapacityWarmupDefault)
}

// capacityWarmingUp reports whether any of pods became routable less than
// loadBalanceCapacityWarmup ago. It uses the readySince the cache keeps for
// LeastRequestTopKRouter's ramp, so a cache without it (cache.PodReadySinceProvider), or a pod it
// has no time for, counts as warm.
//
// The window lasts while any candidate is younger than it, so across a rolling update or a
// scale-up ramp that is the whole rollout plus one window, and a mixed-hardware pool routes
// without the capacity term for that long.
//
// LatestPodReadySince keeps the per-pod read off the hot path once the whole fleet is warm. It
// is the newest stamp in the whole cache, so a pod becoming routable for any model sends every
// call down the per-pod read for one window.
func (r *loadBalanceRouter) capacityWarmingUp(pods []*v1.Pod, now time.Time) bool {
	window := loadBalanceCapacityWarmup
	if window <= 0 {
		return false
	}
	provider, ok := r.cache.(cache.PodReadySinceProvider)
	if !ok {
		return false
	}
	nowNanos := now.UnixNano()
	if latest := provider.LatestPodReadySince(); latest == 0 || time.Duration(nowNanos-latest) >= window {
		return false
	}
	readySince, err := provider.GetPodsReadySince(pods)
	if err != nil {
		return false
	}
	for _, pod := range pods {
		rs := readySince[utils.GeneratePodKey(pod.Namespace, pod.Name)]
		if rs != 0 && time.Duration(nowNanos-rs) < window {
			return true
		}
	}
	return false
}

// kvFreeFraction returns free KV in [0, 1], or NaN as the internal stale-record
// sentinel. Never-reported KV and invalid NaN readings remain neutral (fully free).
// ScoreAll converts the stale sentinel to +Inf, preferring fresh peers while retaining
// the existing all-candidates fallback.
func (r *loadBalanceRouter) kvFreeFraction(ctx *types.RoutingContext, pod *v1.Pod) float64 {
	usage, ok, reason := routeutil.ReadKVUsage(r.cache, pod, ctx.Model, metrics.KVCacheUsagePerc, time.Now(), cache.RoutingMetricStaleThreshold())
	if reason == "stale" {
		// NaN is an internal sentinel; ScoreAll emits +Inf, including when the guardrail is disabled.
		return math.NaN()
	}
	if !ok {
		return 1.0
	}
	if math.IsNaN(usage) {
		return 1.0
	}
	return math.Min(1, math.Max(0, 1-usage))
}

// ApplyLoadImbalanceGate narrows readyPods to the least-loaded subset when running-request load
// is severely skewed (see getTargetPodListOnLoadImbalance). Returns readyPods unchanged if not
// imbalanced.
//
// It is exported so the gateway can apply this hotspot safeguard once, centrally, ahead of
// whichever routing strategy a request actually resolves to — including "load-balance" itself,
// whose own Route() relies on the caller having already applied this gate rather than calling it
// again. Callers should skip invoking this for exclusive strategies (pd, slo*), which manage
// their own pod subsets and would have their role split disrupted by a blanket
// running-request-count filter applied ahead of time.
func ApplyLoadImbalanceGate(ctx *types.RoutingContext, c cache.Cache, readyPods []*v1.Pod) []*v1.Pod {
	if c == nil || len(readyPods) < 2 {
		return readyPods
	}

	// The request's resolved overrides carry the profile's values on top of
	// the process defaults (see ResolveRoutingOverrides).
	lb := ctx.RoutingOverrides().LoadBalance
	imbalanceFactor := lb.ImbalanceFactor
	imbalanceMinGap := lb.ImbalanceMinGap

	podRequestCount := routeutil.RequestCounts(ctx, c, readyPods)
	leastPods, minValue, maxValue, imbalanced := getTargetPodListOnLoadImbalance(podRequestCount, readyPods, imbalanceFactor, imbalanceMinGap)
	if !imbalanced {
		return readyPods
	}

	recordLoadImbalance(ctx.Model)
	if klog.V(4).Enabled() {
		selected := make([]string, 0, len(leastPods))
		selectedSet := make(map[string]struct{}, len(leastPods))
		for _, pod := range leastPods {
			podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
			selected = append(selected, podKey)
			selectedSet[podKey] = struct{}{}
		}
		skipped := make([]string, 0, len(readyPods)-len(leastPods))
		for _, pod := range readyPods {
			podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
			if _, ok := selectedSet[podKey]; !ok {
				skipped = append(skipped, podKey)
			}
		}
		klog.V(4).InfoS("load_balance_imbalance_gate",
			"request_id", ctx.RequestID,
			"restricted_pod_count", len(leastPods),
			"total_pod_count", len(readyPods),
			"min_running_requests", minValue,
			"max_running_requests", maxValue,
			"factor", podRunningRequestImbalanceFactor,
			"min_gap", podRunningRequestImbalanceMinGap,
			"selected_pods", selected,
			"skipped_pods", skipped)
	}
	return leastPods
}

// getTargetPodListOnLoadImbalance returns the least-loaded pods when load is severely skewed.
// The absolute gap must be at least podRunningRequestImbalanceMinGap. With two pods, the
// absolute gap alone determines whether the gate triggers. With three or more pods, the
// busiest count must also exceed podRunningRequestImbalanceFactor*(meanOfOthers+1), where
// meanOfOthers excludes one busiest pod. This keeps the relative threshold tied
// to peer load as that pod's own count rises.
//
// TODO: "load" here is raw running-request count, with no notion of each pod's capacity. In a
// heterogeneous pool this conflates "carrying a lot of work" with "overloaded": a pod that is
// simply faster than its peers is expected to carry more concurrent requests without being more
// loaded (see loadBalanceScore's capacity normalization), but this gate can still flag it as a
// hotspot and exclude it from every strategy's candidate set before any strategy's
// capacity-aware scoring gets a chance to run. Move this to compare capacity-normalized load
// (running/capacity, rescaled to the pool's mean capacity so the existing factor/min-gap
// thresholds keep their current "requests" units and defaults) instead of raw counts.
func getTargetPodListOnLoadImbalance(podRequestCount map[string]int, readyPods []*v1.Pod, imbalanceFactor float64, imbalanceMinGap int) (targetPodList []*v1.Pod, minValue, maxValue int, imbalanced bool) {
	n := len(podRequestCount)
	if n == 0 {
		return nil, 0, 0, false
	}

	minValue = -1
	var sumOfOthers uint64
	needsWideSum := false
	for _, v := range podRequestCount {
		if minValue < 0 || v < minValue {
			minValue = v
		}
		// Keep one maximum out of the sum and round only at the final comparison.
		other := min(v, maxValue)
		maxValue = max(v, maxValue)
		// Routing expects non-negative running-request counts.
		// Guard the uint64 conversion if a caller supplies a negative count.
		if other < 0 || uint64(other) > math.MaxUint64-sumOfOthers {
			needsWideSum = true
		} else {
			sumOfOthers += uint64(other)
		}
	}

	if maxValue-minValue < imbalanceMinGap {
		return nil, minValue, maxValue, false
	}
	if n > 2 {
		otherTotal := float64(sumOfOthers)
		if needsWideSum {
			var sum, term big.Int
			for _, v := range podRequestCount {
				sum.Add(&sum, term.SetInt64(int64(v)))
			}
			sum.Sub(&sum, term.SetInt64(int64(maxValue)))
			otherTotal, _ = sum.Float64()
		}
		meanOfOthers := otherTotal / float64(n-1)
		if float64(maxValue) <= imbalanceFactor*(meanOfOthers+1) {
			return nil, minValue, maxValue, false
		}
	}

	podByKey := routeutil.PodsByKey(readyPods)
	for podKey, v := range podRequestCount {
		if v == minValue {
			if pod := podByKey[podKey]; pod != nil {
				targetPodList = append(targetPodList, pod)
			}
		}
	}
	return targetPodList, minValue, maxValue, len(targetPodList) > 0
}
