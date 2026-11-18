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

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/blacklist"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// candidateBlacklistFleet is n ready pods of model "m" in a real cache, a server over it whose
// breaker confirms every trip, and the routing config that sets the candidate stage to k.
type candidateBlacklistFleet struct {
	t       *testing.T
	server  *Server
	pods    []*v1.Pod
	profile *types.ResolvedConfigProfile
}

func newCandidateBlacklistFleet(t *testing.T, n, k int) *candidateBlacklistFleet {
	t.Helper()
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("pod-%03d", i), Namespace: "ns",
				Labels: map[string]string{constants.ModelLabelPort: "8000"},
			},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	raw := json.RawMessage(fmt.Sprintf(`{"candidate":{"k":%d}}`, k))
	return &candidateBlacklistFleet{
		t:       t,
		server:  &Server{cache: store, routerManager: rm, podBlacklist: blacklist.PodBlacklist{}},
		pods:    pods,
		profile: &types.ResolvedConfigProfile{RoutingConfig: raw, Routing: configprofiles.ParseRoutingConfig(raw)},
	}
}

// blacklist trips the breaker of every pod for which excluded reports true.
func (f *candidateBlacklistFleet) blacklist(excluded func(i int) bool) {
	f.t.Helper()
	for i, pod := range f.pods {
		if excluded(i) {
			tripBreaker(&f.server.podBlacklist, pod)
			require.True(f.t, f.server.podBlacklist.IsBlacklisted(pod), pod.Name)
		}
	}
}

// route runs one least-request decision over the whole fleet and returns the address it chose.
func (f *candidateBlacklistFleet) route() (string, error) {
	f.t.Helper()
	ctx := types.NewRoutingContext(context.Background(), least.RouterLeastRequest, "m", "hello", "req", "")
	f.t.Cleanup(ctx.Delete)
	ctx.ConfigProfile = f.profile
	return f.server.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: f.pods}, "")
}

// addrOf is the address a request to pod i is routed to.
func (f *candidateBlacklistFleet) addrOf(i int) string {
	return fmt.Sprintf("%s:8000", f.pods[i].Status.PodIP)
}

// With the candidate stage on, the blacklist filters the K candidates instead of the fleet, so a
// request must still never reach an excluded pod while a healthy one exists: neither when some of
// its candidates are excluded, nor when all of them are.
func Test_selectTargetPod_CandidateStageNeverRoutesToBlacklistedPod(t *testing.T) {
	// These fleets are mostly excluded, past the cap that would route across them all.
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	tests := []struct {
		name    string
		n, k    int
		healthy func(i int) bool
	}{
		// 30 of 40 excluded with K=4: about a third of the draws hold only excluded pods.
		{"most of the fleet excluded", 40, 4, func(i int) bool { return i%4 == 0 }},
		// One healthy pod of 50 with K=4: it is among the candidates in about 8% of the draws, so
		// almost every request finds its whole candidate set excluded.
		{"one healthy pod", 50, 4, func(i int) bool { return i == 17 }},
		{"half excluded", 40, 8, func(i int) bool { return i%2 == 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCandidateBlacklistFleet(t, tt.n, tt.k)
			f.blacklist(func(i int) bool { return !tt.healthy(i) })
			healthy := map[string]bool{}
			for i := range f.pods {
				if tt.healthy(i) {
					healthy[f.addrOf(i)] = true
				}
			}

			for i := 0; i < 300; i++ {
				addr, err := f.route()
				require.NoError(t, err)
				require.True(t, healthy[addr], "request %d was routed to %s, an excluded pod", i, addr)
			}
		})
	}
}

// Every ready pod excluded is the one case where the blacklist is ignored: the request still
// routes, as it did when the whole fleet was filtered first.
func Test_selectTargetPod_CandidateStageAllBlacklistedStillRoutes(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	f := newCandidateBlacklistFleet(t, 30, 4)
	f.blacklist(func(int) bool { return true })

	for i := 0; i < 20; i++ {
		addr, err := f.route()
		require.NoError(t, err)
		require.NotEmpty(t, addr)
	}
}

// Under the default cap a fleet with a minority excluded still never sends a request to an
// excluded pod, even though a sample of K candidates can be mostly excluded by chance: the stage
// then runs again over the pods that are not.
func Test_selectTargetPod_CandidateStageUnderTheCapNeverRoutesToBlacklistedPod(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(50))
	f := newCandidateBlacklistFleet(t, 40, 8)
	f.blacklist(func(i int) bool { return i%4 == 0 }) // a quarter of the fleet
	excluded := map[string]bool{}
	for i := range f.pods {
		if i%4 == 0 {
			excluded[f.addrOf(i)] = true
		}
	}

	for i := 0; i < 400; i++ {
		addr, err := f.route()
		require.NoError(t, err)
		require.False(t, excluded[addr], "request %d was routed to %s, an excluded pod", i, addr)
	}
}

// Past the cap the blacklist is ignored: most of the fleet excluded is more likely a wrong
// verdict about many pods than most of a fleet failing, and the few pods left would carry
// everything. The request is routed across the pods as if none were excluded, and counted.
func Test_selectTargetPod_CandidateStageOverTheCapRoutesAcrossAllPods(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(50))
	counts := captureFallbackCounts(t)
	f := newCandidateBlacklistFleet(t, 40, 8)
	f.blacklist(func(i int) bool { return i%4 != 0 }) // three quarters of the fleet
	excluded := map[string]bool{}
	for i := range f.pods {
		if i%4 != 0 {
			excluded[f.addrOf(i)] = true
		}
	}

	toExcluded := 0
	for i := 0; i < 300; i++ {
		addr, err := f.route()
		require.NoError(t, err)
		if excluded[addr] {
			toExcluded++
		}
	}

	assert.Positive(t, toExcluded, "the blacklist is ignored, so the excluded pods are routed to again")
	// The cap is judged on the candidates a request is given, so a sample of 8 that happens to hold
	// at most half excluded pods keeps its exclusions: with three quarters of the fleet excluded
	// that is about 11% of requests (a binomial 8, 0.75), so nearly all fall back, not all.
	assert.GreaterOrEqual(t, counts.get(fallbackOverCap), 200, "most requests are counted")
	assert.Zero(t, counts.get(fallbackAllExcluded))
}

// The replica-inflight filter runs after the blacklist's, so it must not see an excluded pod,
// which could be the only replica with room left. Here the one healthy replica is full and the
// excluded one is not; the request is rejected rather than sent to the excluded pod. That holds
// with the blacklist filtering after the candidate stage too: SelectCandidates returns both pods
// when K is off, the blacklist then drops the excluded one, and the inflight filter reads only
// what is left.
func Test_selectTargetPod_BlacklistFiltersAheadOfReplicaInflight(t *testing.T) {
	healthy := podWithReplicaInflight("a", 1)
	excluded := podWithReplicaInflight("b", 1)
	mockCache := &MockCache{}
	// Exactly [healthy]: were the excluded pod still in the list, this call would not match.
	mockCache.On("GetPodsRunningRequests", []*v1.Pod{healthy}).Return(map[string]int64{"ns/a": 1}, nil)
	// Same as Test_selectTargetPod_ReplicaInflightSaturated: Select needs a manager that
	// resolves least-request, not the default one which only knows strategies other tests
	// happened to register.
	manager := routing.NewRouterManagerWithCache(mockCache)
	manager.Init()
	s := &Server{cache: mockCache, routerManager: manager, podBlacklist: blacklist.PodBlacklist{}}
	tripBreaker(&s.podBlacklist, excluded)
	require.True(t, s.podBlacklist.IsBlacklisted(excluded))

	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "r1", "")
	pods := []*v1.Pod{healthy, excluded}
	applyConfigProfile(routingCtx, pods)
	require.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsInflight)

	_, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded), "got %v", err)
	mockCache.AssertExpectations(t)
}

// excludeBlacklistedCandidates is excludeBlacklisted when the stage did not narrow: the
// candidates are the ready list itself.
func Test_excludeBlacklistedCandidates_NotNarrowed(t *testing.T) {
	a, b := blacklistTestPod("a"), blacklistTestPod("b")
	s := &Server{podBlacklist: blacklist.PodBlacklist{}}
	tripBreaker(&s.podBlacklist, b)
	ctx := types.NewRoutingContext(context.Background(), routing.RouterRandom, "m", "", "r1", "")
	ready := []*v1.Pod{a, b}

	assert.Equal(t, []*v1.Pod{a}, s.excludeBlacklistedCandidates(ctx, nil, ready, ready))

	tripBreaker(&s.podBlacklist, a)
	assert.Equal(t, ready, s.excludeBlacklistedCandidates(ctx, nil, ready, ready),
		"with every ready pod excluded the candidates are kept as they are")
}

// inflightReads is a cache that records how many pods the widest running-request read covered,
// which is what the replica-inflight filter reads, and answers from running when it is set
// instead of from the store's own counts, which are all zero.
type inflightReads struct {
	cache.Cache
	running map[string]int64

	mu     sync.Mutex
	widest int
}

func (c *inflightReads) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	c.mu.Lock()
	if len(pods) > c.widest {
		c.widest = len(pods)
	}
	c.mu.Unlock()
	if c.running == nil {
		return c.Cache.GetPodsRunningRequests(pods)
	}
	counts := make(map[string]int64, len(pods))
	for _, pod := range pods {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		counts[key] = c.running[key]
	}
	return counts, nil
}

// limitInflight gives the fleet's requests a per-replica inflight limit and the counts the
// filter reads for them, and returns the cache that records those reads.
func (f *candidateBlacklistFleet) limitInflight(limit int64, running map[string]int64) *inflightReads {
	f.t.Helper()
	f.profile.RequestsInflight = limit
	reads := &inflightReads{Cache: f.server.cache, running: running}
	f.server.cache = reads
	return reads
}

// A replica-inflight limit does not take the blacklist off the candidate stage: the filter runs
// on the K candidates after the blacklist's does, so the request still never reaches an excluded
// pod, including when every one of its candidates is excluded and the stage runs again, and the
// filter reads no more than the K candidates.
func Test_selectTargetPod_CandidateStageWithReplicaInflightNeverRoutesToBlacklistedPod(t *testing.T) {
	// One healthy pod of 50 with K=4: almost every request finds its whole candidate set excluded.
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	f := newCandidateBlacklistFleet(t, 50, 4)
	f.blacklist(func(i int) bool { return i != 17 })
	reads := f.limitInflight(1000, nil)

	for i := 0; i < 300; i++ {
		addr, err := f.route()
		require.NoError(t, err)
		require.Equal(t, f.addrOf(17), addr, "request %d was routed to an excluded pod", i)
	}

	reads.mu.Lock()
	defer reads.mu.Unlock()
	assert.Positive(t, reads.widest, "the limit is checked")
	assert.LessOrEqual(t, reads.widest, 4, "the filter reads the candidates, not the ready pods")
}

// The same with the healthy replica full: the request is rejected, however the stage drew its
// candidates, and never sent to an excluded pod that has room.
func Test_selectTargetPod_CandidateStageWithReplicaInflightRejectsWhenOnlyTheHealthyPodIsFull(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	f := newCandidateBlacklistFleet(t, 50, 4)
	f.blacklist(func(i int) bool { return i != 17 })
	f.limitInflight(2, map[string]int64{"ns/pod-017": 2})

	for i := 0; i < 300; i++ {
		addr, err := f.route()
		require.True(t, errors.Is(err, errReplicaInflightExceeded), "request %d: got %q, %v", i, addr, err)
	}
}

// candidateSelectionsFor is the sum of aibrix_routing_candidate_selections_total for model.
func candidateSelectionsFor(t *testing.T, model string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	total := 0.0
	for _, family := range families {
		if family.GetName() != "aibrix_routing_candidate_selections_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "model" && label.GetValue() == model {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// Running the stage again over the pods the blacklist did not exclude is part of the same
// decision, so a request is counted as narrowed once, not once per pass.
func Test_selectTargetPod_CandidateStageRerunIsCountedOnce(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	// Ten healthy pods of 100 with K=4: the candidates of about two requests in three are all
	// excluded, and the stage then runs again over ten pods, more than K, so it narrows again.
	f := newCandidateBlacklistFleet(t, 100, 4)
	f.blacklist(func(i int) bool { return i%10 != 0 })
	const requests = 200

	before := candidateSelectionsFor(t, "m")
	for i := 0; i < requests; i++ {
		_, err := f.route()
		require.NoError(t, err)
	}

	assert.Equal(t, float64(requests), candidateSelectionsFor(t, "m")-before)
}
