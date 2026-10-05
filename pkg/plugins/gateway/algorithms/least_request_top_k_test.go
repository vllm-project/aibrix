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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Route ranks ready pods by their live running-request count and picks uniformly at random among
// the pods that are both within the top leastRequestTopKCandidates by rank and within
// leastRequestTopKEpsilon of the minimum count. These tests reuse the pod-list and cache fakes
// defined in power_of_two_test.go (po2TestPods, newPo2PodList, po2FakeCache): both routers read
// load through the same shared helpers (getRequestCounts, selectTargetPortForPodWithLeastRequestCount).
//
// Route samples with the global math/rand, which cannot be seeded, so each test asserts something
// that holds for every possible shuffle or tie-break, run over many iterations, rather than a
// single fixed outcome.

func newLrtkCtx(requestID string) *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), RouterLeastRequestTopK, po2TestModel, "", requestID, "")
}

// withTopK sets leastRequestTopKCandidates and leastRequestTopKEpsilon for the duration of the
// calling test, restoring the previous values on cleanup so other tests are not affected.
func withTopK(t *testing.T, k, epsilon int) {
	t.Helper()
	origK, origEpsilon := leastRequestTopKCandidates, leastRequestTopKEpsilon
	leastRequestTopKCandidates = k
	leastRequestTopKEpsilon = epsilon
	t.Cleanup(func() {
		leastRequestTopKCandidates = origK
		leastRequestTopKEpsilon = origEpsilon
	})
}

// withRamp sets leastRequestTopKRampWindow for the duration of the calling test, restoring the
// previous value on cleanup so other tests are not affected. Mirrors withTopK.
func withRamp(t *testing.T, window time.Duration) {
	t.Helper()
	orig := leastRequestTopKRampWindow
	leastRequestTopKRampWindow = window
	t.Cleanup(func() { leastRequestTopKRampWindow = orig })
}

// fakeClock is an injectable clock for ramp-decay tests, mirroring pd/token_load_tracker_test.go's.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lrtkFakeCacheNoReadySince is like po2FakeCache but deliberately does not implement
// cache.PodReadySinceProvider, to exercise applyRampAdjustment's no-op fallback for a cache that
// doesn't support ramp info at all (e.g. a custom or older cache implementation).
type lrtkFakeCacheNoReadySince struct {
	cache.Cache
	running map[string]int64
}

func (c *lrtkFakeCacheNoReadySince) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	counts := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if n, ok := c.running[pod.Name]; ok {
			counts[utils.GeneratePodKey(pod.Namespace, pod.Name)] = n
		}
	}
	return counts, nil
}

// lrtkNamespacedFakeCache reports running counts by pod key rather than by bare pod name like
// po2FakeCache, so two pods sharing a name across namespaces can be given different loads.
type lrtkNamespacedFakeCache struct {
	cache.Cache
	running map[string]int64 // by utils.GeneratePodKey
}

func (c *lrtkNamespacedFakeCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	counts := make(map[string]int64, len(pods))
	for _, pod := range pods {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		if n, ok := c.running[key]; ok {
			counts[key] = n
		}
	}
	return counts, nil
}

// getReadySinceCounts passes GetPodsReadySince's result through keyed by utils.GeneratePodKey,
// the same key getRequestCounts uses -- the two maps are looked up side by side in
// applyRampAdjustment, and a key mismatch makes the whole ramp feature a silent no-op (fail-open,
// no crash), so it's worth pinning down directly.
func TestGetReadySinceCounts_KeyedByPodKey(t *testing.T) {
	pods := po2TestPods("pod1", "pod2")
	fake := &po2FakeCache{readySince: map[string]int64{"pod1": 12345, "pod2": 67890}}

	result := getReadySinceCounts(fake, pods)

	assert.Equal(t, map[string]int64{po2PodKey("pod1"): 12345, po2PodKey("pod2"): 67890}, result)
}

func TestGetReadySinceCounts_OmitsPodsWithNoReadySince(t *testing.T) {
	pods := po2TestPods("pod1", "pod2")
	fake := &po2FakeCache{readySince: map[string]int64{"pod1": 12345}}

	result := getReadySinceCounts(fake, pods)

	assert.Equal(t, map[string]int64{po2PodKey("pod1"): 12345}, result)
}

func TestGetReadySinceCounts_CacheWithoutProvider_ReturnsNil(t *testing.T) {
	pods := po2TestPods("pod1")
	fake := &lrtkFakeCacheNoReadySince{running: map[string]int64{"pod1": 0}}

	assert.Nil(t, getReadySinceCounts(fake, pods))
}

func TestGetReadySinceCounts_EmptyResult_ReturnsNil(t *testing.T) {
	pods := po2TestPods("pod1")
	fake := &po2FakeCache{} // readySince is nil -> GetPodsReadySince returns an empty map

	assert.Nil(t, getReadySinceCounts(fake, pods))
}

// applyRampAdjustment tests use a fixture where podA is the sole ramping pod (real count 0,
// readySince = t0) and podB/podC are already-warm, non-ramping pods (no readySince entry) whose
// real counts are 40 and 32 respectively -- so the warm floor must be 32 (podC), not 40 (podB)
// and, critically, not 0 (podA's own real count): using min-over-all-pods for the floor would
// collapse it to podA's own count and make the penalty always zero, which is exactly the bug
// this fixture is designed to catch. counts is keyed by utils.GeneratePodKey (see po2PodKey), as
// getRequestCounts returns it; readySince is keyed by pod name because po2FakeCache's
// GetPodsReadySince re-keys it.
func rampTestFixture() (pods []*v1.Pod, counts map[string]int, t0 time.Time, readySince map[string]int64) {
	pods = po2TestPods("podA", "podB", "podC")
	counts = map[string]int{po2PodKey("podA"): 0, po2PodKey("podB"): 40, po2PodKey("podC"): 32}
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	readySince = map[string]int64{"podA": t0.UnixNano()}
	return
}

func TestApplyRampAdjustment_Disabled_ReturnsCountsUnchanged(t *testing.T) {
	withRamp(t, 0)
	pods, counts, t0, readySince := rampTestFixture()
	fake := &po2FakeCache{readySince: readySince}

	adjusted := applyRampAdjustment(fake, t0, pods, counts)

	assert.Equal(t, counts, adjusted)
}

func TestApplyRampAdjustment_WholeFleetRamping_ReturnsCountsUnchanged(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, _ := rampTestFixture()
	// Every pod, not just podA, has very recent readySince -- e.g. this gateway just restarted
	// and its informer is doing its initial relist. No warm baseline exists to ramp toward.
	fake := &po2FakeCache{readySince: map[string]int64{
		"podA": t0.UnixNano(), "podB": t0.UnixNano(), "podC": t0.UnixNano(),
	}}

	adjusted := applyRampAdjustment(fake, t0, pods, counts)

	assert.Equal(t, counts, adjusted)
}

func TestApplyRampAdjustment_UntrackedOrZeroReadySince_NotPenalized(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, _ := rampTestFixture()
	// podA explicitly has readySince 0 ("no ramp info" even though present in the map);
	// podB/podC are absent from the map entirely. Both must be treated as fully warm.
	fake := &po2FakeCache{readySince: map[string]int64{"podA": 0}}

	adjusted := applyRampAdjustment(fake, t0, pods, counts)

	assert.Equal(t, counts, adjusted)
}

// Decay at t=0 and each quarter of the 300s window. floor=32 (see rampTestFixture) and
// quarter-window elapsed values were chosen so every expected value -- floor * (window-elapsed)/window
// -- is an exact integer, with no rounding ambiguity.
func TestApplyRampAdjustment_DecaysOverElapsedTime(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, readySince := rampTestFixture()
	fake := &po2FakeCache{readySince: readySince}

	tests := []struct {
		name     string
		elapsed  time.Duration
		wantPodA int
	}{
		{"t=0, full floor penalty", 0, 32},
		{"t=1/4 of window", 75 * time.Second, 24},
		{"t=1/2 of window", 150 * time.Second, 16},
		{"t=3/4 of window", 225 * time.Second, 8},
		{"t=window boundary, penalty gone", 300 * time.Second, 0},
		{"t=past window, penalty gone", 400 * time.Second, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adjusted := applyRampAdjustment(fake, t0.Add(tt.elapsed), pods, counts)
			assert.Equal(t, tt.wantPodA, adjusted[po2PodKey("podA")])
			assert.Equal(t, counts[po2PodKey("podB")], adjusted[po2PodKey("podB")], "non-ramping pod must be untouched")
			assert.Equal(t, counts[po2PodKey("podC")], adjusted[po2PodKey("podC")], "non-ramping pod must be untouched")
		})
	}
}

// A now() earlier than readySince (clock skew, or a fake clock rewinding mid-test) must clamp
// elapsed to zero -- i.e. behave exactly like t=0 -- rather than compute a negative elapsed that
// would push the decay factor above 1 and the penalty past the intended floor.
func TestApplyRampAdjustment_NegativeElapsedClampsToZero(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, readySince := rampTestFixture()
	fake := &po2FakeCache{readySince: readySince}

	adjusted := applyRampAdjustment(fake, t0.Add(-5*time.Second), pods, counts)

	assert.Equal(t, 32, adjusted[po2PodKey("podA")])
}

// applyRampAdjustment's LatestPodReadySince short-circuit must actually skip the per-pod
// GetPodsReadySince call -- not just happen to produce the same result via the slow path -- since
// that call, plus the floor computation and map-allocation that follow it, is exactly the cost
// this check exists to avoid on every routing decision now that ramp is enabled by default.
func TestApplyRampAdjustment_LatestReadySinceEarlyExit_SkipsPerPodReadySinceCall(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, _ := rampTestFixture()

	t.Run("no pod ever known to the cache", func(t *testing.T) {
		fake := &po2FakeCache{} // LatestPodReadySince() == 0

		adjusted := applyRampAdjustment(fake, t0, pods, counts)

		assert.Equal(t, counts, adjusted)
		assert.Zero(t, fake.readySinceCalls, "LatestPodReadySince()==0 must short-circuit before GetPodsReadySince")
	})

	t.Run("latest known readySince is older than the ramp window", func(t *testing.T) {
		// podA's own entry is stale (400s > the 300s window), simulating what
		// LatestPodReadySince aggregates: even though a per-pod entry exists, nothing
		// anywhere is within the window, so the short-circuit must still fire on the
		// aggregate signal without needing to inspect podA's entry specifically.
		staleReadySince := map[string]int64{"podA": t0.Add(-400 * time.Second).UnixNano()}
		fake := &po2FakeCache{readySince: staleReadySince}

		adjusted := applyRampAdjustment(fake, t0, pods, counts)

		assert.Equal(t, counts, adjusted)
		assert.Zero(t, fake.readySinceCalls, "a stale LatestPodReadySince must short-circuit before GetPodsReadySince")
	})

	t.Run("latest known readySince is within the ramp window", func(t *testing.T) {
		_, _, _, readySince := rampTestFixture() // podA's readySince == t0, i.e. elapsed == 0
		fake := &po2FakeCache{readySince: readySince}

		adjusted := applyRampAdjustment(fake, t0, pods, counts)

		assert.Equal(t, 32, adjusted[po2PodKey("podA")], "the fixture's actual ramp math must still run")
		assert.Equal(t, 1, fake.readySinceCalls, "a fresh LatestPodReadySince must not skip GetPodsReadySince")
	})
}

// A cache that doesn't implement PodReadySinceProvider's LatestPodReadySince (or the interface at
// all) must fall through to the pre-optimization no-op path rather than panicking or skipping
// incorrectly -- covers a hypothetical partial implementer, since Go's structural typing would
// otherwise let a cache missing just this one method silently fail the whole type assertion.
func TestApplyRampAdjustment_CacheWithoutLatestPodReadySince_FallsBackToNoOp(t *testing.T) {
	withRamp(t, 300*time.Second)
	pods, counts, t0, _ := rampTestFixture()
	fake := &lrtkFakeCacheNoReadySince{running: map[string]int64{"podA": 0, "podB": 40, "podC": 32}}

	adjusted := applyRampAdjustment(fake, t0, pods, counts)

	assert.Equal(t, counts, adjusted)
}

func TestLeastRequestTopKRouter_NoReadyPods(t *testing.T) {
	router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{})

	addr, err := router.Route(newLrtkCtx("req-empty"), newPo2PodList(nil, nil))

	require.Error(t, err)
	assert.Empty(t, addr)
}

// Unlike power-of-two, which skips the cache entirely when there is nothing to compare,
// least-request-top-k always ranks through eligibleLeastLoaded, so even a single ready pod
// costs one cache read.
func TestLeastRequestTopKRouter_SingleReadyPod(t *testing.T) {
	fake := &po2FakeCache{}
	router := NewLeastRequestTopKRouterWithCache(fake)
	ctx := newLrtkCtx("req-single")

	addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), nil))

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", addr)
	assert.Equal(t, "pod1", ctx.TargetPod().Name)
	assert.Len(t, fake.batches, 1, "least-request-top-k always reads counts, even for one pod")
}

// One routing decision reads every ready pod's count in a single batch: unlike power-of-two,
// which only reads its two sampled pods, top-k must rank the whole fleet to find the least loaded.
func TestLeastRequestTopKRouter_ReadsAllReadyPodsInOneBatch(t *testing.T) {
	fake := &po2FakeCache{running: map[string]int64{}}
	router := NewLeastRequestTopKRouterWithCache(fake)
	names := []string{"pod1", "pod2", "pod3", "pod4", "pod5"}

	_, err := router.Route(newLrtkCtx("req-batch"), newPo2PodList(po2TestPods(names...), nil))

	require.NoError(t, err)
	require.Len(t, fake.batches, 1, "exactly one cache read per routing decision")
	assert.ElementsMatch(t, names, fake.batches[0])
}

// With K == 1 there is nothing to spread over: Route always resolves to a pod with the
// (tied-for-)global-minimum count, whatever epsilon is set to.
func TestLeastRequestTopKRouter_KOfOneAlwaysPicksTheMinimum(t *testing.T) {
	t.Run("unique minimum", func(t *testing.T) {
		withTopK(t, 1, 4)
		router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{
			running: map[string]int64{"pod1": 0, "pod2": 5, "pod3": 9},
		})
		pods := po2TestPods("pod1", "pod2", "pod3")

		for i := 0; i < 20; i++ {
			ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
			_, err := router.Route(ctx, newPo2PodList(pods, nil))
			require.NoError(t, err)
			assert.Equal(t, "pod1", ctx.TargetPod().Name)
		}
	})

	t.Run("tied minimum spreads over the tied pods only", func(t *testing.T) {
		withTopK(t, 1, 4)
		router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{
			running: map[string]int64{"pod1": 0, "pod2": 0, "pod3": 5},
		})
		pods := po2TestPods("pod1", "pod2", "pod3")

		tally := map[string]int{}
		for i := 0; i < 100; i++ {
			ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
			_, err := router.Route(ctx, newPo2PodList(pods, nil))
			require.NoError(t, err)
			tally[ctx.TargetPod().Name]++
		}

		assert.Zero(t, tally["pod3"], "busier pod must never be chosen when K=1: %v", tally)
		assert.Positive(t, tally["pod1"], "%v", tally)
		assert.Positive(t, tally["pod2"], "%v", tally)
	})
}

// Counts are keyed by namespace/name, so two pods sharing a name across namespaces are ranked
// independently: keying by bare pod name would collapse them into one entry and let the busy
// pod's count overwrite the idle one's (or vice versa).
func TestLeastRequestTopKRouter_SameNamePodsInDifferentNamespacesAreRankedSeparately(t *testing.T) {
	withTopK(t, 1, 0)
	pods := po2TestPods("pod1", "pod1")
	pods[1].Namespace = "other"
	router := NewLeastRequestTopKRouterWithCache(&lrtkNamespacedFakeCache{running: map[string]int64{
		utils.GeneratePodKey(po2Namespace, "pod1"): 5,
		utils.GeneratePodKey("other", "pod1"):      0,
	}})

	for i := 0; i < 50; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		assert.Equal(t, "other", ctx.TargetPod().Namespace, "the idle pod must win every pick")
	}
}

// A pod ranked below the top K is never chosen, however wide epsilon is, because rank alone
// already excludes it.
func TestLeastRequestTopKRouter_NeverPicksAPodOutsideTopK(t *testing.T) {
	withTopK(t, 2, 1000)
	router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{running: map[string]int64{
		"pod1": 0, "pod2": 1, "pod3": 2, "pod4": 3, "pod5": 4,
	}})
	pods := po2TestPods("pod1", "pod2", "pod3", "pod4", "pod5")

	tally := map[string]int{}
	for i := 0; i < 100; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		tally[ctx.TargetPod().Name]++
	}

	for _, name := range []string{"pod3", "pod4", "pod5"} {
		assert.Zero(t, tally[name], "rank beyond K must never be chosen: %v", tally)
	}
	assert.Positive(t, tally["pod1"], "%v", tally)
	assert.Positive(t, tally["pod2"], "%v", tally)
}

// Epsilon can shrink the eligible set below K: a pod within the top K by rank but further than
// epsilon from the minimum is excluded even though K alone would allow it.
func TestLeastRequestTopKRouter_EpsilonNarrowsCandidatesWithinK(t *testing.T) {
	withTopK(t, 5, 0)
	router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{running: map[string]int64{
		"pod1": 0, "pod2": 0, "pod3": 1, "pod4": 1, "pod5": 1,
	}})
	pods := po2TestPods("pod1", "pod2", "pod3", "pod4", "pod5")

	tally := map[string]int{}
	for i := 0; i < 100; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		tally[ctx.TargetPod().Name]++
	}

	for _, name := range []string{"pod3", "pod4", "pod5"} {
		assert.Zero(t, tally[name], "outside epsilon of the minimum must never be chosen: %v", tally)
	}
	assert.Positive(t, tally["pod1"], "%v", tally)
	assert.Positive(t, tally["pod2"], "%v", tally)
}

// A negative epsilon disables the closeness bound entirely, leaving only the K-by-rank bound.
func TestLeastRequestTopKRouter_NegativeEpsilonDisablesTheClosenessBound(t *testing.T) {
	withTopK(t, 3, -1)
	router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{running: map[string]int64{
		"pod1": 0, "pod2": 50, "pod3": 100, "pod4": 1000,
	}})
	pods := po2TestPods("pod1", "pod2", "pod3", "pod4")

	tally := map[string]int{}
	for i := 0; i < 150; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		tally[ctx.TargetPod().Name]++
	}

	assert.Zero(t, tally["pod4"], "rank 4 is still excluded by K=3: %v", tally)
	for _, name := range []string{"pod1", "pod2", "pod3"} {
		assert.Positive(t, tally[name],
			"epsilon disabled, so every top-K pod is eligible however far from the minimum: %v", tally)
	}
}

// K <= 0, and K larger than the ready fleet, both fall back to considering every ready pod.
func TestLeastRequestTopKRouter_KFallsBackToAllPods(t *testing.T) {
	tests := []struct {
		name string
		k    int
	}{
		{name: "k is zero", k: 0},
		{name: "k is negative", k: -1},
		{name: "k exceeds the fleet size", k: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTopK(t, tt.k, -1) // epsilon disabled so only the K bound is under test
			router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{running: map[string]int64{
				"pod1": 0, "pod2": 1, "pod3": 2,
			}})
			pods := po2TestPods("pod1", "pod2", "pod3")

			tally := map[string]int{}
			for i := 0; i < 100; i++ {
				ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
				_, err := router.Route(ctx, newPo2PodList(pods, nil))
				require.NoError(t, err)
				tally[ctx.TargetPod().Name]++
			}

			for _, name := range []string{"pod1", "pod2", "pod3"} {
				assert.Positive(t, tally[name], "%v", tally)
			}
		})
	}
}

// A cache that cannot report counts degrades load awareness, not availability: every pod is
// treated as idle, so any of them may be picked.
func TestLeastRequestTopKRouter_CacheFailureTreatsAllPodsAsIdle(t *testing.T) {
	tests := []struct {
		name string
		fake *po2FakeCache
	}{
		{name: "cache error", fake: &po2FakeCache{runningErr: fmt.Errorf("redis unavailable")}},
		{name: "nil counts without an error", fake: &po2FakeCache{nilCounts: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTopK(t, 5, 4)
			router := NewLeastRequestTopKRouterWithCache(tt.fake)
			pods := po2TestPods("pod1", "pod2", "pod3")

			tally := map[string]int{}
			for i := 0; i < 100; i++ {
				ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
				addr, err := router.Route(ctx, newPo2PodList(pods, nil))
				require.NoError(t, err, "routing must fail open when counts are unavailable")
				assert.NotEmpty(t, addr)
				tally[ctx.TargetPod().Name]++
			}

			for _, name := range []string{"pod1", "pod2", "pod3"} {
				assert.Positive(t, tally[name], "%v", tally)
			}
		})
	}
}

// The pod is chosen on its running-request total, and only then is a port chosen within it, from
// the per-port counts of that pod alone -- the same shared helper power-of-two uses.
func TestLeastRequestTopKRouter_ChoosesLeastLoadedPortOfTheChosenPod(t *testing.T) {
	t.Run("one data-parallel pod", func(t *testing.T) {
		withTopK(t, 5, 4)
		fake := &po2FakeCache{portRunning: map[string]float64{
			"pod1/8000": 5, "pod1/8001": 2, "pod1/8002": 9, "pod1/8003": 2,
		}}
		router := NewLeastRequestTopKRouterWithCache(fake)
		ports := map[string][]int{po2PodKey("pod1"): {8000, 8001, 8002, 8003}}

		ctx := newLrtkCtx("req-dp")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), ports))

		require.NoError(t, err)
		assert.Contains(t, []int{8001, 8003}, ctx.TargetPort(), "only the least loaded ports are eligible")
		assert.Equal(t, fmt.Sprintf("10.0.0.1:%d", ctx.TargetPort()), addr)
	})

	t.Run("pod first, then port", func(t *testing.T) {
		// K=1 makes the pod choice deterministic: pod2 must win on its total, and its port
		// must come from pod2's own counts, even though pod1's port counts look emptier.
		withTopK(t, 1, 4)
		fake := &po2FakeCache{
			running: map[string]int64{"pod1": 9, "pod2": 1},
			portRunning: map[string]float64{
				"pod1/8000": 0, "pod1/8001": 0,
				"pod2/8000": 7, "pod2/8001": 1,
			},
		}
		router := NewLeastRequestTopKRouterWithCache(fake)
		ports := map[string][]int{po2PodKey("pod1"): {8000, 8001}, po2PodKey("pod2"): {8000, 8001}}

		ctx := newLrtkCtx("req-dp-pods")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1", "pod2"), ports))

		require.NoError(t, err)
		assert.Equal(t, "pod2", ctx.TargetPod().Name)
		assert.Equal(t, 8001, ctx.TargetPort())
		assert.Equal(t, "10.0.0.2:8001", addr)
	})

	t.Run("single-port pod uses its only port", func(t *testing.T) {
		router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{})

		ctx := newLrtkCtx("req-one-port")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), map[string][]int{po2PodKey("pod1"): {8000}}))

		require.NoError(t, err)
		assert.Equal(t, 8000, ctx.TargetPort())
		assert.Equal(t, "10.0.0.1:8000", addr)
	})

	t.Run("pod without listed ports falls back to its default port", func(t *testing.T) {
		router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{})

		ctx := newLrtkCtx("req-no-ports")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), map[string][]int{po2PodKey("pod1"): {}}))

		require.NoError(t, err)
		assert.Zero(t, ctx.TargetPort(), "no port was chosen, so the default is used")
		assert.Equal(t, "10.0.0.1:8000", addr)
	})
}

func TestLeastRequestTopKRouter_SubscribesToNothing(t *testing.T) {
	assert.Equal(t, []string{}, NewLeastRequestTopKRouterWithCache(nil).SubscribedMetrics())
}

func TestLeastRequestTopKRouter_PolarityIsLeast(t *testing.T) {
	assert.Equal(t, types.PolarityLeast, NewLeastRequestTopKRouterWithCache(nil).Polarity())
}

// ScoreAll scores every pod with its running count; only pods that are both within the top K by
// rank and within epsilon of the minimum get random jitter in [0, epsilon), so jitter can reorder
// the eligible cluster but never promote an ineligible pod. With K=2 and epsilon=3 over counts
// 0/2/3/100, pod1 and pod2 are eligible; pod3 is within epsilon but ranked third, and pod4 is
// far from the minimum.
func TestLeastRequestTopKRouter_ScoreAllJittersOnlyEligiblePods(t *testing.T) {
	withTopK(t, 2, 3)
	withRamp(t, 0)
	router := NewLeastRequestTopKRouterWithCache(&po2FakeCache{running: map[string]int64{
		"pod1": 0, "pod2": 2, "pod3": 3, "pod4": 100,
	}})
	pods := po2TestPods("pod1", "pod2", "pod3", "pod4")

	jittered := map[string]bool{}
	for i := 0; i < 50; i++ {
		scores, scored, err := router.ScoreAll(newLrtkCtx(fmt.Sprintf("req-%d", i)), newPo2PodList(pods, nil))

		require.NoError(t, err)
		assert.Equal(t, []bool{true, true, true, true}, scored)
		assert.Greater(t, scores[0], -3.0)
		assert.LessOrEqual(t, scores[0], 0.0)
		assert.Greater(t, scores[1], -1.0)
		assert.LessOrEqual(t, scores[1], 2.0)
		assert.Equal(t, 3.0, scores[2], "ineligible pod keeps its plain count")
		assert.Equal(t, 100.0, scores[3], "ineligible pod keeps its plain count")
		jittered["pod1"] = jittered["pod1"] || scores[0] != 0
		jittered["pod2"] = jittered["pod2"] || scores[1] != 2
	}
	assert.True(t, jittered["pod1"] && jittered["pod2"], "eligible pods must actually get jitter: %v", jittered)
}

// leastRequestTopKRampWindow ships enabled by default (see its doc comment) -- the production
// incident that motivated this feature showed the pileup happening in practice, so the fix is
// active out of the box rather than requiring opt-in. Every other ramp test below explicitly sets
// the window via withRamp regardless (matching this file's existing withTopK convention of never
// relying on an ambient package-var default), so this is the only place the actual default value
// is pinned down.
func TestLeastRequestTopKRampWindow_DefaultIsFiveMinutesEnabled(t *testing.T) {
	assert.Equal(t, 300*time.Second, leastRequestTopKRampWindow)
}

// utils.LoadEnvDuration alone rejects non-positive values and falls back to the (positive)
// default, so "0" would leave the ramp enabled; loadLeastRequestTopKRampWindow handles it first
// so the documented off switch actually works.
func TestLoadLeastRequestTopKRampWindow(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses the five-minute default", env: "", want: 300 * time.Second},
		{name: "explicit duration", env: "45s", want: 45 * time.Second},
		{name: "zero disables the ramp", env: "0", want: 0},
		{name: "zero with a unit disables the ramp", env: "0s", want: 0},
		{name: "negative is invalid and falls back to the default", env: "-5s", want: 300 * time.Second},
		{name: "unparseable falls back to the default", env: "soon", want: 300 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(leastRequestTopKRampWindowEnv, tt.env)

			assert.Equal(t, tt.want, loadLeastRequestTopKRampWindow())
		})
	}
}

// With ramp explicitly disabled, readySince data being present in the cache must have zero
// effect: podA (real count 0) dominates every pick, exactly reproducing the pre-fix pileup this
// feature exists to solve. This is the end-to-end companion to
// TestApplyRampAdjustment_Disabled_ReturnsCountsUnchanged.
func TestLeastRequestTopKRouter_RampDisabled_NewPodStillDominates(t *testing.T) {
	withTopK(t, 1, 4)
	withRamp(t, 0)
	clock := newFakeClock()
	fake := &po2FakeCache{
		running:    map[string]int64{"podA": 0, "podB": 32},
		readySince: map[string]int64{"podA": clock.Now().UnixNano()},
	}
	router := newLeastRequestTopKRouterWithCacheAndClock(fake, clock.Now)
	pods := po2TestPods("podA", "podB")

	for i := 0; i < 20; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		assert.Equal(t, "podA", ctx.TargetPod().Name,
			"with ramp disabled, the real-count minimum always wins")
	}
}

// With ramp enabled, podA's effective count at t=0 ties the fleet's proven-best pod (podB) instead
// of sitting at a lone-outlier zero, so K=1 no longer deterministically resolves to podA on every
// decision -- podB must win some share of picks too.
func TestLeastRequestTopKRouter_RampEnabled_PreventsNewPodFromDominating(t *testing.T) {
	withTopK(t, 1, 4)
	withRamp(t, 300*time.Second)
	clock := newFakeClock()
	fake := &po2FakeCache{
		running:    map[string]int64{"podA": 0, "podB": 32},
		readySince: map[string]int64{"podA": clock.Now().UnixNano()},
	}
	router := newLeastRequestTopKRouterWithCacheAndClock(fake, clock.Now)
	pods := po2TestPods("podA", "podB")

	tally := map[string]int{}
	for i := 0; i < 200; i++ {
		ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		tally[ctx.TargetPod().Name]++
	}

	assert.Positive(t, tally["podA"], "%v", tally)
	assert.Positive(t, tally["podB"],
		"ramp should tie podA with the fleet's proven-best pod instead of letting it dominate every pick: %v", tally)
}

// As elapsed time advances toward the ramp window, podA's penalty decays and its selection share
// should not decrease -- it becomes progressively more (never less) attractive relative to the
// still-busy fleet.
func TestLeastRequestTopKRouter_RampEnabled_SelectionShareGrowsAsRampDecays(t *testing.T) {
	withTopK(t, 1, 4)
	withRamp(t, 300*time.Second)
	clock := newFakeClock()
	fake := &po2FakeCache{
		running:    map[string]int64{"podA": 0, "podB": 32},
		readySince: map[string]int64{"podA": clock.Now().UnixNano()},
	}
	router := newLeastRequestTopKRouterWithCacheAndClock(fake, clock.Now)
	pods := po2TestPods("podA", "podB")

	tallyAt := func() int {
		tally := 0
		for i := 0; i < 200; i++ {
			ctx := newLrtkCtx(fmt.Sprintf("req-%d", i))
			_, err := router.Route(ctx, newPo2PodList(pods, nil))
			require.NoError(t, err)
			if ctx.TargetPod().Name == "podA" {
				tally++
			}
		}
		return tally
	}

	early := tallyAt() // t=0: podA tied with podB (effective 32 vs 32)
	clock.Advance(240 * time.Second)
	late := tallyAt() // t=240s (4/5 of the 300s window): podA's effective count down to 6, well under podB's 32

	assert.GreaterOrEqual(t, late, early,
		"podA's share of picks should not shrink as its ramp penalty decays: early=%d late=%d", early, late)
	assert.Greater(t, late, 150, "by t=240s podA should be winning nearly every pick: late=%d/200", late)
}

// ScoreAll (the blended-strategy path) must apply the same ramp adjustment as Route: without
// ramp, podA's real count (0) sits far below podB's (32) even after jitter, but with ramp enabled
// at t=0 they're tied at the adjusted floor, so their scores must land close together.
func TestLeastRequestTopKRouter_ScoreAllAppliesRampAdjustment(t *testing.T) {
	withTopK(t, 5, 4)
	withRamp(t, 300*time.Second)
	clock := newFakeClock()
	fake := &po2FakeCache{
		running:    map[string]int64{"podA": 0, "podB": 32},
		readySince: map[string]int64{"podA": clock.Now().UnixNano()},
	}
	router := newLeastRequestTopKRouterWithCacheAndClock(fake, clock.Now)
	pods := po2TestPods("podA", "podB")

	scores, scored, err := router.ScoreAll(newLrtkCtx("req-scoreall"), newPo2PodList(pods, nil))

	require.NoError(t, err)
	require.True(t, scored[0] && scored[1])
	assert.InDelta(t, scores[0], scores[1], 5,
		"ramp should put podA's score close to podB's, not ~32 apart as it would without ramp: %v", scores)
}
