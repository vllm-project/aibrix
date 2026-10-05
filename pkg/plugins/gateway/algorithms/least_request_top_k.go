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
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const RouterLeastRequestTopK types.RoutingAlgorithm = "least-request-top-k"

func init() {
	Register(RouterLeastRequestTopK, NewLeastRequestTopKRouter)
}

// leastRequestTopKCandidates bounds how many of the least-loaded ready pods are eligible before
// picking uniformly at random among them. Plain least-request always resolves to the single
// global-minimum pod; when several independent gateway replicas all read the same
// Redis-backed running-request counter and each pick that same strict minimum, they can all
// route their next request there simultaneously -- the counter's HINCRBY is fire-and-forget
// with respect to the request path (see cache_running_requests.go's overlaySelfRunningRequests
// doc comment), so none of the other gateways see each other's just-issued increments in time.
// Restricting to a uniform-random pick among the K least-loaded pods, instead of always the
// single least-loaded one, spreads that simultaneous traffic across K pods so no single pod
// absorbs a full multi-gateway burst. K should generally be at least the number of concurrently
// deciding gateway replicas for that spreading to be effective; 5 is a reasonable default for a
// handful of gateway replicas and is adjustable per deployment. See leastRequestTopKEpsilon for
// the other half of the eligibility rule.
var leastRequestTopKCandidates = utils.LoadEnvInt("AIBRIX_ROUTING_LEAST_REQUEST_TOP_K", 5)

// leastRequestTopKEpsilon further bounds eligibility by closeness to the minimum, not just
// rank: a pod only qualifies if its count is within this many requests of the least-loaded
// ready pod's count. Rank alone can't distinguish a tightly clustered fleet from a skewed one --
// if the true minimum is idle (count 0) but the Kth-ranked pod already has 15 requests queued, a
// uniform pick among the top K would sometimes route to that materially worse pod just to fill
// out K candidates. Epsilon fixes this: the candidate set shrinks when load is skewed (few or
// one pod near the minimum) and widens when load is flat (many pods near the minimum), instead
// of always targeting exactly K. The two bounds combine as an AND: a pod must be within epsilon
// of the minimum AND among the K least-loaded. There's no universally correct default -- it
// needs tuning to the deployment's typical running-request-count magnitude (a delta of 2 means
// something very different when counts run 0-5 than when they run 20-50); the default here is a
// conservative starting point, not a validated one.
var leastRequestTopKEpsilon = utils.LoadEnvInt("AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_EPSILON", 4)

const (
	leastRequestTopKRampWindowEnv     = "AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_RAMP_WINDOW"
	leastRequestTopKRampWindowDefault = 300 * time.Second
)

// leastRequestTopKRampWindow bounds how long a newly-(re)eligible pod's synthetic ramp penalty
// (see applyRampAdjustment) decays for. A pod that has never been routed to reads as count 0
// (getRequestCounts), which -- when the rest of the fleet sits well above epsilon of that --
// makes it the sole eligible candidate and funnels every routing decision to it until it
// "catches up," before it's actually warmed up (confirmed against a production incident: a
// newly-added pod absorbed 6 requests in 350ms while its engine's own reported running-count
// never left 0, then dropped out of routing entirely). The ramp penalty counters this by making
// the pod's effective count start at parity with the fleet's proven-best pod instead of a lone
// outlier, decaying to 0 -- i.e. fully back to plain least-request-top-k behavior -- by the end
// of this window. Defaults to enabled, at a 5-minute window: the production incident above showed
// this pileup happening in practice, so the fix ships active rather than requiring opt-in. Set
// the env var to 0 to disable; see loadLeastRequestTopKRampWindow.
var leastRequestTopKRampWindow = loadLeastRequestTopKRampWindow()

// loadLeastRequestTopKRampWindow reads leastRequestTopKRampWindowEnv. utils.LoadEnvDuration
// rejects non-positive values and falls back to the default, which here is positive, so it
// alone could never turn the ramp off -- an explicit "0" is therefore handled up front.
func loadLeastRequestTopKRampWindow() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(leastRequestTopKRampWindowEnv)); err == nil && d == 0 {
		klog.Infof("set %s: 0, ramp penalty disabled", leastRequestTopKRampWindowEnv)
		return 0
	}
	return utils.LoadEnvDuration(leastRequestTopKRampWindowEnv, leastRequestTopKRampWindowDefault)
}

// LeastRequestTopKRouter picks among the ready pods that are both among the K least-loaded and
// within leastRequestTopKEpsilon requests of the least-loaded pod's count, rather than always
// the single least-loaded pod. Unlike power-of-two -- which samples two pods uniformly from the
// whole fleet and can end up comparing two already-busy pods -- every candidate here is drawn
// from the genuinely least-loaded end of the fleet, so a pick is never worse than the
// Kth-least-loaded pod, and the epsilon bound keeps it from being meaningfully worse than the
// least-loaded pod either.
//
// It implements both types.Router (Route: a direct uniform-random pick among the eligible set,
// used as a standalone top-level strategy) and types.PodScorer (ScoreAll: a per-pod score for
// the eligible set with per-request random jitter, used when blended with other strategies --
// see ScoreAll's doc comment for why the blended path needs its own randomization mechanism).
type LeastRequestTopKRouter struct {
	cache cache.Cache
	// clock is injectable so unit tests can advance ramp-penalty decay without sleeping;
	// nil in production, where now() falls back to time.Now(). See pd/token_load_tracker.go
	// for the same pattern.
	clock func() time.Time
}

// NewLeastRequestTopKRouter creates a LeastRequestTopKRouter backed by the global cache.
func NewLeastRequestTopKRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return NewLeastRequestTopKRouterWithCache(c), nil
}

// NewLeastRequestTopKRouterWithCache creates a LeastRequestTopKRouter that reads load from the
// given cache.
func NewLeastRequestTopKRouterWithCache(c cache.Cache) *LeastRequestTopKRouter {
	return &LeastRequestTopKRouter{cache: c}
}

// newLeastRequestTopKRouterWithCacheAndClock is a test-only constructor for deterministic
// ramp-decay tests. Production always goes through NewLeastRequestTopKRouterWithCache (clock nil).
func newLeastRequestTopKRouterWithCacheAndClock(c cache.Cache, clock func() time.Time) *LeastRequestTopKRouter {
	return &LeastRequestTopKRouter{cache: c, clock: clock}
}

// now returns the router's current time, defaulting to time.Now when no clock was injected.
func (r *LeastRequestTopKRouter) now() time.Time {
	if r.clock == nil {
		return time.Now()
	}
	return r.clock()
}

// Route implements [types.Router]: it reads the live cross-gateway running-request count for
// every ready pod, then picks uniformly at random among the pods satisfying both
// leastRequestTopKCandidates and leastRequestTopKEpsilon.
func (r *LeastRequestTopKRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available")
	}

	eligible, counts, minCount := r.eligibleLeastLoaded(readyPods)
	target := eligible[rand.IntN(len(eligible))]
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("least_request_top_k_selection",
			"request_id", ctx.RequestID, "target_pod", target.Name,
			"target_pod_count", counts[utils.GeneratePodKey(target.Namespace, target.Name)],
			"min_count", minCount, "eligible_count", len(eligible), "candidate_count", len(readyPods))
	}

	targetPort := selectTargetPortForPodWithLeastRequestCount(r.cache, target, readyPodList.ListPortsForPod())
	ctx.SetTargetPod(target)
	if targetPort != 0 {
		ctx.SetTargetPort(targetPort)
	}
	return ctx.TargetAddress(), nil
}

// getReadySinceCounts returns each ready pod's readySince (UnixNano; see Pod.readySince), keyed
// by utils.GeneratePodKey like getRequestCounts' counts map. Mirrors getRequestCounts' fail-open
// shape, but the opposite safe default: the cache not supporting cache.PodReadySinceProvider, the
// call failing, or a pod missing from the result all mean "no ramp info" (pod simply absent from
// the returned map), never "assume it's mid-ramp".
func getReadySinceCounts(c cache.Cache, readyPods []*v1.Pod) map[string]int64 {
	provider, ok := c.(cache.PodReadySinceProvider)
	if !ok {
		return nil
	}
	readySince, err := provider.GetPodsReadySince(readyPods)
	if err != nil || len(readySince) == 0 {
		return nil
	}
	return readySince
}

// applyRampAdjustment returns counts unchanged when ramping is disabled (leastRequestTopKRampWindow
// <= 0), the cache has no ramp info for any pod, or every pod with ramp info is currently within
// its own ramp window (no established warm baseline to ramp toward -- notably including the case
// where this gateway process just restarted and its informer is doing its initial relist, which
// is indistinguishable from every pod having just been created; see readySince in pod.go).
// Otherwise it returns a new map where each still-ramping pod's count is bumped by a penalty that
// starts at floor (the minimum count among pods NOT currently ramping) and decays linearly to 0
// by leastRequestTopKRampWindow.
//
// floor must come from the non-ramping subset, not the minimum over all pods: the ramping pod is
// usually the true minimum (count 0, never routed to), so a floor taken over everyone would
// collapse to the ramping pod's own count and the penalty would always be zero -- self-defeating.
//
// Ramp is enabled by default, so this runs on every routing decision -- in steady state (fleet
// fully warmed up) it must cost next to nothing. The cache.PodReadySinceProvider.LatestPodReadySince
// check below is what makes that true: it's a single atomic load, versus the per-pod
// GetPodsReadySince call plus two more passes over readyPods plus a fresh map allocation that
// follow it, so skipping straight to "nothing is ramping" there avoids all of that on every call
// where it's true (the overwhelming majority of the time).
func applyRampAdjustment(c cache.Cache, now time.Time, readyPods []*v1.Pod, counts map[string]int) map[string]int {
	window := leastRequestTopKRampWindow
	if window <= 0 {
		return counts
	}
	nowNanos := now.UnixNano()
	if provider, ok := c.(cache.PodReadySinceProvider); ok {
		if latest := provider.LatestPodReadySince(); latest == 0 || time.Duration(nowNanos-latest) >= window {
			// Nothing anywhere has become newly-eligible within the ramp window -- every
			// individual pod's readySince is at least this old, so none can be ramping.
			return counts
		}
	}

	readySince := getReadySinceCounts(c, readyPods)
	if len(readySince) == 0 {
		return counts
	}

	warmMin, haveWarm := 0, false
	for _, pod := range readyPods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		rs, tracked := readySince[podKey]
		inRamp := tracked && rs != 0 && time.Duration(nowNanos-rs) < window
		if inRamp {
			continue
		}
		if !haveWarm || counts[podKey] < warmMin {
			warmMin, haveWarm = counts[podKey], true
		}
	}
	if !haveWarm {
		// Whole fleet is currently ramping (e.g. this gateway just restarted) -- no warm
		// baseline exists to chase, so leave counts untouched for this decision.
		return counts
	}

	adjusted := make(map[string]int, len(counts))
	for k, v := range counts {
		adjusted[k] = v
	}
	for _, pod := range readyPods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		rs, tracked := readySince[podKey]
		if !tracked || rs == 0 {
			continue
		}
		elapsed := time.Duration(nowNanos - rs)
		if elapsed >= window {
			continue
		}
		if elapsed < 0 {
			elapsed = 0 // defensive against clock skew / a fake clock rewinding in tests
		}
		decay := 1 - float64(elapsed)/float64(window)
		penalty := int(math.Round(float64(warmMin) * decay))
		adjusted[podKey] += penalty
		if penalty != 0 && klog.V(4).Enabled() {
			klog.V(4).InfoS("least_request_top_k_ramp_penalty",
				"target_pod", pod.Name, "real_count", counts[podKey], "effective_count", adjusted[podKey],
				"penalty", penalty, "elapsed", elapsed, "warm_floor", warmMin, "ramp_window", window)
		}
	}
	return adjusted
}

// eligibleLeastLoaded ranks readyPods by running-request count and returns the prefix that is
// both within the top K by rank (leastRequestTopKCandidates) and within epsilon of the minimum
// count (leastRequestTopKEpsilon), along with the per-pod counts (keyed by utils.GeneratePodKey)
// and the minimum count observed. Pods are shuffled before the (stable) sort by count, so a tie
// straddling either cutoff -- or every pod being idle -- doesn't deterministically favor
// whichever pod happens to come first in readyPods. Shared by Route (uniform pick among the
// result) and ScoreAll (scores the result with jitter -- see its doc comment for why blending
// needs a different randomization point).
//
// counts are adjusted by applyRampAdjustment before ranking, so a newly-(re)eligible pod's
// synthetic zero doesn't make it a lone outlier -- see leastRequestTopKRampWindow. minCount is
// derived from the (possibly ramp-adjusted) counts, so the epsilon window automatically re-anchors
// to the best proven-or-adequately-ramped candidate rather than being fooled by a real zero.
func (r *LeastRequestTopKRouter) eligibleLeastLoaded(readyPods []*v1.Pod) (eligible []*v1.Pod, counts map[string]int, minCount int) {
	counts = getRequestCounts(r.cache, readyPods)
	counts = applyRampAdjustment(r.cache, r.now(), readyPods, counts)

	candidates := make([]*v1.Pod, len(readyPods))
	copy(candidates, readyPods)
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	countOf := func(pod *v1.Pod) int { return counts[utils.GeneratePodKey(pod.Namespace, pod.Name)] }
	sort.SliceStable(candidates, func(i, j int) bool {
		return countOf(candidates[i]) < countOf(candidates[j])
	})

	k := leastRequestTopKCandidates
	if k <= 0 || k > len(candidates) {
		k = len(candidates)
	}

	// candidates is sorted ascending by count, so once one pod exceeds min+epsilon every pod
	// after it does too -- n only needs to grow until that first violation or until it hits k.
	minCount = countOf(candidates[0])
	n := k
	if epsilon := leastRequestTopKEpsilon; epsilon >= 0 {
		for n = 1; n < k; n++ {
			if countOf(candidates[n])-minCount > epsilon {
				break
			}
		}
	}
	return candidates[:n], counts, minCount
}

// ScoreAll implements [types.PodScorer], letting this strategy be blended with others (e.g.
// load-balance) instead of only running standalone via Route.
//
// The multi-strategy blend (multiStrategyRouter.scoreAndRank) combines every configured
// strategy's normalized score with a deterministic weighted sum and picks the argmax -- there is
// no randomness at the combining step. So Route's own source of spread (a uniform pick among the
// eligible set) has no equivalent once blended: returning the same score for every eligible pod
// would just let combining with another strategy's tie-break (or plain pod-name order) decide
// deterministically, collapsing back to a single consistently-favored pod. Instead, eligible
// pods get their count minus a fresh random jitter in [0, epsilon] -- bounded to epsilon so
// jitter can only reorder pods that were already within epsilon of the minimum, never promote an
// ineligible one -- redrawn on every call so which eligible pod scores best varies from request
// to request. Ineligible pods keep their plain count (no jitter), which is already enough to
// normalize worse than the jittered eligible cluster in virtually every case.
func (r *LeastRequestTopKRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	eligible, counts, _ := r.eligibleLeastLoaded(pods)
	eligibleSet := make(map[string]struct{}, len(eligible))
	for _, pod := range eligible {
		eligibleSet[utils.GeneratePodKey(pod.Namespace, pod.Name)] = struct{}{}
	}

	epsilon := leastRequestTopKEpsilon
	for i, pod := range pods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		score := float64(counts[podKey])
		if _, ok := eligibleSet[podKey]; ok && epsilon >= 0 {
			score -= rand.Float64() * float64(epsilon)
		}
		scores[i] = score
		scored[i] = true
	}
	return scores, scored, nil
}

// Polarity implements [types.PodScorer]: the fewer requests, the better -- same convention as
// least-request.
func (r *LeastRequestTopKRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

// SubscribedMetrics implements [types.Router]. Like least-request, this reads the live
// cross-gateway running-request count (Cache.GetPodsRunningRequests), not a scraped engine
// metric, so there's nothing to subscribe to.
func (r *LeastRequestTopKRouter) SubscribedMetrics() []string {
	return []string{}
}

var _ types.Router = (*LeastRequestTopKRouter)(nil)
var _ types.PodScorer = (*LeastRequestTopKRouter)(nil)
