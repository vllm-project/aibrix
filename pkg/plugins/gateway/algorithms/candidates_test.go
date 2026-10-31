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
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func candidatePods(n int) []*v1.Pod {
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = makeLBPod(fmt.Sprintf("pod-%03d", i), fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	return pods
}

func candidateCtx(t *testing.T, resolved string) *types.RoutingContext {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(resolved), "m1", "hello", "req", "")
	t.Cleanup(ctx.Delete)
	ctx.ResolvedStrategy = resolved
	return ctx
}

func requireDistinctSubset(t *testing.T, got, ready []*v1.Pod) {
	t.Helper()
	inReady := make(map[*v1.Pod]bool, len(ready))
	for _, pod := range ready {
		inReady[pod] = true
	}
	seen := make(map[*v1.Pod]bool, len(got))
	for _, pod := range got {
		require.True(t, inReady[pod], "candidate %s is not a ready pod", pod.Name)
		require.False(t, seen[pod], "candidate %s appears twice", pod.Name)
		seen[pod] = true
	}
}

// estimator is a strategy whose candidate estimate of pod i is scores[pod].
type estimator struct {
	scores   map[*v1.Pod]float64
	polarity types.Polarity
	err      error
	calls    int
}

func (e *estimator) Route(*types.RoutingContext, types.PodList) (string, error) { return "", nil }
func (e *estimator) ScoreAll(*types.RoutingContext, types.PodList) ([]float64, []bool, error) {
	panic("the candidate stage must not call ScoreAll")
}
func (e *estimator) Polarity() types.Polarity { return e.polarity }
func (e *estimator) CandidateScores(_ *types.RoutingContext, list types.PodList) ([]float64, []bool, error) {
	e.calls++
	if e.err != nil {
		return nil, nil, e.err
	}
	pods := list.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))
	for i, pod := range pods {
		scores[i], scored[i] = e.scores[pod], true
	}
	return scores, scored, nil
}

// scoreByIndex scores pods[i] as i.
func scoreByIndex(pods []*v1.Pod, polarity types.Polarity) *estimator {
	e := &estimator{scores: make(map[*v1.Pod]float64, len(pods)), polarity: polarity}
	for i, pod := range pods {
		e.scores[pod] = float64(i)
	}
	return e
}

// plainRouter routes but has no estimate, like random.
type plainRouter struct{}

func (plainRouter) Route(*types.RoutingContext, types.PodList) (string, error) { return "", nil }

// plainScorer scores but has no estimate.
type plainScorer struct{}

func (plainScorer) ScoreAll(*types.RoutingContext, types.PodList) ([]float64, []bool, error) {
	panic("the candidate stage must not call ScoreAll")
}
func (plainScorer) Polarity() types.Polarity { return types.PolarityLeast }

func blendOf(parts map[string]types.PodScorer, weights map[string]int) *multiStrategyRouter {
	m := &multiStrategyRouter{config: &MultiRouterConfig{}, scorers: parts}
	for _, name := range []string{"a", "b", "c"} {
		if w, ok := weights[name]; ok {
			m.config.Items = append(m.config.Items, RouterItem{Name: name, Coefficient: w})
		}
	}
	return m
}

func TestSelectCandidates_KeepsAllPodsWhenNotApplicable(t *testing.T) {
	pods := candidatePods(40)
	router := scoreByIndex(pods, types.PolarityMost)
	cases := []struct {
		name     string
		resolved string
		k        int
	}{
		{"disabled", "least-request:1,load-balance:1", 0},
		{"negative K", "load-balance", -1},
		{"K covers every pod", "load-balance", 40},
		{"global-view strategy", "vtc-basic", 8},
		{"blend with a global-view strategy", "least-request:1,vtc-basic:1", 8},
		{"session pin", "session-affinity", 8},
		{"unparseable strategy", "", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectCandidates(candidateCtx(t, tc.resolved), router, pods, tc.k)
			assert.Equal(t, pods, got)
		})
	}
	assert.Zero(t, router.calls, "no estimate is computed when the stage does not apply")
}

// The best K - K/4 estimates come first, best first; the other K/4 slots are random pods from
// the rest.
func TestSelectCandidates_KeepsBestEstimatesPlusRandomSlots(t *testing.T) {
	pods := candidatePods(200)
	for _, tc := range []struct {
		polarity types.Polarity
		best     func(rank int) *v1.Pod
	}{
		{types.PolarityMost, func(rank int) *v1.Pod { return pods[199-rank] }},
		{types.PolarityLeast, func(rank int) *v1.Pod { return pods[rank] }},
	} {
		router := scoreByIndex(pods, tc.polarity)
		explored := map[*v1.Pod]bool{}
		for trial := 0; trial < 200; trial++ {
			got := selectCandidates(candidateCtx(t, "load-balance"), router, pods, 16)
			requireDistinctSubset(t, got, pods)
			require.Len(t, got, 16)
			for rank := 0; rank < 12; rank++ {
				require.Same(t, tc.best(rank), got[rank], "rank %d", rank)
			}
			for _, pod := range got[12:] {
				explored[pod] = true
			}
		}
		assert.Greater(t, len(explored), 100, "the random slots reach across the rest of the fleet")
	}
}

// With K below 4 there are no random slots.
func TestSelectCandidates_SmallKIsAllRanked(t *testing.T) {
	pods := candidatePods(50)
	got := selectCandidates(candidateCtx(t, "load-balance"), scoreByIndex(pods, types.PolarityLeast), pods, 3)
	assert.Equal(t, pods[:3], got)
}

// The estimates are blended like the router blends its scores: a prefix holder that is also
// idle beats both a busy holder and an idle pod without the prefix.
func TestSelectCandidates_BlendsStrategiesUnderOneBudget(t *testing.T) {
	pods := candidatePods(100)
	prefix := &estimator{scores: map[*v1.Pod]float64{}, polarity: types.PolarityMost}
	load := &estimator{scores: map[*v1.Pod]float64{}, polarity: types.PolarityLeast}
	for i := 0; i < 10; i++ {
		prefix.scores[pods[i]] = 100 // pods 0-9 hold the prefix
	}
	for i := 0; i < 5; i++ {
		load.scores[pods[i]] = 10 // pods 0-4 are busy
	}
	router := blendOf(map[string]types.PodScorer{"a": prefix, "b": load}, map[string]int{"a": 1, "b": 1})
	for trial := 0; trial < 50; trial++ {
		got := selectCandidates(candidateCtx(t, "prefix-cache:1,load-balance:1"), router, pods, 8)
		requireDistinctSubset(t, got, pods)
		require.Len(t, got, 8)
		assert.ElementsMatch(t, pods[5:10], got[:5], "the idle holders rank first")
	}

	// A weight shifts the balance: with the load signal weighted 3:1, an idle pod without the
	// prefix (0.75) outranks a busy holder (0.25) for the slot after the idle holders (1.0).
	router = blendOf(map[string]types.PodScorer{"a": prefix, "b": load}, map[string]int{"a": 1, "b": 3})
	got := selectCandidates(candidateCtx(t, "prefix-cache:1,load-balance:3"), router, pods, 8)
	assert.ElementsMatch(t, pods[5:10], got[:5])
	for _, pod := range got[5:6] {
		assert.NotContains(t, pods[:5], pod, "a busy holder does not take a ranked slot over an idle pod")
	}
}

// A strategy without an estimate, or whose estimate fails, adds the same 0 to every pod: it
// takes part in the weights but does not reorder pods. With no estimate at all, the K pods are
// drawn at random.
func TestSelectCandidates_StrategiesWithoutEstimates(t *testing.T) {
	pods := candidatePods(60)
	ranked := scoreByIndex(pods, types.PolarityMost)
	failing := &estimator{err: errors.New("no counts"), polarity: types.PolarityLeast}

	router := blendOf(map[string]types.PodScorer{"a": ranked, "b": plainScorer{}, "c": failing}, map[string]int{"a": 1, "b": 5, "c": 5})
	got := selectCandidates(candidateCtx(t, "least-request:1,load-balance:1"), router, pods, 8)
	assert.Equal(t, []*v1.Pod{pods[59], pods[58], pods[57], pods[56], pods[55], pods[54]}, got[:6])
	assert.Equal(t, 1, failing.calls)

	for _, router := range []types.Router{
		plainRouter{},
		nil,
		blendOf(map[string]types.PodScorer{"a": plainScorer{}, "b": failing}, map[string]int{"a": 1, "b": 1}),
	} {
		got := selectCandidates(candidateCtx(t, "random"), router, pods, 8)
		requireDistinctSubset(t, got, pods)
		require.Len(t, got, 8)
	}
}

// Pods the estimates cannot tell apart share the ranked slots across requests, so the holders
// of a hot prefix, or an idle fleet, are not narrowed to the same few pods.
func TestSelectCandidates_SpreadsEqualEstimates(t *testing.T) {
	pods := candidatePods(40)
	holders := &estimator{scores: map[*v1.Pod]float64{}, polarity: types.PolarityMost}
	for _, pod := range pods[:20] {
		holders.scores[pod] = 100
	}
	seen := map[*v1.Pod]bool{}
	for trial := 0; trial < 300; trial++ {
		got := selectCandidates(candidateCtx(t, "prefix-cache"), holders, pods, 4)
		for _, pod := range got[:3] {
			require.Contains(t, pods[:20], pod, "ranked slots go to holders")
			seen[pod] = true
		}
	}
	assert.Len(t, seen, 20, "every holder takes a ranked slot some of the time")
}

// TestSelectCandidates_LargeSample covers the branch that draws from a permutation when the
// sample is more than half of the ready pods.
func TestSelectCandidates_LargeSample(t *testing.T) {
	pods := candidatePods(30)
	for _, router := range []types.Router{plainRouter{}, scoreByIndex(pods, types.PolarityMost)} {
		for trial := 0; trial < 200; trial++ {
			got := selectCandidates(candidateCtx(t, "least-request"), router, pods, 25)
			requireDistinctSubset(t, got, pods)
			require.Len(t, got, 25)
		}
	}
}

// TestSelectCandidates_SamplesUniformly checks that without estimates every pod is about equally
// likely to be a candidate, so the stage does not bias load toward part of the fleet.
func TestSelectCandidates_SamplesUniformly(t *testing.T) {
	const n, k, trials = 50, 5, 20000
	pods := candidatePods(n)
	counts := make(map[*v1.Pod]int, n)
	for trial := 0; trial < trials; trial++ {
		for _, pod := range selectCandidates(candidateCtx(t, "random"), plainRouter{}, pods, k) {
			counts[pod]++
		}
	}
	want := float64(trials*k) / n // 2000
	for _, pod := range pods {
		assert.InDelta(t, want, float64(counts[pod]), want*0.15, "pod %s", pod.Name)
	}
}

func TestBestIndices(t *testing.T) {
	scores := []float64{3, 9, 1, 9, 5}
	got := bestIndices(scores, 3)
	require.Len(t, got, 3)
	assert.ElementsMatch(t, []int{1, 3}, got[:2])
	assert.Equal(t, 4, got[2])
	assert.Len(t, bestIndices(scores, 10), 5)
	assert.Empty(t, bestIndices(scores, 0))
}

// countingCache answers exact running counts (counting the reads) and in-memory estimates.
type countingCache struct {
	cache.Cache
	exact, approx map[string]int64
	exactReads    int
}

func (c *countingCache) GetPodsRunningRequests([]*v1.Pod) (map[string]int64, error) {
	c.exactReads++
	return c.exact, nil
}

func (c *countingCache) GetPodsRunningRequestsApprox([]*v1.Pod) map[string]int64 { return c.approx }

// The load estimates read memory, not Redis, and they never stand in for the exact counts that
// the gate and the router read for the candidates afterwards.
func TestLoadEstimatesReadNoExactCounts(t *testing.T) {
	pods := candidatePods(20)
	c := &countingCache{Cache: cache.NewWithPodsForTest(pods, "m1"), exact: map[string]int64{}, approx: map[string]int64{}}
	for i, pod := range pods {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		c.approx[key] = int64(i) // pod-000 looks idle in memory
		c.exact[key] = 50
	}
	c.exact[utils.GeneratePodKey(pods[0].Namespace, pods[0].Name)] = 0

	lb := &loadBalanceRouter{cache: c}
	lr := least.NewLeastRequestTopKRouterWithCache(c)
	for _, scorer := range []types.CandidateScorer{lb, lr} {
		ctx := candidateCtx(t, "load-balance")
		ctx.StartRunningRequestsSnapshot()
		c.exactReads = 0
		scores, scored, err := scorer.CandidateScores(ctx, &utils.PodArray{Pods: pods})
		require.NoError(t, err)
		require.Len(t, scores, len(pods))
		for i := range scored {
			require.True(t, scored[i], "pod %d is scored", i)
		}
		assert.Less(t, scores[0], scores[19], "the estimate follows the in-memory counts")
		assert.Zero(t, c.exactReads, "estimates read no exact counts")

		counts, err := ctx.RunningRequests(pods[:4], c.GetPodsRunningRequests)
		require.NoError(t, err)
		assert.Equal(t, int64(50), counts[utils.GeneratePodKey(pods[1].Namespace, pods[1].Name)],
			"a later reader gets the exact count, not the estimate")
		assert.Equal(t, 1, c.exactReads)
		ctx.StopRunningRequestsSnapshot()
	}

	// A cache without in-memory counts gives no estimate rather than reading Redis.
	plain := &struct{ cache.Cache }{c}
	for _, scorer := range []types.CandidateScorer{&loadBalanceRouter{cache: plain}, least.NewLeastRequestTopKRouterWithCache(plain)} {
		c.exactReads = 0
		_, _, err := scorer.CandidateScores(candidateCtx(t, "load-balance"), &utils.PodArray{Pods: pods})
		assert.Error(t, err)
		assert.Zero(t, c.exactReads)
	}
}

// Every strategy that the gateway asks for an estimate exposes one, and the ones that read
// running counts take them from memory.
func TestCandidateSafeStrategiesWithEstimates(t *testing.T) {
	c := cache.NewWithPodsForTest(candidatePods(2), "m1")
	for _, scorer := range []types.PodScorer{
		&loadBalanceRouter{cache: c},
		least.NewLeastRequestTopKRouterWithCache(c),
		least.NewLeastKvCacheScorer(c),
	} {
		_, ok := scorer.(types.CandidateScorer)
		assert.True(t, ok, "%T", scorer)
	}
}
