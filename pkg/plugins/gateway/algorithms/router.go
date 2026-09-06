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
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterNotSet = ""
)

var (
	ErrInitTimeout           = errors.New("router initialization timeout")
	ErrFallbackNotSupported  = errors.New("router not support fallback")
	ErrFallbackNotRegistered = errors.New("fallback router not registered")
	defaultRM                = NewRouterManager()
)

// RouterItem represents a single routing algorithm and its weight coefficient for multi-router config.
type RouterItem struct {
	Name        string
	Coefficient int // Integer weight coefficient (0 to 1000000)
}

// MultiRouterConfig holds the parsed routing algorithms and their weight coefficients.
type MultiRouterConfig struct {
	Items []RouterItem
}

// ParseMultiRouterConfig parses a multi-router string into a MultiRouterConfig.
// Format example: "prefix-cache:2,least-latency:1,least-request"
// - Weight coefficients must be integers in range [0, 1000000].
// - Default weight coefficient is 1 if omitted.
// - Weight coefficient 0 means the routing algorithm should be skipped.
func ParseMultiRouterConfig(routerStr string) (*MultiRouterConfig, error) {
	if routerStr == "" {
		return nil, errors.New("empty routing algorithm")
	}

	parts := strings.Split(routerStr, ",")
	if len(parts) == 0 {
		return nil, errors.New("invalid routing algorithm format")
	}

	var items []RouterItem

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty algorithm name")
		}

		name := part
		coefInt := 1 // Default weight coefficient is 1

		if strings.Contains(part, ":") {
			subParts := strings.Split(part, ":")
			if len(subParts) != 2 {
				return nil, fmt.Errorf("invalid algorithm format: %s", part)
			}
			name = strings.TrimSpace(subParts[0])
			if name == "" {
				return nil, fmt.Errorf("empty algorithm name in: %s", part)
			}

			coefStr := strings.TrimSpace(subParts[1])
			parsedCoef, err := strconv.Atoi(coefStr)
			if err != nil {
				return nil, fmt.Errorf("invalid weight coefficient in: %s (must be an integer)", part)
			}
			if parsedCoef < 0 || parsedCoef > 1000000 {
				return nil, fmt.Errorf("weight coefficient out of bounds [0, 1000000] in: %s", part)
			}
			coefInt = parsedCoef
		}

		// If weight coefficient is 0, we skip this algorithm entirely
		if coefInt > 0 {
			items = append(items, RouterItem{Name: name, Coefficient: coefInt})
		}
	}

	if len(items) == 0 {
		return nil, errors.New("no valid algorithms found (all weight coefficients were 0)")
	}

	// Check for exclusive strategies (pd, slo*)
	// If found, we ignore other strategies and return just the exclusive one
	// If multiple exclusive strategies are found, the first one encountered wins
	for _, item := range items {
		if isExclusiveStrategyName(item.Name) {
			klog.V(4).Infof("Exclusive routing strategy '%s' found in multi-strategy config. Other strategies will be ignored.", item.Name)
			return &MultiRouterConfig{Items: []RouterItem{{Name: item.Name, Coefficient: 1}}}, nil
		}
	}

	return &MultiRouterConfig{Items: items}, nil
}

// isExclusiveStrategyName reports whether name is an exclusive routing strategy (pd, slo*)
// that manages its own pod subset and must not be blended with other strategies or have its
// pod list pre-narrowed by the load-imbalance gate. This is the single definition shared by
// ParseMultiRouterConfig, appendLoadBalanceBlend, and ResolveExclusiveStrategy (used by the
// gateway) so the set of exclusive strategies can't drift between call sites.
func isExclusiveStrategyName(name string) bool {
	return name == string(RouterPD) || strings.HasPrefix(name, "slo")
}

// ResolveExclusiveStrategy parses algStr and, if it resolves to a single exclusive strategy
// (pd, slo*), returns its name and true. algStr may itself already be a multi-strategy string
// that collapses to one exclusive item (e.g. "pd:1,least-request:1" resolves to "pd") — this
// lets callers detect exclusivity from the caller's raw, unparsed algorithm string rather than
// comparing it against the exclusive name directly.
func ResolveExclusiveStrategy(algStr string) (string, bool) {
	cfg, err := ParseMultiRouterConfig(algStr)
	if err != nil || len(cfg.Items) != 1 {
		return "", false
	}
	if !isExclusiveStrategyName(cfg.Items[0].Name) {
		return "", false
	}
	return cfg.Items[0].Name, true
}

// autoBlendLoadBalanceWeight/autoBlendLeastRequestWeight control the silent multi-strategy
// blend appendLoadBalanceBlend adds behind every request's chosen strategy. Setting the
// load-balance weight to 0 disables the whole feature, matching ParseMultiRouterConfig's own
// "weight 0 means skip" convention.
var (
	autoBlendLoadBalanceWeight  = utils.LoadEnvInt("AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT", 1)
	autoBlendLeastRequestWeight = utils.LoadEnvInt("AIBRIX_ROUTING_AUTO_BLEND_LEAST_REQUEST_WEIGHT", 1)
)

// autoBlendPrefixCacheWeight/autoBlendPrefixCacheLoadBalanceWeight set the default blend ratio
// between prefix-cache and load-balance specifically, used in place of the flat
// autoBlendLoadBalanceWeight when a caller requests plain "prefix-cache" with no explicit weight
// of its own (see the len(cfg.Items) == 1 branch in appendLoadBalanceBlend). 5:4 == 1.25:1: a
// deliberate, modest lean toward cache-affinity so it edges out load-balance on an exact-tie
// disagreement (see the multiStrategyRouter.Route tie-break comment) instead of falling to an
// arbitrary alphabetical pick, without giving it enough weight to keep steering traffic at a
// cache-warm pod once load-balance clearly disagrees — a stronger lean risks masking real
// congestion on a pod whose capacity/drain-rate estimate hasn't caught up yet under sustained
// concurrent load.
var (
	autoBlendPrefixCacheWeight            = utils.LoadEnvInt("AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_WEIGHT", 5)
	autoBlendPrefixCacheLoadBalanceWeight = utils.LoadEnvInt("AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_LOAD_BALANCE_WEIGHT", 4)
)

// maxCachedAlgorithmStrings bounds how many distinct algorithm-string keys
// RouterManager.multiRouterCache and unblendableLogged will retain. Both maps are keyed by the
// client-controlled routing-strategy string — which may embed an arbitrary weight coefficient
// even on an otherwise-fixed strategy name (e.g. "prefix-cache:12345") — and neither map is
// otherwise evicted except by a full Register/RegisterProvider/Init reset. Without a cap, a
// client varying that coefficient across requests could grow either map without bound. Once a
// map is at the cap, additional unique strings simply skip caching/log-dedup for that request
// rather than growing the map further.
var maxCachedAlgorithmStrings = utils.LoadEnvInt("AIBRIX_ROUTER_MAX_CACHED_ALGORITHM_STRINGS", 4096)

// mentionedAlgorithmNames returns the set of algorithm names appearing in algStr, regardless
// of weight coefficient — including ones explicitly weighted to 0. ParseMultiRouterConfig
// drops weight-0 items entirely (that's its "skip this algorithm" convention), so its output
// can't tell "caller explicitly disabled X" apart from "caller never mentioned X". This raw
// scan preserves that distinction for appendLoadBalanceBlend, which must not silently
// re-introduce a strategy the caller explicitly zeroed out. algStr is assumed to already be
// syntactically valid (checked via ParseMultiRouterConfig by the caller).
func mentionedAlgorithmNames(algStr string) map[string]bool {
	mentioned := make(map[string]bool)
	for _, part := range strings.Split(algStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		if idx := strings.Index(part, ":"); idx >= 0 {
			name = strings.TrimSpace(part[:idx])
		}
		if name != "" {
			mentioned[name] = true
		}
	}
	return mentioned
}

// appendLoadBalanceBlend silently expands algStr into a hidden multi-strategy composite that
// also scores pods on load-balance's capacity-aware pending_time, so no single strategy can
// keep steering traffic at an already-hot pod. least-request is appended alongside it (unless
// already present) purely so multi-port/data-parallel pod routing keeps working: the
// multi-strategy router's port selection only engages when "least-request" is one of the
// configured scorers (see setTargetPortIfNeeded) — except when prefix-cache is one of the
// caller's strategies: prefix-cache already accounts for pod load via the gateway's
// ApplyLoadImbalanceGate and its own stddev-based candidate filtering
// (getTargetPodFromMatchedPodsFromCounts), so also blending in least-request as a full scoring
// participant would double-count "current load" against prefix-cache's own cache-affinity
// signal (load-balance and least-request are both direct functions of the same running-request
// metric, so they aren't independent votes) and can override cache locality far more readily
// than intended. For prefix-cache, only load-balance is blended in.
//
// One tradeoff of that exception: a prefix-cache request against multi-port/data-parallel pods
// won't get setTargetPortIfNeeded's port selection, since that lookup requires least-request to
// be one of the configured scorers.
//
// cfg is algStr's own already-parsed config, passed in by the caller (Select) so algStr isn't
// parsed twice per request.
//
// algStr itself is never modified by this function, so ctx.Algorithm, response headers, and
// Validate() all continue to reflect exactly what the caller asked for — load-balance is never
// surfaced as something the caller chose or needs to know about.
//
// Returns ok=false when there's nothing to add: the blend is disabled, algStr already resolves
// to an exclusive strategy (pd, slo*) that manages its own pod selection and must not be
// blended with anything else, or algStr is already the standalone "load-balance" strategy
// (which must keep running its own Route(), not a blend).
func appendLoadBalanceBlend(algStr string, cfg *MultiRouterConfig) (string, bool) {
	if autoBlendLoadBalanceWeight <= 0 {
		return "", false
	}

	if len(cfg.Items) == 1 && (isExclusiveStrategyName(cfg.Items[0].Name) || cfg.Items[0].Name == string(RouterLoadBalance)) {
		// Exclusive strategies (pd, slo*) manage their own pod selection and must not be
		// blended. An explicit, standalone "load-balance" selection is also left alone: it
		// already runs load-balance's own Route() (pending-time minimization + KV-cache
		// tie-break), so blending in least-request here would silently replace that with
		// generic weighted soft-scoring instead.
		return "", false
	}

	// Use the raw mention set, not cfg.Items, so an explicit "load-balance:0" or
	// "least-request:0" (caller opting out) isn't mistaken for "never mentioned" and
	// silently re-added — cfg.Items has already dropped weight-0 entries by this point.
	mentioned := mentionedAlgorithmNames(algStr)

	includesPrefixCache := false
	for _, item := range cfg.Items {
		if item.Name == string(RouterPrefixCache) {
			includesPrefixCache = true
			break
		}
	}

	// A bare "prefix-cache" request (no explicit weight, nothing else blended in) gets the
	// dedicated prefix-cache/load-balance ratio instead of the flat autoBlendLoadBalanceWeight
	// append: with only one item, the caller's own coefficient carries no information before
	// blending, so rewriting it here doesn't discard anything the caller expressed.
	prefixCacheOnly := len(cfg.Items) == 1 && includesPrefixCache

	blended := algStr
	if !mentioned[string(RouterLoadBalance)] {
		if prefixCacheOnly {
			blended = fmt.Sprintf("%s:%d,%s:%d", RouterPrefixCache, autoBlendPrefixCacheWeight, RouterLoadBalance, autoBlendPrefixCacheLoadBalanceWeight)
		} else {
			blended += fmt.Sprintf(",%s:%d", RouterLoadBalance, autoBlendLoadBalanceWeight)
		}
	}
	if !includesPrefixCache && !mentioned[string(RouterLeastRequest)] && autoBlendLeastRequestWeight > 0 {
		blended += fmt.Sprintf(",%s:%d", RouterLeastRequest, autoBlendLeastRequestWeight)
	}
	if blended == algStr {
		return "", false
	}
	return blended, true
}

// multiStrategyRouter coordinates multiple sub-routers and selects a pod via soft-scoring
type multiStrategyRouter struct {
	config  *MultiRouterConfig
	scorers map[string]types.PodScorer
}

// newMultiStrategyRouter initializes a new multiStrategyRouter based on the parsed config
func newMultiStrategyRouter(config *MultiRouterConfig, rm *RouterManager, ctx *types.RoutingContext) (*multiStrategyRouter, error) {
	scorers := make(map[string]types.PodScorer)

	for _, item := range config.Items {
		rm.routerMu.RLock()
		provider, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]
		rm.routerMu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("strategy %s not registered", item.Name)
		}

		router, err := provider(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize strategy %s: %v", item.Name, err)
		}

		scorer, ok := router.(types.PodScorer)
		if !ok {
			return nil, fmt.Errorf("strategy %s does not implement types.PodScorer interface", item.Name)
		}
		scorers[item.Name] = scorer
	}

	return &multiStrategyRouter{
		config:  config,
		scorers: scorers,
	}, nil
}

// Route executes the multi-strategy scoring pipeline and returns the best pod IP
func (m *multiStrategyRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", errors.New("empty pod list")
	}

	if len(pods) == 1 {
		ctx.SetTargetPod(pods[0])
		m.setTargetPortIfNeeded(ctx, readyPodList, pods[0])
		m.runPostRouteUpdates(ctx, readyPodList, pods[0])
		return ctx.TargetAddress(), nil
	}

	topPod, _, err := m.scoreAndRank(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	// Store target pod for updating cache if needed
	ctx.SetTargetPod(topPod)
	m.setTargetPortIfNeeded(ctx, readyPodList, topPod)
	m.runPostRouteUpdates(ctx, readyPodList, topPod)

	return ctx.TargetAddress(), nil
}

func (m *multiStrategyRouter) setTargetPortIfNeeded(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) {
	if !isMultiPortPods(readyPodList.All()) {
		return
	}
	scorer, ok := m.scorers[string(RouterLeastRequest)]
	if !ok {
		return
	}
	leastRequest, ok := scorer.(*leastRequestRouter)
	if !ok {
		return
	}
	if port := selectTargetPortForPodWithLeastRequestCount(leastRequest.cache, targetPod, readyPodList.ListPortsForPod()); port != 0 {
		ctx.SetTargetPort(port)
	}
}

func (m *multiStrategyRouter) runPostRouteUpdates(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) {
	for _, item := range m.config.Items {
		scorer := m.scorers[item.Name]
		updater, ok := scorer.(types.PostRouteUpdater)
		if !ok {
			continue
		}
		if err := updater.PostRouteUpdate(ctx, readyPodList, targetPod); err != nil {
			klog.Warningf("post-route update for strategy %s failed: %v", item.Name, err)
		}
	}
}

// scoreAndRank calculates final scores for all pods and returns the winner
func (m *multiStrategyRouter) scoreAndRank(ctx *types.RoutingContext, readyPodList types.PodList) (*v1.Pod, map[*v1.Pod]float64, error) {
	pods := readyPodList.All()
	finalScores := make(map[*v1.Pod]float64)
	logEnabled := klog.V(4).Enabled()

	// To collect diagnostic info for klog
	type podDiag struct {
		StrategyLog []string
	}
	var diags map[*v1.Pod]*podDiag
	if logEnabled {
		diags = make(map[*v1.Pod]*podDiag, len(pods))
		for _, pod := range pods {
			diags[pod] = &podDiag{}
		}
	}

	// Calculate total weight to act as denominator
	totalWeight := 0.0
	for _, item := range m.config.Items {
		totalWeight += float64(item.Coefficient)
	}

	if totalWeight <= 0 {
		return nil, nil, errors.New("total weight must be greater than zero")
	}

	// Iterate over each sub-strategy
	for _, item := range m.config.Items {
		scorer := m.scorers[item.Name]

		// 1. Collect raw scores
		scores, scored, err := scorer.ScoreAll(ctx, readyPodList)
		if err != nil {
			klog.Warningf("Strategy %s failed to score: %v", item.Name, err)
			scored = make([]bool, len(pods)) // all false
			scores = make([]float64, len(pods))
		}

		// 2. Normalize to [0, 1] based on polarity
		normScores := m.normalizeScoresArray(scores, scored, scorer.Polarity())

		// 3. Aggregate into final sum
		weightFraction := float64(item.Coefficient) / totalWeight
		for i, pod := range pods {
			weightedScore := normScores[i] * weightFraction
			finalScores[pod] += weightedScore

			if logEnabled {
				// Record diagnostic information for this pod and strategy
				rawScoreStr := "N/A"
				if scored[i] {
					rawScoreStr = fmt.Sprintf("%.2f", scores[i])
				}
				diagStr := fmt.Sprintf("%s(raw:%s, norm:%.2f, weight:%.3f)", item.Name, rawScoreStr, normScores[i], weightedScore)
				diags[pod].StrategyLog = append(diags[pod].StrategyLog, diagStr)
			}
		}
	}

	// 4. Find the pod with the highest score
	var topPods []*v1.Pod
	maxScore := -1.0

	for _, pod := range pods {
		score := finalScores[pod]
		// handle floating point precision
		if math.Abs(score-maxScore) < 1e-9 {
			topPods = append(topPods, pod)
		} else if score > maxScore {
			maxScore = score
			topPods = []*v1.Pod{pod}
		}
	}

	if len(topPods) == 0 {
		return nil, nil, errors.New("no valid target pod found after scoring")
	}

	// Tie-break: select deterministically by pod name. topPods' order otherwise reflects
	// readyPodList's underlying order, which traces back to Go map iteration over the pod
	// registry (see pkg/utils/registry.go) and gets reshuffled whenever that cache is
	// invalidated — including on ordinary pod-object update churn between requests, not just
	// membership changes. Without a deterministic tie-break, a request whose scores legitimately
	// tie across two pods (e.g. once prefix-cache match and load-balance load both equalize) can
	// flip its winner from one call to the next for reasons unrelated to load or cache affinity,
	// causing spurious reroutes/thrashing instead of a stable pick.
	if len(topPods) > 1 {
		sort.Slice(topPods, func(i, j int) bool { return topPods[i].Name < topPods[j].Name })
	}
	winner := topPods[0]

	// 5. Log the routing decision and all candidate metrics to klog
	if logEnabled {
		c, cacheErr := cache.Get()
		var logBuilder strings.Builder
		fmt.Fprintf(&logBuilder, "Multi-strategy routing decision for request [%s]. Selected target pod: [%s]. Candidate pod details:\n", ctx.RequestID, winner.Name)
		for _, pod := range pods {
			winnerFlag := " "
			if pod.Name == winner.Name {
				winnerFlag = "*"
			}
			outstandingStr := "N/A"
			if cacheErr == nil {
				if v, err := c.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning); err == nil && v != nil {
					outstandingStr = fmt.Sprintf("%.0f", v.GetSimpleValue())
				}
			}
			fmt.Fprintf(&logBuilder, "  [%s] Pod: %-30s | FinalScore: %.4f | Outstanding: %-4s | Details: %s\n",
				winnerFlag, pod.Name, finalScores[pod], outstandingStr, strings.Join(diags[pod].StrategyLog, ", "))
		}
		klog.Info(logBuilder.String())
	}

	return winner, finalScores, nil
}

// madOutlierMultiplier bounds how far (in median-absolute-deviations) a raw score may sit
// from the median before winsorizeClip clamps it. 3 MADs is the conventional "extreme outlier"
// threshold (roughly equivalent to ~4 standard deviations for a normal distribution, but robust
// to the outlier itself skewing the estimate the way a stddev computed on the same data would).
const madOutlierMultiplier = 3.0

// winsorizeClip clamps any value more than madOutlierMultiplier median-absolute-deviations from
// the median to that bound, returning a new slice (the input is left untouched). Skipped for
// fewer than 3 values, where a robust center/spread isn't meaningful, and when mad==0 (every
// value is identical, so there's no spread to judge outliers against).
func winsorizeClip(values []float64) []float64 {
	n := len(values)
	if n < 3 {
		return values
	}

	median := medianOf(values)
	absDevs := make([]float64, n)
	for i, v := range values {
		absDevs[i] = math.Abs(v - median)
	}
	mad := medianOf(absDevs)
	if mad == 0 {
		return values
	}

	lower := median - madOutlierMultiplier*mad
	upper := median + madOutlierMultiplier*mad

	clipped := make([]float64, n)
	for i, v := range values {
		switch {
		case v > upper:
			clipped[i] = upper
		case v < lower:
			clipped[i] = lower
		default:
			clipped[i] = v
		}
	}
	return clipped
}

func medianOf(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}

// normalizeScoresArray converts raw scores to [0,1] via min-max scaling, after first
// winsorizing outliers (see winsorizeClip). Plain min-max scaling let one pod's extreme,
// unrelated raw value (e.g. a collapsed drain-rate on an otherwise-unrelated pod) stretch the
// comparison scale for every other pod, compressing a real, meaningful gap between two other
// pods down to almost nothing — e.g. with raw pending-times {91, 333, 4534.67}, min-max scored
// 333 as 0.95 (nearly best) purely because 4534.67 was in the pool, masking that it was
// actually carrying 3.7x the load of the 91 pod. Winsorizing first clamps 4534.67 to a robust
// bound before scaling, so the outlier can no longer set the scale's max — while ordinary,
// non-pathological distributions (no value far from the median) are left untouched, preserving
// magnitude-sensitive scoring for cases like a genuine proportional "sweet spot" pod that isn't
// the best on any single strategy but is consistently close to the best on all of them.
func (m *multiStrategyRouter) normalizeScoresArray(scores []float64, scored []bool, polarity types.Polarity) []float64 {
	normScores := make([]float64, len(scores))

	// Collect scored, finite raw values (and their original indices) to winsorize before
	// scaling, so one outlier can't stretch the min-max range for everyone else.
	var indices []int
	var values []float64
	for i, isScored := range scored {
		if isScored && isFiniteScore(scores[i]) {
			indices = append(indices, i)
			values = append(values, scores[i])
		}
	}

	if len(values) == 0 {
		return normScores // all 0.0
	}

	clipped := winsorizeClip(values)

	minVal := clipped[0]
	maxVal := clipped[0]
	for _, v := range clipped[1:] {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	for j, i := range indices {
		if maxVal == minVal {
			normScores[i] = 1.0 // all (post-clip) scored pods get max score if there's no difference
			continue
		}

		if polarity == types.PolarityLeast {
			// Reverse score if smaller is better: (max - s) / (max - min)
			normScores[i] = (maxVal - clipped[j]) / (maxVal - minVal)
		} else {
			// Higher is better: (s - min) / (max - min)
			normScores[i] = (clipped[j] - minVal) / (maxVal - minVal)
		}
	}

	return normScores
}

func isFiniteScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0)
}

type RouterManager struct {
	routerInited      context.Context
	routerDoneInit    context.CancelFunc
	routerFactory     map[types.RoutingAlgorithm]types.RouterProviderFunc
	routerConstructor map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc
	// multiRouterCache is keyed by (possibly blended) algorithm string; both it and
	// unblendableLogged below are capped at maxCachedAlgorithmStrings since the key is
	// client-controlled and neither map is otherwise evicted.
	multiRouterCache map[string]*multiStrategyRouter
	// unblendableLogged is the set of original algorithm strings for which we've already
	// emitted a V(2) "skipping auto-blend" line. It is log-once bookkeeping only: Select
	// still re-evaluates the blend on every request so a later Register/Init can start
	// blending, and so a transient construct error is retried rather than black-holed.
	unblendableLogged map[string]struct{}
	routerMu          sync.RWMutex
}

func NewRouterManager() *RouterManager {
	rm := &RouterManager{}
	rm.routerInited, rm.routerDoneInit = context.WithTimeout(context.Background(), 5*time.Second)
	rm.routerFactory = make(map[types.RoutingAlgorithm]types.RouterProviderFunc)
	rm.routerConstructor = make(map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc)
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.unblendableLogged = make(map[string]struct{})
	return rm
}

// Validate validates if user provided routing routers is supported by gateway
func (rm *RouterManager) Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	// Parse the strategy configuration using multi-router parsing logic
	cfg, err := ParseMultiRouterConfig(algorithms)
	if err != nil {
		return RouterNotSet, false
	}

	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()

	// Validate each strategy in the configuration
	for _, item := range cfg.Items {
		provider, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]
		if !ok {
			return RouterNotSet, false
		}
		if len(cfg.Items) > 1 {
			if provider == nil {
				return RouterNotSet, false
			}
			router, err := provider(types.RoutingAlgorithm(algorithms).NewContext(context.Background(), "", "", "validate", ""))
			if err != nil {
				return RouterNotSet, false
			}
			if _, ok := router.(types.PodScorer); !ok {
				return RouterNotSet, false
			}
		}
	}

	// Return the original algorithms string to keep it intact in headers
	return types.RoutingAlgorithm(algorithms), true
}
func Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	return defaultRM.Validate(algorithms)
}

// Select the user provided router provider supported by gateway, no error reported and fallback to random router
// Call Validate before this function to ensure expected behavior.
func (rm *RouterManager) Select(ctx *types.RoutingContext) (types.Router, error) {
	algStr := string(ctx.Algorithm)

	// Parse once and reuse for both the auto-blend attempt below and the legacy
	// multi-strategy fallback, instead of re-parsing algStr repeatedly per request.
	cfg, cfgErr := ParseMultiRouterConfig(algStr)

	if cfgErr == nil {
		// Silently blend in load-balance's capacity-aware scoring (and, when needed,
		// least-request for multi-port support) behind whatever strategy the caller asked
		// for. The caller never sees this: ctx.Algorithm/algStr below is untouched, so
		// headers, Validate(), and error messages all still reflect the original strategy
		// name.
		blended, ok := appendLoadBalanceBlend(algStr, cfg)
		klog.V(4).Infof("routing select: algStr=%q autoBlendLoadBalanceWeight=%d autoBlendLeastRequestWeight=%d blend_ok=%v blended=%q", algStr, autoBlendLoadBalanceWeight, autoBlendLeastRequestWeight, ok, blended)
		if ok {
			if router, blendedOK := rm.tryAutoBlend(ctx, algStr, cfg, blended); blendedOK {
				return router, nil
			}
		}

		if len(cfg.Items) > 1 {
			multiRouter, err := rm.getOrCreateMultiStrategyRouter(algStr, cfg, ctx)
			if err == nil {
				return multiRouter, nil
			}
			// If multi-router initialization fails (e.g. strategy doesn't implement ScoreAll),
			// we log a message and fall back to the traditional single strategy provider below.
			klog.Infof("Cannot use multi-strategy router for %s: %v, falling back to legacy single strategy", algStr, err)
		}

		// If the parser recognized exactly one valid strategy (e.g. it was an exclusive strategy like "pd"
		// or just a single strategy that doesn't support ScoreAll), we fallback to that specific strategy.
		if len(cfg.Items) == 1 {
			algStr = cfg.Items[0].Name
		}
	} else {
		// Log the error but don't fallback to Random. Allow the request to fail down the line,
		// preserving the HTTP 400 Bad Request API contract.
		klog.Warningf("Failed to parse multi-strategy config '%s': %v", algStr, cfgErr)
	}

	// Legacy Single strategy fallback
	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()
	if provider, ok := rm.routerFactory[types.RoutingAlgorithm(algStr)]; ok {
		return provider(ctx)
	} else {
		// Return an error rather than falling back to random to preserve 400 Bad Request
		return nil, fmt.Errorf("unsupported router strategy: %s", algStr)
	}
}
func Select(ctx *types.RoutingContext) (types.Router, error) {
	return defaultRM.Select(ctx)
}

// tryAutoBlend attempts to construct the silent load-balance composite for algStr. origCfg is
// algStr's own already-parsed config (reused from Select rather than re-parsed here).
// Returns ok=false when the blend cannot be used this request; the caller then routes
// algStr alone. Permanent skips (primary is not a PodScorer, blend not registered) are
// logged once at V(2). Transient construct errors are not remembered, so the next
// request retries.
func (rm *RouterManager) tryAutoBlend(ctx *types.RoutingContext, algStr string, origCfg *MultiRouterConfig, blended string) (types.Router, bool) {
	// Fast path: once blended has been constructed successfully once, every later request
	// for it is a single lock-protected map read — no re-parsing, re-registration check, or
	// re-probing of the primary strategy's PodScorer-ness on every request.
	rm.routerMu.RLock()
	cached, hit := rm.multiRouterCache[blended]
	rm.routerMu.RUnlock()
	if hit {
		return cached, true
	}

	blendedCfg, parseErr := ParseMultiRouterConfig(blended)
	if parseErr != nil || len(blendedCfg.Items) <= 1 {
		rm.logUnblendableOnce(algStr, "blended config is invalid or collapsed to a single strategy")
		return nil, false
	}
	if !rm.allRegistered(blendedCfg) {
		rm.logUnblendableOnce(algStr, "one or more blended strategies are not registered")
		return nil, false
	}

	// Probe the caller's own strategies before constructing the composite for the first
	// time. random (and any other non-PodScorer) is a permanent skip: building the
	// composite would fail the same way on every request and used to log that at Info on
	// the hot path.
	scorerOK, transientErr := rm.allImplementPodScorer(origCfg, ctx)
	if transientErr != nil {
		klog.V(4).Infof("Cannot auto-blend %s this request: %v", algStr, transientErr)
		return nil, false
	}
	if !scorerOK {
		rm.logUnblendableOnce(algStr, "primary strategy does not implement types.PodScorer")
		return nil, false
	}

	multiRouter, err := rm.getOrCreateMultiStrategyRouter(blended, blendedCfg, ctx)
	if err != nil {
		klog.V(4).Infof("Cannot silently blend load-balance into %s: %v, falling back to %s alone", algStr, err, algStr)
		return nil, false
	}
	return multiRouter, true
}

func (rm *RouterManager) logUnblendableOnce(algStr, reason string) {
	rm.routerMu.Lock()
	_, seen := rm.unblendableLogged[algStr]
	if !seen && len(rm.unblendableLogged) < maxCachedAlgorithmStrings {
		rm.unblendableLogged[algStr] = struct{}{}
	}
	rm.routerMu.Unlock()
	if !seen {
		klog.V(2).Infof("Skipping auto-blend for %s (%s), routing it alone", algStr, reason)
	}
}

// allImplementPodScorer reports whether every strategy in cfg constructs as a
// types.PodScorer. A provider error is returned separately so the caller can retry
// next request instead of treating it as a permanent "not a scorer" skip.
func (rm *RouterManager) allImplementPodScorer(cfg *MultiRouterConfig, ctx *types.RoutingContext) (bool, error) {
	for _, item := range cfg.Items {
		rm.routerMu.RLock()
		provider, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]
		rm.routerMu.RUnlock()
		if !ok || provider == nil {
			return false, nil
		}
		router, err := provider(ctx)
		if err != nil {
			return false, err
		}
		if _, isScorer := router.(types.PodScorer); !isScorer {
			return false, nil
		}
	}
	return true, nil
}

// allRegistered reports whether every strategy in cfg has a registered provider, without
// constructing any of them.
func (rm *RouterManager) allRegistered(cfg *MultiRouterConfig) bool {
	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()
	for _, item := range cfg.Items {
		if _, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]; !ok {
			return false
		}
	}
	return true
}

func (rm *RouterManager) getOrCreateMultiStrategyRouter(algStr string, cfg *MultiRouterConfig, ctx *types.RoutingContext) (*multiStrategyRouter, error) {
	rm.routerMu.RLock()
	if router, ok := rm.multiRouterCache[algStr]; ok {
		rm.routerMu.RUnlock()
		return router, nil
	}
	rm.routerMu.RUnlock()

	router, err := newMultiStrategyRouter(cfg, rm, ctx)
	if err != nil {
		return nil, err
	}

	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	if cached, ok := rm.multiRouterCache[algStr]; ok {
		return cached, nil
	}
	if len(rm.multiRouterCache) < maxCachedAlgorithmStrings {
		rm.multiRouterCache[algStr] = router
	}
	return router, nil
}

func (rm *RouterManager) Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.unblendableLogged = make(map[string]struct{})
	rm.routerConstructor[algorithm] = func() types.RouterProviderFunc {
		router, err := constructor()
		if err != nil {
			klog.Errorf("Failed to construct router for %s: %v", algorithm, err)
			return nil
		}
		return func(_ *types.RoutingContext) (types.Router, error) {
			return router, nil
		}
	}
}
func Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	defaultRM.Register(algorithm, constructor)
}

func (rm *RouterManager) RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.routerFactory[algorithm] = provider
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.unblendableLogged = make(map[string]struct{})
	klog.V(4).Infof("Registered router for %s", algorithm)
}
func RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	defaultRM.RegisterProvider(algorithm, provider)
}

func (rm *RouterManager) SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	r, ok := router.(types.FallbackRouter)
	if !ok {
		return ErrFallbackNotSupported
	}

	<-rm.routerInited.Done()
	initErr := rm.routerInited.Err()
	if initErr != context.Canceled {
		return fmt.Errorf("router did not initialized: %v", initErr)
	}

	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()

	if provider, ok := rm.routerFactory[fallback]; !ok {
		return ErrFallbackNotRegistered
	} else {
		r.SetFallback(fallback, provider)
	}
	return nil
}
func SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	return defaultRM.SetFallback(router, fallback)
}

func (rm *RouterManager) Init() {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	for algorithm, constructor := range rm.routerConstructor {
		rm.routerFactory[algorithm] = constructor()
		klog.V(4).Infof("Registered router for %s", algorithm)
	}
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.unblendableLogged = make(map[string]struct{})
	rm.routerDoneInit()
}
func Init() {
	defaultRM.Init()
}
