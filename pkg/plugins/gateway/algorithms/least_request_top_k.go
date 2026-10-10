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

// leastRequestTopKCandidates (K) bounds the eligible set by rank. Plain least-request always picks
// the single global minimum, so gateway replicas that read the same Redis-backed counter at the
// same moment all pick the same pod: the counter increment is fire-and-forget (see
// overlaySelfRunningRequests in cache_running_requests.go), so none sees the others' picks in
// time. A uniform pick among the K least-loaded spreads that burst; K should be at least the
// number of gateway replicas deciding concurrently. Tuning notes are in ENV_VARS.md.
var leastRequestTopKCandidates = utils.LoadEnvInt("AIBRIX_ROUTING_LEAST_REQUEST_TOP_K", 5)

// leastRequestTopKEpsilon bounds the eligible set by closeness to the minimum: a pod also has to
// be within this many requests of the least-loaded pod. Rank alone can't tell a clustered fleet
// from a skewed one (one idle pod, the rest far busier), where padding the set to K would route
// to materially worse pods. The two bounds combine as an AND. The default is a starting point, not
// a validated one; it depends on the deployment's typical running-request count.
var leastRequestTopKEpsilon = utils.LoadEnvInt("AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_EPSILON", 4)

const (
	leastRequestTopKRampWindowEnv     = "AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_RAMP_WINDOW"
	leastRequestTopKRampWindowDefault = 300 * time.Second
)

// leastRequestTopKRampWindow is how long a newly routable pod's ramp penalty (see
// applyRampAdjustment) takes to decay to 0. A pod with no routing history reads as count 0, so
// when the rest of the fleet is well above epsilon it becomes the sole eligible candidate and
// absorbs every decision before it has warmed up (seen in production: 6 requests in 350ms to a new
// pod whose engine never reported running any). The window runs from when this gateway first saw
// the pod routable (see readySince in pod.go). On by default; 0 disables it.
var leastRequestTopKRampWindow = loadLeastRequestTopKRampWindow()

// loadLeastRequestTopKRampWindow reads leastRequestTopKRampWindowEnv. utils.LoadEnvDuration
// rejects non-positive values and falls back to the (positive) default, so an explicit "0" has to
// be handled here for it to be an off switch.
func loadLeastRequestTopKRampWindow() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(leastRequestTopKRampWindowEnv)); err == nil && d == 0 {
		klog.Infof("set %s: 0, ramp penalty disabled", leastRequestTopKRampWindowEnv)
		return 0
	}
	return utils.LoadEnvDuration(leastRequestTopKRampWindowEnv, leastRequestTopKRampWindowDefault)
}

// LeastRequestTopKRouter picks uniformly at random among the ready pods that are both among the K
// least-loaded and within leastRequestTopKEpsilon of the minimum, instead of always the minimum.
// Unlike power-of-two, every candidate comes from the least-loaded end of the fleet.
//
// It is a types.Router (Route, as a standalone strategy) and a types.PodScorer (ScoreAll, for
// blending with other strategies).
type LeastRequestTopKRouter struct {
	cache cache.Cache
	// clock lets tests advance ramp decay without sleeping; nil in production (time.Now).
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

// newLeastRequestTopKRouterWithCacheAndClock is the test constructor for deterministic ramp decay.
func newLeastRequestTopKRouterWithCacheAndClock(c cache.Cache, clock func() time.Time) *LeastRequestTopKRouter {
	return &LeastRequestTopKRouter{cache: c, clock: clock}
}

func (r *LeastRequestTopKRouter) now() time.Time {
	if r.clock == nil {
		return time.Now()
	}
	return r.clock()
}

// Route implements [types.Router]: a uniform random pick among the eligible pods.
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

// getReadySinceCounts returns each ready pod's readySince keyed by utils.GeneratePodKey, or nil
// when the cache has no ramp info. It fails open the opposite way to getRequestCounts: missing
// means "not ramping", never "ramping".
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

// applyRampAdjustment adds a penalty to each pod still inside its ramp window: the lowest count
// among the pods that are not ramping, decaying linearly to 0 over leastRequestTopKRampWindow. It
// returns counts unchanged when ramping is disabled, nothing is ramping, or every pod is (a
// just-restarted gateway sees the whole fleet as new, leaving no warm baseline to ramp toward).
//
// The floor must come from the non-ramping pods: the ramping pod is usually the true minimum, so
// a minimum over all pods would equal its own count and the penalty would always be 0.
//
// This runs on every routing decision, so the LatestPodReadySince check is a single atomic load
// that skips the per-pod work once the fleet is warm.
func applyRampAdjustment(c cache.Cache, now time.Time, readyPods []*v1.Pod, counts map[string]int) map[string]int {
	window := leastRequestTopKRampWindow
	if window <= 0 {
		return counts
	}
	nowNanos := now.UnixNano()
	if provider, ok := c.(cache.PodReadySinceProvider); ok {
		if latest := provider.LatestPodReadySince(); latest == 0 || time.Duration(nowNanos-latest) >= window {
			// Every pod's readySince is at least this old, so none can be ramping.
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

// eligibleLeastLoaded returns the prefix of readyPods, ranked by running-request count, that is
// within the top K and within epsilon of the minimum, plus the per-pod counts (keyed by
// utils.GeneratePodKey) and the minimum. Pods are shuffled before the stable sort so ties don't
// favor readyPods order. Counts are ramp-adjusted first, so a new pod's zero doesn't anchor the
// epsilon window. Shared by Route and ScoreAll.
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

	// Sorted ascending, so once one pod exceeds min+epsilon every later one does too.
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

// ScoreAll implements [types.PodScorer] so the strategy can be blended with others. The blend
// sums normalized scores deterministically and takes the argmax, so Route's random pick has no
// equivalent there: identical scores would let the other strategy's tie-break favor the same pod
// every time. Instead each eligible pod scores its count minus fresh random jitter in
// [0, epsilon), which can reorder the eligible cluster but never lift an ineligible pod above
// it. Ineligible pods keep their plain count.
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

// Polarity implements [types.PodScorer]: fewer requests is better, as for least-request.
func (r *LeastRequestTopKRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

// SubscribedMetrics implements [types.Router]. It reads the live cross-gateway running-request
// count, not a scraped metric, so there is nothing to subscribe to.
func (r *LeastRequestTopKRouter) SubscribedMetrics() []string {
	return []string{}
}

var _ types.Router = (*LeastRequestTopKRouter)(nil)
var _ types.PodScorer = (*LeastRequestTopKRouter)(nil)
