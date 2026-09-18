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
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Route samples its two pods with the global math/rand, which cannot be seeded, so no test
// here depends on which pods were sampled. Each asserts something that holds for every
// possible sample: with exactly two pods both are always sampled, and a pod with a strictly
// higher count never wins a comparison.
//
// There are two kinds of test. The ones on po2FakeCache check the routing decision itself, and
// that the counts come from Cache.GetPodsRunningRequests. The ones on po2Gateway run Route
// against a real cache.Store and make the calls the gateway makes around it (AddRequestCount
// after routing, DoneRequestCount on completion), so they also cover the counter the router
// depends on.

const (
	po2TestModel = "test-model"
	po2Namespace = "default"
)

// po2TestPods returns ready single-port pods named names, at 10.0.0.1, 10.0.0.2, ... on port 8000.
func po2TestPods(names ...string) []*v1.Pod {
	pods := make([]*v1.Pod, len(names))
	for i, name := range names {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: po2Namespace,
				Labels: map[string]string{
					constants.ModelLabelName: po2TestModel,
					constants.ModelLabelPort: "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	return pods
}

// po2PodList is a mockPodList that also reports per-pod ports, the way utils.PodArray does for
// pods carrying the model.aibrix.ai/port label.
type po2PodList struct {
	*mockPodList
	ports map[string][]int
}

func (l *po2PodList) ListPortsForPod() map[string][]int { return l.ports }

func newPo2PodList(pods []*v1.Pod, ports map[string][]int) *po2PodList {
	return &po2PodList{mockPodList: newMockPodList(pods, nil), ports: ports}
}

func newPo2Ctx(requestID string) *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), RouterPowerOfTwo, po2TestModel, "", requestID, "")
}

// po2FakeCache serves counts from maps. It embeds a nil cache.Cache, so a Cache method the
// router is not supposed to call panics instead of returning something plausible.
type po2FakeCache struct {
	cache.Cache

	// running is what GetPodsRunningRequests reports, by pod name. A pod not in it is missing
	// from the result, as for a pod the cache does not know.
	running    map[string]int64
	runningErr error
	// nilCounts makes GetPodsRunningRequests answer (nil, nil), as a custom or mock cache might.
	nilCounts bool
	// portRunning is the per-port realtime metric of data-parallel pods, by "<pod>/<port>".
	portRunning map[string]float64

	batches [][]string // pod names of every GetPodsRunningRequests call, in order
}

func (c *po2FakeCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	c.batches = append(c.batches, names)
	if c.runningErr != nil {
		return nil, c.runningErr
	}
	if c.nilCounts {
		return nil, nil
	}
	counts := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if n, ok := c.running[pod.Name]; ok {
			counts[utils.GeneratePodKey(pod.Namespace, pod.Name)] = n
		}
	}
	return counts, nil
}

func (c *po2FakeCache) GetMetricValueByPod(podName, _, metricName string) (metrics.MetricValue, error) {
	if port, ok := strings.CutPrefix(metricName, metrics.RealtimeNumRequestsRunning+"/"); ok {
		if n, found := c.portRunning[podName+"/"+port]; found {
			return &metrics.SimpleMetricValue{Value: n}, nil
		}
	}
	return nil, fmt.Errorf("no metric %s for pod %s", metricName, podName)
}

func TestPowerOfTwoRouter_NoReadyPods(t *testing.T) {
	router := NewPowerOfTwoRouterWithCache(&po2FakeCache{})

	addr, err := router.Route(newPo2Ctx("req-empty"), newPo2PodList(nil, nil))

	require.Error(t, err)
	assert.Empty(t, addr)
}

func TestPowerOfTwoRouter_SingleReadyPodIsChosenWithoutReadingCounts(t *testing.T) {
	fake := &po2FakeCache{}
	router := NewPowerOfTwoRouterWithCache(fake)
	ctx := newPo2Ctx("req-single")

	addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), nil))

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", addr)
	assert.Equal(t, "pod1", ctx.TargetPod().Name)
	assert.Empty(t, fake.batches, "there is nothing to compare, so no count should be read")
}

// With two pods both are always sampled, so the pod with the lower count must win every time.
func TestPowerOfTwoRouter_PrefersLessLoadedPod(t *testing.T) {
	tests := []struct {
		name    string
		running map[string]int64
		wantPod string // empty: a tie, either is acceptable
	}{
		{name: "second is busier", running: map[string]int64{"pod1": 1, "pod2": 5}, wantPod: "pod1"},
		{name: "first is busier", running: map[string]int64{"pod1": 5, "pod2": 1}, wantPod: "pod2"},
		{name: "idle beats busy", running: map[string]int64{"pod1": 0, "pod2": 7}, wantPod: "pod1"},
		{name: "busy loses to idle", running: map[string]int64{"pod1": 7, "pod2": 0}, wantPod: "pod2"},
		{name: "off by one is still decisive", running: map[string]int64{"pod1": 3, "pod2": 4}, wantPod: "pod1"},
		{name: "pod without a count is idle", running: map[string]int64{"pod2": 4}, wantPod: "pod1"},
		{name: "no counts at all", running: map[string]int64{}},
		{name: "tie", running: map[string]int64{"pod1": 3, "pod2": 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewPowerOfTwoRouterWithCache(&po2FakeCache{running: tt.running})
			ctx := newPo2Ctx("req-1")

			addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1", "pod2"), nil))

			require.NoError(t, err)
			chosen := ctx.TargetPod().Name
			assert.Contains(t, []string{"pod1", "pod2"}, chosen)
			if tt.wantPod != "" {
				assert.Equal(t, tt.wantPod, chosen)
			}
			assert.Equal(t, map[string]string{"pod1": "10.0.0.1:8000", "pod2": "10.0.0.2:8000"}[chosen], addr)
		})
	}
}

// One routing decision costs one cache read, for the two sampled pods only: a router that
// scored every ready pod, or read the pods one by one, would be as slow as a scan.
func TestPowerOfTwoRouter_ReadsOnlyTheTwoSampledPodsInOneBatch(t *testing.T) {
	fake := &po2FakeCache{running: map[string]int64{}}
	router := NewPowerOfTwoRouterWithCache(fake)
	names := []string{"pod1", "pod2", "pod3", "pod4", "pod5", "pod6"}
	pods := po2TestPods(names...)

	const requests = 50
	chosen := make([]string, requests)
	for i := 0; i < requests; i++ {
		ctx := newPo2Ctx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		chosen[i] = ctx.TargetPod().Name
	}

	require.Len(t, fake.batches, requests, "exactly one cache read per routing decision")
	for i, batch := range fake.batches {
		require.Len(t, batch, 2, "request %d read %v", i, batch)
		assert.NotEqual(t, batch[0], batch[1], "request %d compared a pod with itself", i)
		assert.Subset(t, names, batch)
		assert.Contains(t, batch, chosen[i], "request %d chose a pod it did not compare", i)
	}
}

// A pod with a strictly higher count never wins, so one heavily loaded pod among many receives
// nothing, while every other pod stays reachable.
func TestPowerOfTwoRouter_NeverPicksTheBusiestPodAmongMany(t *testing.T) {
	router := NewPowerOfTwoRouterWithCache(&po2FakeCache{running: map[string]int64{"pod1": 1000}})
	pods := po2TestPods("pod1", "pod2", "pod3", "pod4")

	tally := map[string]int{}
	for i := 0; i < 200; i++ {
		ctx := newPo2Ctx(fmt.Sprintf("req-%d", i))
		_, err := router.Route(ctx, newPo2PodList(pods, nil))
		require.NoError(t, err)
		tally[ctx.TargetPod().Name]++
	}

	assert.Zero(t, tally["pod1"], "busy pod was chosen: %v", tally)
	for _, name := range []string{"pod2", "pod3", "pod4"} {
		assert.Positive(t, tally[name], "%s should still receive traffic: %v", name, tally)
	}
}

// A cache that cannot report counts degrades load awareness, not availability.
func TestPowerOfTwoRouter_CacheFailureStillRoutes(t *testing.T) {
	tests := []struct {
		name string
		fake *po2FakeCache
	}{
		{name: "cache error", fake: &po2FakeCache{runningErr: fmt.Errorf("redis unavailable")}},
		{name: "nil counts without an error", fake: &po2FakeCache{nilCounts: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewPowerOfTwoRouterWithCache(tt.fake)
			ctx := newPo2Ctx("req-cache-failure")

			addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1", "pod2"), nil))

			require.NoError(t, err, "routing must fail open when counts are unavailable")
			assert.Contains(t, []string{"10.0.0.1:8000", "10.0.0.2:8000"}, addr)
		})
	}
}

// The pod is chosen on its running-request total, and only then is a port chosen within it,
// from the per-port counts of that pod alone.
func TestPowerOfTwoRouter_ChoosesLeastLoadedPortOfTheChosenPod(t *testing.T) {
	t.Run("one data-parallel pod", func(t *testing.T) {
		fake := &po2FakeCache{portRunning: map[string]float64{
			"pod1/8000": 5, "pod1/8001": 2, "pod1/8002": 9, "pod1/8003": 2,
		}}
		router := NewPowerOfTwoRouterWithCache(fake)
		ports := map[string][]int{"pod1": {8000, 8001, 8002, 8003}}

		ctx := newPo2Ctx("req-dp")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), ports))

		require.NoError(t, err)
		assert.Contains(t, []int{8001, 8003}, ctx.TargetPort(), "only the least loaded ports are eligible")
		assert.Equal(t, fmt.Sprintf("10.0.0.1:%d", ctx.TargetPort()), addr)
	})

	t.Run("pod first, then port", func(t *testing.T) {
		// pod1 is far busier overall, and its port counts are deliberately misleading: pod2
		// must win on its total, and its port comes from pod2's own counts.
		fake := &po2FakeCache{
			running: map[string]int64{"pod1": 9, "pod2": 1},
			portRunning: map[string]float64{
				"pod1/8000": 0, "pod1/8001": 0,
				"pod2/8000": 7, "pod2/8001": 1,
			},
		}
		router := NewPowerOfTwoRouterWithCache(fake)
		ports := map[string][]int{"pod1": {8000, 8001}, "pod2": {8000, 8001}}

		ctx := newPo2Ctx("req-dp-pods")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1", "pod2"), ports))

		require.NoError(t, err)
		assert.Equal(t, "pod2", ctx.TargetPod().Name)
		assert.Equal(t, 8001, ctx.TargetPort())
		assert.Equal(t, "10.0.0.2:8001", addr)
	})

	t.Run("single-port pod uses its only port", func(t *testing.T) {
		router := NewPowerOfTwoRouterWithCache(&po2FakeCache{})

		ctx := newPo2Ctx("req-one-port")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), map[string][]int{"pod1": {8000}}))

		require.NoError(t, err)
		assert.Equal(t, 8000, ctx.TargetPort())
		assert.Equal(t, "10.0.0.1:8000", addr)
	})

	t.Run("pod without listed ports falls back to its default port", func(t *testing.T) {
		router := NewPowerOfTwoRouterWithCache(&po2FakeCache{})

		ctx := newPo2Ctx("req-no-ports")
		addr, err := router.Route(ctx, newPo2PodList(po2TestPods("pod1"), map[string][]int{"pod1": {}}))

		require.NoError(t, err)
		assert.Zero(t, ctx.TargetPort(), "no port was chosen, so the default is used")
		assert.Equal(t, "10.0.0.1:8000", addr)
	})
}

func TestPowerOfTwoRouter_SubscribesToRunningRequests(t *testing.T) {
	assert.Equal(t, []string{metrics.RealtimeNumRequestsRunning}, NewPowerOfTwoRouterWithCache(nil).SubscribedMetrics())
}

// po2Gateway runs Route against a real cache.Store and makes the calls the gateway makes around
// it, so the counters Route reads are the ones the gateway maintains.
type po2Gateway struct {
	t      *testing.T
	store  *cache.Store
	router *PowerOfTwoRouter
	pods   []*v1.Pod
	ports  map[string][]int
}

func newPo2Gateway(t *testing.T, pods []*v1.Pod, ports map[string][]int) *po2Gateway {
	t.Helper()
	store := cache.NewWithPodsForTest(pods, po2TestModel)
	return &po2Gateway{t: t, store: store, router: NewPowerOfTwoRouterWithCache(store), pods: pods, ports: ports}
}

// pick only routes: Route on its own does not count the request.
func (g *po2Gateway) pick(requestID string) (*types.RoutingContext, string, error) {
	ctx := newPo2Ctx(requestID)
	addr, err := g.router.Route(ctx, newPo2PodList(g.pods, g.ports))
	return ctx, addr, err
}

// route routes a request and then registers it, as gateway_req_body.go does. It never fails
// the test itself, so it is safe to call from goroutines.
func (g *po2Gateway) route(requestID string) (*types.RoutingContext, string, error) {
	ctx, addr, err := g.pick(requestID)
	if err != nil {
		return ctx, "", err
	}
	g.store.AddRequestCount(ctx, requestID, po2TestModel)
	return ctx, addr, nil
}

// done completes a request, as gateway.go does when the response ends.
func (g *po2Gateway) done(ctx *types.RoutingContext) {
	g.store.DoneRequestCount(ctx, ctx.RequestID, po2TestModel, 0)
}

func (g *po2Gateway) running(pod string) int64 {
	g.t.Helper()
	n, err := g.store.GetPodRunningRequests(pod, po2Namespace)
	require.NoError(g.t, err)
	return n
}

// The counts Route compares are the store's: seeding a pod's running requests steers routing.
func TestPowerOfTwoRouter_ReadsRunningRequestsFromTheStore(t *testing.T) {
	tests := []struct {
		name    string
		count1  float64
		count2  float64
		wantPod string
	}{
		{name: "second is busier", count1: 1, count2: 5, wantPod: "pod1"},
		{name: "first is busier", count1: 5, count2: 1, wantPod: "pod2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := po2TestPods("pod1", "pod2")
			store := cache.NewWithPodsMetricsForTest(pods, po2TestModel, map[string]map[string]metrics.MetricValue{
				"pod1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: tt.count1}},
				"pod2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: tt.count2}},
			})
			router := NewPowerOfTwoRouterWithCache(store)

			ctx := newPo2Ctx("req-seeded")
			_, err := router.Route(ctx, newPo2PodList(pods, nil))

			require.NoError(t, err)
			assert.Equal(t, tt.wantPod, ctx.TargetPod().Name)
		})
	}
}

// Route leaves counting to the gateway: the request is counted once it is registered with the
// cache, and uncounted exactly once when it completes, however often completion is reported.
func TestPowerOfTwoRouter_RequestLifecycleThroughTheCache(t *testing.T) {
	g := newPo2Gateway(t, po2TestPods("pod1", "pod2"), nil)

	ctx, _, err := g.pick("req-lifecycle")
	require.NoError(t, err)
	assert.Zero(t, g.running("pod1")+g.running("pod2"), "Route alone must not count the request")

	g.store.AddRequestCount(ctx, ctx.RequestID, po2TestModel)
	chosen, other := "pod1", "pod2"
	if ctx.TargetPod().Name == "pod2" {
		chosen, other = other, chosen
	}
	assert.Equal(t, int64(1), g.running(chosen))
	assert.Equal(t, int64(0), g.running(other))

	g.done(ctx)
	g.done(ctx)
	g.store.DoneRequestTrace(ctx, ctx.RequestID, po2TestModel, 10, 20, 0)
	assert.Equal(t, int64(0), g.running(chosen), "completion must uncount once, not once per report")
	assert.Equal(t, int64(0), g.running(other))
}

// While requests are in flight, the two pods' counts can never differ by more than one, whatever
// the tie-breaks: both are always sampled and the emptier one wins. This fails if Route stops
// reading live counts, or stops seeing what the gateway counted.
func TestPowerOfTwoRouter_TwoPodsStayWithinOneOfEachOther(t *testing.T) {
	g := newPo2Gateway(t, po2TestPods("pod1", "pod2"), nil)
	const requests = 40

	for i := 0; i < requests; i++ {
		_, _, err := g.route(fmt.Sprintf("req-%d", i))
		require.NoError(t, err)

		a, b := g.running("pod1"), g.running("pod2")
		diff := a - b
		if diff < 0 {
			diff = -diff
		}
		require.LessOrEqual(t, diff, int64(1), "after %d requests the counts are %d and %d", i+1, a, b)
	}

	// An even number of requests with a gap of at most one is an exact split.
	assert.Equal(t, int64(requests/2), g.running("pod1"))
	assert.Equal(t, int64(requests/2), g.running("pod2"))
}

// With nothing completing, requests alternate over the ports of one data-parallel pod. This
// characterizes what the cache reports per port today; it is not proof of true per-port load
// accounting. addPodStats writes the pod's total running requests into the routed port's
// metric slot, so the port just used always looks strictly busier than its sibling and the next
// request takes the other one. A real per-port counter would behave the same here, but if the two
// slots ever held the same value, port choice would fall back to a random tie-break and the
// after-every-request check below would flake. Choosing a port from per-port numbers is what
// TestPowerOfTwoRouter_ChoosesLeastLoadedPortOfTheChosenPod checks.
func TestPowerOfTwoRouter_RequestsSpreadEvenlyOverPorts(t *testing.T) {
	g := newPo2Gateway(t, po2TestPods("pod1"), map[string][]int{"pod1": {8000, 8001}})
	const requests = 40

	routed := map[int]int{}
	for i := 0; i < requests; i++ {
		ctx, _, err := g.route(fmt.Sprintf("req-%d", i))
		require.NoError(t, err)
		routed[ctx.TargetPort()]++

		diff := routed[8000] - routed[8001]
		if diff < 0 {
			diff = -diff
		}
		require.LessOrEqual(t, diff, 1, "after %d requests the ports have served %v", i+1, routed)
	}

	assert.Equal(t, map[int]int{8000: requests / 2, 8001: requests / 2}, routed)
}

// Concurrent requests must leave no count behind. Run with -race.
func TestPowerOfTwoRouter_ConcurrentRequestsDrainToZero(t *testing.T) {
	names := []string{"pod1", "pod2", "pod3"}
	g := newPo2Gateway(t, po2TestPods(names...), nil)
	const requests = 60

	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, _, err := g.route(fmt.Sprintf("req-%d", i))
			if err != nil {
				errs <- err
				return
			}
			g.done(ctx)
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("route failed: %v", err)
	}
	for _, name := range names {
		assert.Zero(t, g.running(name), "%s must drain back to zero", name)
	}
}
