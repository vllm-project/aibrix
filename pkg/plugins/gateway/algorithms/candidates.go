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
	"math/rand/v2"
	"sort"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// The candidate stage (RFC #3188) bounds the per-request routing work by K instead of the number
// of ready pods. Every strategy and the load-imbalance gate read each pod's cross-gateway running
// count from Redis, so routing over N pods costs several N-pod Redis reads per request. With
// AIBRIX_ROUTING_CANDIDATE_K > 0, the gateway first ranks every ready pod on the strategies' own
// in-memory estimates (types.CandidateScorer), blended with the router's weights, keeps the best
// of them plus a few at random, K pods in all, and runs the gate and the router on that set
// only. K is the only knob: the prefix holders of a request, the least-loaded pods and the pods
// with the most KV headroom all compete for the same K slots, the way they compete in the
// router's own blend. Off by default; a model config profile can set K through
// routingConfig.candidate.k.
var routingCandidateK = utils.LoadEnvInt("AIBRIX_ROUTING_CANDIDATE_K", 0)

// candidateExploreDivisor reserves K/candidateExploreDivisor of the K slots (none when K < 4)
// for pods drawn at random from the ones the ranking left out. The estimates the ranking reads
// lag: other gateways' running counts by up to one gateway-snapshot interval (100ms), scraped
// engine metrics by up to one scrape interval. Between refreshes every request ranks the same
// pods first, so a purely ranked set would keep offering the router the same few pods; the
// random slots keep other pods in view, and the exact scoring that follows decides between them.
// It is not a knob: it guards the ranking, it does not express a routing preference.
const candidateExploreDivisor = 4

// candidateSafeStrategies are the strategies whose choice depends only on the pods they are
// given, so routing over a sample changes which pods compete but not the formulas that judge
// them. Checks that compare a pod with the rest of its pool (the gate's spread, prefix-cache's
// standard-deviation check, the blend's normalization) then compare it with the sample, not the
// fleet. A new strategy is kept off the candidate stage until it is added here.
// Strategies that keep a global view (vtc's fairness, the queue router, session pins, external
// and throughput routers, and the exclusive pd and slo routers) always see every ready pod.
var candidateSafeStrategies = map[string]bool{
	string(RouterLoadBalance):            true,
	string(RouterPrefixCache):            true,
	string(RouterRandom):                 true,
	string(RouterPowerOfTwo):             true,
	string(least.RouterLeastRequest):     true,
	string(least.RouterLeastRequestTopK): true,
	string(least.RouterLeastKvCache):     true,
	string(least.RouterLeastGpuCache):    true,
	string(least.RouterLeastBusyTime):    true,
	string(least.RouterLeastLatency):     true,
	string(least.RouterUtil):             true,
}

// candidateStageApplies reports whether every strategy in the request's resolved strategy, the
// one Select actually built including any auto-blend, is candidate-safe.
func candidateStageApplies(ctx *types.RoutingContext) bool {
	resolved := ctx.ResolvedStrategy
	if resolved == "" {
		resolved = string(ctx.Algorithm)
	}
	cfg, err := ParseMultiRouterConfig(resolved)
	if err != nil || len(cfg.Items) == 0 {
		return false
	}
	for _, item := range cfg.Items {
		if !candidateSafeStrategies[item.Name] {
			return false
		}
	}
	return true
}

// SelectCandidates returns the pods the gate and router should consider for this request: all of
// readyPods when the candidate stage is off, does not apply to the strategy, or would not shrink
// the set; otherwise K of them (see routingCandidateK). It reads no per-pod state from Redis.
// router is the one Select returned for ctx.
func SelectCandidates(ctx *types.RoutingContext, router types.Router, readyPods []*v1.Pod) []*v1.Pod {
	return selectCandidates(ctx, router, readyPods, ctx.RoutingOverrides().Candidate.K)
}

func selectCandidates(ctx *types.RoutingContext, router types.Router, readyPods []*v1.Pod, k int) []*v1.Pod {
	if k <= 0 || len(readyPods) <= k || !candidateStageApplies(ctx) {
		return readyPods
	}

	candidates := make([]*v1.Pod, 0, k)
	picked := make(map[*v1.Pod]struct{}, k)
	ranked := 0
	if estimates, ok := candidateEstimates(ctx, router, readyPods); ok {
		ranked = k - k/candidateExploreDivisor
		for _, i := range bestIndices(estimates, ranked) {
			picked[readyPods[i]] = struct{}{}
			candidates = append(candidates, readyPods[i])
		}
	}
	sampleCandidates(readyPods, k, picked, &candidates)

	if klog.V(5).Enabled() {
		klog.V(5).InfoS("routing candidate stage", "request_id", ctx.RequestID, "ready_pods", len(readyPods),
			"candidates", len(candidates), "ranked", ranked, "strategy", ctx.ResolvedStrategy)
	}
	return candidates
}

// weightedScorer is one strategy of the router and its weight in the blend.
type weightedScorer struct {
	scorer types.PodScorer
	weight float64
}

// candidateParts returns the strategies router blends, with their weights: the configured
// items of a multi-strategy router, or router itself with weight 1.
func candidateParts(router types.Router) []weightedScorer {
	if m, ok := router.(*multiStrategyRouter); ok {
		parts := make([]weightedScorer, 0, len(m.config.Items))
		for _, item := range m.config.Items {
			if scorer, ok := m.scorers[item.Name]; ok && item.Coefficient > 0 {
				parts = append(parts, weightedScorer{scorer: scorer, weight: float64(item.Coefficient)})
			}
		}
		return parts
	}
	if scorer, ok := router.(types.PodScorer); ok {
		return []weightedScorer{{scorer: scorer, weight: 1}}
	}
	return nil
}

// candidateEstimates blends the in-memory estimates of router's strategies into one score per
// pod, higher is better, the way multiStrategyRouter.scoreAndRank blends ScoreAll: each
// strategy's scores are normalized to [0, 1] by its polarity and weighted by its share of the
// total weight. A strategy without an estimate (no types.CandidateScorer, or an error) counts
// as 0 for every pod, so it shifts no pod ahead of another. ok is false when no strategy has an
// estimate; the candidates are then drawn at random.
func candidateEstimates(ctx *types.RoutingContext, router types.Router, readyPods []*v1.Pod) ([]float64, bool) {
	parts := candidateParts(router)
	total := 0.0
	for _, part := range parts {
		total += part.weight
	}
	if total <= 0 {
		return nil, false
	}
	podList := &utils.PodArray{Pods: readyPods}
	var blended []float64
	for _, part := range parts {
		estimator, ok := part.scorer.(types.CandidateScorer)
		if !ok {
			continue
		}
		scores, scored, err := estimator.CandidateScores(ctx, podList)
		if err != nil || len(scores) != len(readyPods) || len(scored) != len(readyPods) {
			if err != nil {
				klog.V(4).ErrorS(err, "candidate estimate skipped", "request_id", ctx.RequestID)
			}
			continue
		}
		if blended == nil {
			blended = make([]float64, len(readyPods))
		}
		share := part.weight / total
		for i, norm := range normalizeScores(scores, scored, estimator.Polarity()) {
			blended[i] += norm * share
		}
	}
	return blended, blended != nil
}

// bestIndices returns the indices of the n highest scores, best first. Equal scores are ordered
// at random, so pods the estimates cannot tell apart (an idle fleet, the holders of one hot
// prefix) share the ranked slots across requests instead of the same pods always winning.
func bestIndices(scores []float64, n int) []int {
	if n <= 0 {
		return nil
	}
	if n > len(scores) {
		n = len(scores)
	}
	top := make([]int, 0, n)
	for _, i := range rand.Perm(len(scores)) {
		s := scores[i]
		if len(top) == n && s <= scores[top[n-1]] {
			continue
		}
		pos := sort.Search(len(top), func(j int) bool { return scores[top[j]] < s })
		if len(top) < n {
			top = append(top, 0)
		}
		copy(top[pos+1:], top[pos:len(top)-1])
		top[pos] = i
	}
	return top
}

// sampleCandidates adds pods of readyPods not yet in picked to candidates, uniformly at random,
// until it holds k. The caller guarantees k < len(readyPods), so enough such pods exist. When
// the sample is a small share of readyPods, rejection sampling keeps this O(k) instead of
// touching every pod.
func sampleCandidates(readyPods []*v1.Pod, k int, picked map[*v1.Pod]struct{}, candidates *[]*v1.Pod) {
	add := func(pod *v1.Pod) {
		if _, dup := picked[pod]; !dup {
			picked[pod] = struct{}{}
			*candidates = append(*candidates, pod)
		}
	}
	if 2*k > len(readyPods) {
		for _, i := range rand.Perm(len(readyPods)) {
			if len(*candidates) >= k {
				return
			}
			add(readyPods[i])
		}
		return
	}
	for len(*candidates) < k {
		add(readyPods[rand.IntN(len(readyPods))])
	}
}
