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
var (
	podRunningRequestImbalanceFactor = utils.LoadEnvFloat("AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR", 2.0)
	podRunningRequestImbalanceMinGap = utils.LoadEnvInt("AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP", 8)
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

// ScoreAll returns pending_time = request_count / capacity for each pod.
// Lower pending_time means the pod has more headroom to accept this request.
func (r *loadBalanceRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		reqCount := 0.0
		if v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning); err == nil && v != nil {
			reqCount = v.GetSimpleValue()
		}
		scores[i] = reqCount / r.capacityOf(pod)
		scored[i] = true
	}

	klog.V(4).InfoS("load_balance_scores",
		"request_id", ctx.RequestID,
		"model", ctx.Model,
		"pod_count", len(pods))

	return scores, scored, nil
}

// Polarity indicates lower pending_time is better.
func (r *loadBalanceRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

// Route selects the pod with minimum pending_time. Ties are broken using least combined
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

	minScore := math.MaxFloat64
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
		return "", ErrorNoAvailablePod
	}

	// Reuse the least-kv-cache strategy's own scorer to break the tie, rather than a
	// bespoke lookup: any registered types.PodScorer works here as a tie-breaker.
	targetPod, err := RouteByScore(ctx, &utils.PodArray{Pods: candidates}, newLeastKvCacheRouter(r.cache))
	if err != nil {
		return "", err
	}

	klog.V(4).InfoS("load_balance_selected",
		"request_id", ctx.RequestID,
		"target_pod", targetPod.Name,
		"pending_time", minScore)

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

// capacityOf returns the throughput capacity of a pod.
// Uses observed drain rate from cache when available; falls back to 1.0 (uniform).
func (r *loadBalanceRouter) capacityOf(pod *v1.Pod) float64 {
	if v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeRunningRequestsDrainRate1m); err == nil && v != nil {
		if dr := v.GetSimpleValue(); dr > 0 {
			return dr
		}
	}
	return 1.0
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

	podRequestCount := getRequestCounts(c, readyPods)
	leastPods, minValue, maxValue, imbalanced := getTargetPodListOnLoadImbalance(podRequestCount, readyPods)
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
func getTargetPodListOnLoadImbalance(podRequestCount map[string]int, readyPods []*v1.Pod) (targetPodList []*v1.Pod, minValue, maxValue int, imbalanced bool) {
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

	if maxValue-minValue < podRunningRequestImbalanceMinGap {
		return nil, minValue, maxValue, false
	}
	if n > 2 {
		meanOfOthers := float64(sum-maxValue) / float64(n-1)
		if float64(maxValue) <= podRunningRequestImbalanceFactor*(meanOfOthers+1) {
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
