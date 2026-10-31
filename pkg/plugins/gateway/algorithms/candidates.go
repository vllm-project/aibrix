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
// AIBRIX_ROUTING_CANDIDATE_K > 0, the gateway picks K pods first and runs the gate and the router
// on those only. K is the only knob.
//
// The router's own strategies pick the K pods. A random scout of candidateScoutFactor*K ready
// pods, plus up to K pods a strategy finds directly (prefix-cache's holders of the request's
// prefix, see types.CandidateFinder), is scored on each strategy's in-memory estimate
// (types.CandidateScorer), blended with the router's weights and normalization. The best K/2
// of the scout are kept and the other K/2 are drawn at random from the whole fleet. A
// request's prefix holders, its least-loaded pods and the pods with the most KV headroom so
// compete for the same K slots, as they compete in the router's blend. Estimating only a scout
// keeps the stage's own work bounded by K too: estimating every ready pod costs about a
// microsecond per pod and strategy, which at 2,000 pods is as much as full scoring in process.
// Off by default; a model config profile can set K through routingConfig.candidate.k.
var routingCandidateK = utils.LoadEnvInt("AIBRIX_ROUTING_CANDIDATE_K", 0)

// candidateScoutFactor sets the random scout to candidateScoutFactor*K ready pods. Ranking K
// of 4K random pods already keeps most of the gain of ranking every pod (the
// power-of-d-choices effect), at a cost that does not grow with the fleet.
const candidateScoutFactor = 4

// The best K/2 scouted pods (rounded down) are ranked into the set; the other slots go to pods
// drawn at random from the whole fleet. The random half does two jobs. The estimates lag (other
// gateways' running counts by up to one gateway-snapshot interval, scraped engine metrics by up
// to one scrape interval), so between refreshes every request ranks the same pods first; random
// pods keep others in view, and the exact scoring that follows decides between them. And the
// load-imbalance gate and the blend's normalization run on the candidates and assume they look
// like the fleet: with three quarters ranked, the set mixes idle pods with hot prefix holders,
// the gate fires against the holders and prefix affinity collapses (candidatesim,
// TestCandidateSelectionKOnly). It is not a knob: it guards the ranking.
func candidateRankedSlots(k int) int { return k / 2 }

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
	ranked, scouted := 0, 0
	path := candidatePathNoEstimate
	parts := candidateParts(router)
	if hasEstimate(parts) {
		path = candidatePathEstimateFailed
		scout := candidateScout(ctx, parts, readyPods, k)
		scouted = len(scout)
		if estimates, ok := candidateEstimates(ctx, parts, scout); ok {
			path = candidatePathRanked
			ranked = candidateRankedSlots(k)
			for _, i := range bestIndices(estimates, ranked) {
				picked[scout[i]] = struct{}{}
				candidates = append(candidates, scout[i])
			}
		}
	}
	sampleCandidates(readyPods, k, picked, &candidates)
	recordCandidateSelection(ctx.Model, path)

	if klog.V(5).Enabled() {
		klog.V(5).InfoS("routing candidate stage", "request_id", ctx.RequestID, "ready_pods", len(readyPods),
			"candidates", len(candidates), "scouted", scouted, "ranked", ranked, "path", path, "strategy", ctx.ResolvedStrategy)
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

func hasEstimate(parts []weightedScorer) bool {
	for _, part := range parts {
		if _, ok := part.scorer.(types.CandidateScorer); ok {
			return true
		}
	}
	return false
}

// candidateScout returns the ready pods the stage estimates: up to K pods each finder among
// parts returns, best first, then candidateScoutFactor*K pods drawn at random, without
// duplicates. When that would cover most of the fleet, it is the whole fleet.
func candidateScout(ctx *types.RoutingContext, parts []weightedScorer, readyPods []*v1.Pod, k int) []*v1.Pod {
	size := candidateScoutFactor * k
	if 2*size >= len(readyPods) {
		return readyPods
	}
	scout := make([]*v1.Pod, 0, size+k)
	seen := make(map[*v1.Pod]struct{}, size+k)
	podList := &utils.PodArray{Pods: readyPods}
	for _, part := range parts {
		finder, ok := part.scorer.(types.CandidateFinder)
		if !ok {
			continue
		}
		found := finder.FindCandidates(ctx, podList, k)
		if len(found) > k {
			found = found[:k]
		}
		for _, pod := range found {
			if _, dup := seen[pod]; !dup {
				seen[pod] = struct{}{}
				scout = append(scout, pod)
			}
		}
	}
	sampleCandidates(readyPods, len(scout)+size, seen, &scout)
	return scout
}

// candidateEstimates blends the in-memory estimates of the strategies in parts into one score
// per pod of pods, higher is better, the way multiStrategyRouter.scoreAndRank blends ScoreAll:
// each strategy's scores are normalized to [0, 1] by its polarity over pods and weighted by its
// share of the total weight. A strategy without an estimate (no types.CandidateScorer, or an
// error) counts as 0 for every pod, so it shifts no pod ahead of another. ok is false when no
// strategy has an estimate.
func candidateEstimates(ctx *types.RoutingContext, parts []weightedScorer, pods []*v1.Pod) ([]float64, bool) {
	total := 0.0
	for _, part := range parts {
		total += part.weight
	}
	if total <= 0 {
		return nil, false
	}
	podList := &utils.PodArray{Pods: pods}
	var blended []float64
	for _, part := range parts {
		estimator, ok := part.scorer.(types.CandidateScorer)
		if !ok {
			continue
		}
		scores, scored, err := estimator.CandidateScores(ctx, podList)
		if err != nil || len(scores) != len(pods) || len(scored) != len(pods) {
			if err != nil {
				klog.V(4).ErrorS(err, "candidate estimate skipped", "request_id", ctx.RequestID)
			}
			continue
		}
		if blended == nil {
			blended = make([]float64, len(pods))
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
// until it holds k, or every ready pod. Every pod in candidates must be in picked. When the
// sample is a small share of readyPods, rejection sampling keeps this O(k) instead of touching
// every pod.
func sampleCandidates(readyPods []*v1.Pod, k int, picked map[*v1.Pod]struct{}, candidates *[]*v1.Pod) {
	add := func(pod *v1.Pod) {
		if _, dup := picked[pod]; !dup {
			picked[pod] = struct{}{}
			*candidates = append(*candidates, pod)
		}
	}
	k = min(k, len(readyPods))
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
