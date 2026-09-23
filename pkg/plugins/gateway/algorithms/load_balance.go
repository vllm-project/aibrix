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
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
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
// env var prefix_cache.go's NewPrefixCacheRouter reads. The value is a process-wide
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
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	counts, err := r.cache.GetPodsRunningRequests(pods)
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
			queued := GetPodModelMetricsSimpleValue(r.cache, pod.Name, pod.Namespace, ctx.Model, metrics.NumRequestsWaiting)
			load += queuedWeight * queued
		}
		kvFree := r.kvFreeFraction(ctx, pod)
		scores[i] = loadBalanceScore(load, capacities[i], kvFree, kvPressureAlpha, kvCriticalFree)
		scored[i] = true

		if klog.V(4).Enabled() {
			klog.V(4).InfoS("load_balance_score",
				"request_id", ctx.RequestID,
				"pod", pod.Name,
				"running_requests", running,
				"capacity", capacities[i],
				"kv_free", kvFree,
				"score", formatLoadBalanceScore(scores[i]))
		}
	}

	return scores, scored, nil
}

// loadBalanceScore is the effective load of a replica: the work already committed to it, divided by
// how fast it drains work, inflated as its KV cache fills. capacity is in tokens/sec and kvFree in
// [0, 1]. Below the critical free-KV fraction the replica is unusable and the score is +Inf.
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
	targetPod, err := RouteByScore(ctx, &utils.PodArray{Pods: candidates}, newLeastKvCacheRouter(r.cache))
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

// capacities returns each pod's capacity, the observed output tokens/sec (see
// RealtimeOutputTokenRateEWMA). The gateway learns relative hardware speed from this rather than
// from pod labels, so a mixed B40/A100/H20 pool needs no configuration.
//
// A pod with no positive estimate yet (just started, or no completed output tokens in the last
// window) is given the mean of the pods that have one, so it competes as an average replica. A
// fixed fallback such as 1.0 would be off by the tokens/sec scale (thousands) against measured pods
// and shut the new pod out of the traffic it needs to be measured. With no estimate anywhere every
// pod gets 1.0, which makes the score plain load × KV penalty.
func (r *loadBalanceRouter) capacities(pods []*v1.Pod) []float64 {
	caps := make([]float64, len(pods))
	sum, known := 0.0, 0
	for i, pod := range pods {
		v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeOutputTokenRateEWMA)
		if err != nil || v == nil {
			continue
		}
		if rate := v.GetSimpleValue(); rate > 0 {
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

// kvFreeFraction returns the pod's free KV-cache fraction in [0, 1]. A pod whose engine does not
// report KV usage is treated as fully free: no penalty and no guardrail, rather than penalizing
// engines that simply lack the metric. A NaN reading (e.g. 0/0 in the engine) is treated the same
// way: math.Min/Max propagate NaN, and a NaN free fraction would compare false against the
// guardrail and turn the pod's score into NaN, which never wins a comparison in Route.
func (r *loadBalanceRouter) kvFreeFraction(ctx *types.RoutingContext, pod *v1.Pod) float64 {
	v, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.KVCacheUsagePerc)
	if err != nil || v == nil {
		return 1.0
	}
	usage := v.GetSimpleValue()
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

	podRequestCount := getRequestCounts(c, readyPods)
	leastPods, minValue, maxValue, imbalanced := getTargetPodListOnLoadImbalance(podRequestCount, readyPods, imbalanceFactor, imbalanceMinGap)
	if !imbalanced {
		return readyPods
	}

	recordLoadImbalance(ctx.Model)
	if klog.V(4).Enabled() {
		selected := make([]string, 0, len(leastPods))
		selectedSet := make(map[string]struct{}, len(leastPods))
		for _, pod := range leastPods {
			selected = append(selected, pod.Name)
			selectedSet[pod.Name] = struct{}{}
		}
		skipped := make([]string, 0, len(readyPods)-len(leastPods))
		for _, pod := range readyPods {
			if _, ok := selectedSet[pod.Name]; !ok {
				skipped = append(skipped, pod.Name)
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
// The absolute gap must be at least podRunningRequestImbalanceMinGap. For 3+ pods the busiest
// pod must also exceed podRunningRequestImbalanceFactor*(meanOfOthers+1), where meanOfOthers
// excludes the busiest pod itself. Excluding it matters: folding the busiest pod's own count
// into the baseline it's compared against makes the baseline rise in lockstep as that pod gets
// busier, so the check keeps almost-but-not-quite firing (the busiest pod would need to reach
// roughly factor times the *combined* load of every other pod before triggering, not factor
// times a typical pod's load) — silently permitting severe, worsening imbalance. Two pods skip
// the factor check: with the default factor of 2, max > 2*(mean+1) reduces to max > max+2 and
// never fires, which would disable hotspot protection on the common 2-replica deployment.
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
	sum := 0
	for _, v := range podRequestCount {
		sum += v
		if minValue < 0 || v < minValue {
			minValue = v
		}
		if v > maxValue {
			maxValue = v
		}
	}

	if maxValue-minValue < imbalanceMinGap {
		return nil, minValue, maxValue, false
	}
	if n > 2 {
		meanOfOthers := float64(sum-maxValue) / float64(n-1)
		if float64(maxValue) <= imbalanceFactor*(meanOfOthers+1) {
			return nil, minValue, maxValue, false
		}
	}

	for podname, v := range podRequestCount {
		if v == minValue {
			pod, _ := utils.FilterPodByName(podname, readyPods)
			if pod != nil {
				targetPodList = append(targetPodList, pod)
			}
		}
	}
	return targetPodList, minValue, maxValue, len(targetPodList) > 0
}
