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

package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatedTransport answers every prefill POST with an empty JSON object (a valid
// no-op vLLM prefill reply) but only after gate is closed. While the gate is
// shut every in-flight prefill stays registered in PrefillRequestTracker, so
// the tracker state observed by later selections is exactly the set of
// decisions made so far.
type gatedTransport struct {
	gate <-chan struct{}
}

func (tr *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	select {
	case <-tr.gate:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

func burstPod(name, role string, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{PDRoleSetIdentifier: "burst-rs", PDRoleIdentifier: role},
		},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// burstPodKey returns the tracker key, utils.GeneratePodKey, of the burstPod
// named name.
func burstPodKey(name string) string {
	return utils.GeneratePodKey("", name)
}

// TestPDRouter_ConcurrentBurstSeesPriorSelections drives a burst of concurrent
// Route calls at idle prefill pods with the least_request policy and holds
// every prefill HTTP call open until the whole burst has been routed.
//
// Selection and tracker registration must be one atomic step: the k-th
// decision has to see the k-1 registrations made before it. When that holds,
// least_request keeps the per-pod counts within one of each other and a burst
// divisible by the pod count lands exactly evenly. When selection reads the
// tracker and registration happens later, without a lock, concurrent decisions
// read the same stale snapshot and the burst piles unevenly onto whichever
// pods looked idle at read time.
func TestPDRouter_ConcurrentBurstSeesPriorSelections(t *testing.T) {
	const (
		prefillCount = 4
		requests     = 256 // divisible by prefillCount
	)

	prefillPods := make([]*v1.Pod, 0, prefillCount)
	for i := 0; i < prefillCount; i++ {
		prefillPods = append(prefillPods, burstPod(fmt.Sprintf("prefill-%d", i), "prefill", fmt.Sprintf("127.0.0.%d", i+1)))
	}
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePod)

	gate := make(chan struct{})
	tracker := pd.NewPrefillRequestTracker()
	pending := pd.NewPendingDecodeTracker()
	client := &http.Client{Transport: &gatedTransport{gate: gate}}
	r := &pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pending,
		httpClient:            client,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}
	r.podSelector = selector.NewDefaultSelector(r.filterPrefillDecodePods)
	r.prefillExecutor = prefill.NewDefaultExecutor(client, tracker)

	ctxs := make([]*types.RoutingContext, requests)
	errs := make([]error, requests)
	for i := range ctxs {
		ctx := types.NewRoutingContext(context.Background(), "pd", "burst-model", "hello", fmt.Sprintf("burst-%d", i), "user")
		ctx.Engine = "vllm"
		ctx.ReqPath = testChatCompletionsPath
		ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`)
		ctxs[i] = ctx
	}

	podList := &utils.PodArray{Pods: readyPods}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range ctxs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = r.Route(ctxs[i], podList)
		}(i)
	}
	close(start)

	// Wait until every request has been routed and parked on the gate, i.e.
	// every prefill registration is in the tracker at once.
	deadline := time.Now().Add(20 * time.Second)
	for {
		total := int32(0)
		for _, cnt := range tracker.GetPrefillRequestCountsForPods(prefillPods) {
			total += cnt
		}
		if total == requests {
			break
		}
		require.True(t, time.Now().Before(deadline), "only %d of %d prefill requests registered before deadline", total, requests)
		time.Sleep(5 * time.Millisecond)
	}
	heldCounts := tracker.GetPrefillRequestCountsForPods(prefillPods)
	heldPending := pending.GetPendingDecodeCount(utils.GeneratePodKey(decodePod.Namespace, decodePod.Name))

	close(gate)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "Route for request %d failed", i)
	}

	// Decisions as reported to the client must agree with the tracker ledger
	// observed while the burst was held, and both must be exactly even.
	selected := map[string]int{}
	for _, ctx := range ctxs {
		selected[ctx.RespHeaders[HeaderPrefillTargetPod]]++
	}
	want := requests / prefillCount
	for _, pod := range prefillPods {
		assert.Equalf(t, want, selected[pod.Name], "%s selections (all: %v)", pod.Name, selected)
		assert.Equalf(t, int32(want), heldCounts[utils.GeneratePodKey(pod.Namespace, pod.Name)], "%s tracked prefill count while held (all: %v)", pod.Name, heldCounts)
	}
	assert.Equal(t, float64(requests), heldPending, "pending decode count while held")

	// Every registration is released once its prefill call returns.
	for _, cnt := range tracker.GetPrefillRequestCountsForPods(prefillPods) {
		assert.Equal(t, int32(0), cnt)
	}
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(utils.GeneratePodKey(decodePod.Namespace, decodePod.Name)))
}

// barrierRunningCache blocks every GetPodsRunningRequests (and shared decode
// ledger read) until expected callers are inside the read simultaneously,
// proving Redis I/O runs concurrently outside selectMu without relying on
// wall-clock timing thresholds.
type barrierRunningCache struct {
	cache.Cache
	expected    int32
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	calls       atomic.Int64
	counts      map[string]int64
	remote      map[string]cache.RemoteDecodeLoad
	release     chan struct{}
	releaseOnce sync.Once
	timeout     time.Duration
}

func newBarrierRunningCache(expected int32, timeout time.Duration) *barrierRunningCache {
	return &barrierRunningCache{
		Cache:    cache.NewForTest(),
		expected: expected,
		release:  make(chan struct{}),
		timeout:  timeout,
	}
}

func (b *barrierRunningCache) waitBarrier() error {
	b.calls.Add(1)
	cur := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		prev := b.maxInFlight.Load()
		if cur <= prev || b.maxInFlight.CompareAndSwap(prev, cur) {
			break
		}
	}
	if cur >= b.expected {
		b.releaseOnce.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
		return nil
	case <-time.After(b.timeout):
		return fmt.Errorf("timed out waiting for %d concurrent Redis reads (peak in-flight=%d)", b.expected, b.maxInFlight.Load())
	}
}

func (b *barrierRunningCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	if err := b.waitBarrier(); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(pods))
	for _, p := range pods {
		key := utils.GeneratePodKey(p.Namespace, p.Name)
		out[key] = b.counts[key]
	}
	return out, nil
}

func (b *barrierRunningCache) SharedDecodeLedgerAvailable() bool { return true }
func (b *barrierRunningCache) PublishDecodeLedger(_ cache.DecodeLedgerProvider) func(string) {
	return func(string) {}
}

func (b *barrierRunningCache) GetPodsRunningRequestsAndDecodeLedger(pods []*v1.Pod) (map[string]int64, map[string]cache.RemoteDecodeLoad, error) {
	counts, err := b.GetPodsRunningRequests(pods)
	if err != nil {
		return nil, nil, err
	}
	return counts, b.remote, nil
}

// TestPDRouter_ConcurrentRedisReadsDoNotSerializeOnSelectMu verifies that
// concurrent filterPrefillDecodePods calls execute their cross-gateway Redis
// reads in parallel before acquiring selectMu, for both default load_balancing
// and token_load with a shared decode ledger.
func TestPDRouter_ConcurrentRedisReadsDoNotSerializeOnSelectMu(t *testing.T) {
	const workers = 16
	prefillPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
	}
	decodePods := []*v1.Pod{
		burstPod("decode-0", "decode", "127.0.0.100"),
		burstPod("decode-1", "decode", "127.0.0.101"),
	}
	readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePods...)

	cases := []struct {
		name         string
		decodePolicy pd.DecodeScorePolicy
		sharedLedger bool
	}{
		{name: "load_balancing", decodePolicy: nil, sharedLedger: false},
		{name: "token_load_shared_ledger", decodePolicy: pd.TokenLoadDecodePolicy{}, sharedLedger: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := newBarrierRunningCache(workers, 3*time.Second)
			r := &pdRouter{
				cache:                 bc,
				prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
				decodePolicy:          tc.decodePolicy,
				prefillRequestTracker: pd.NewPrefillRequestTracker(),
				pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
				tokenLoadTracker:      pd.NewTokenLoadTracker(),
				prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
				selectionCounts:       map[string]int64{},
			}
			if tc.sharedLedger {
				r.sharedDecodeLedger = bc
			}

			start := make(chan struct{})
			errs := make([]error, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					reqID := fmt.Sprintf("concurrent-%s-%d", tc.name, i)
					ctx := types.NewRoutingContext(context.Background(), "pd", "model-a", "hello", reqID, "user")
					ctx.Engine = "vllm"
					ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`)
					<-start
					p, d, err := r.filterPrefillDecodePods(ctx, readyPods)
					errs[i] = err
					if err == nil {
						assert.NotNil(t, p)
						assert.NotNil(t, d)
						r.pendingDecodeTracker.RemovePendingDecode(reqID)
						r.prefillRequestTracker.RemovePrefillRequest(reqID)
						r.releaseTokenLoad(reqID)
					}
				}(i)
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				require.NoErrorf(t, err, "worker %d failed", i)
			}
			assert.Equal(t, int32(workers), bc.maxInFlight.Load(), "all workers must execute Redis reads concurrently outside selectMu")
			assert.Equal(t, int64(workers), bc.calls.Load())
		})
	}
}

// TestPDRouter_ReusesRoutingContextRunningRequestsSnapshot verifies that
// filterPrefillDecodePods reuses a per-decision RoutingContext.RunningRequests
// snapshot (such as one already populated by narrowToInflightCapacity) instead
// of issuing a second Redis read, and uses those counts for decode selection.
func TestPDRouter_ReusesRoutingContextRunningRequestsSnapshot(t *testing.T) {
	prefill0 := burstPod("prefill-0", "prefill", "127.0.0.1")
	decodeHot := burstPod("decode-hot", "decode", "127.0.0.100")
	decodeIdle := burstPod("decode-idle", "decode", "127.0.0.101")
	readyPods := []*v1.Pod{prefill0, decodeHot, decodeIdle}

	bc := newBarrierRunningCache(1, time.Second)
	bc.counts = map[string]int64{
		utils.GeneratePodKey(prefill0.Namespace, prefill0.Name):     0,
		utils.GeneratePodKey(decodeHot.Namespace, decodeHot.Name):   25,
		utils.GeneratePodKey(decodeIdle.Namespace, decodeIdle.Name): 0,
	}
	r := &pdRouter{
		cache:                 bc,
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}

	ctx := types.NewRoutingContext(context.Background(), "pd", "model-a", "hello", "req-snapshot", "user")
	ctx.Engine = "vllm"
	ctx.StartRunningRequestsSnapshot()
	defer ctx.StopRunningRequestsSnapshot()

	// Simulate narrowToInflightCapacity populating the decision snapshot on all ready pods.
	_, err := ctx.RunningRequests(readyPods, bc.GetPodsRunningRequests)
	require.NoError(t, err)
	require.Equal(t, int64(1), bc.calls.Load())

	_, decodePod, err := r.filterPrefillDecodePods(ctx, readyPods)
	require.NoError(t, err)
	require.NotNil(t, decodePod)
	assert.Equal(t, decodeIdle.Name, decodePod.Name, "decode selection must observe the snapshot running counts")
	assert.Equal(t, int64(1), bc.calls.Load(), "PD selection must reuse the RoutingContext snapshot instead of reading Redis again")

	r.pendingDecodeTracker.RemovePendingDecode(ctx.RequestID)
	r.prefillRequestTracker.RemovePrefillRequest(ctx.RequestID)
}

// TestPDRouter_PrefetchThenPrefillImbalanceNarrowDoesNotRereadRedis verifies
// that when prefill load-imbalance fires under selectMu and narrows decodePods
// to a single roleset, decode scoring reuses the counts already prefetched
// before selectMu instead of issuing a second Redis read.
func TestPDRouter_PrefetchThenPrefillImbalanceNarrowDoesNotRereadRedis(t *testing.T) {
	prefillHot := burstPod("prefill-rs0", "prefill", "127.0.0.1")
	prefillHot.Labels[PDRoleSetIdentifier] = "rs-0"
	decodeRS0 := burstPod("decode-rs0", "decode", "127.0.0.100")
	decodeRS0.Labels[PDRoleSetIdentifier] = "rs-0"

	prefillIdle := burstPod("prefill-rs1", "prefill", "127.0.0.2")
	prefillIdle.Labels[PDRoleSetIdentifier] = "rs-1"
	decodeRS1 := burstPod("decode-rs1", "decode", "127.0.0.101")
	decodeRS1.Labels[PDRoleSetIdentifier] = "rs-1"

	readyPods := []*v1.Pod{prefillHot, decodeRS0, prefillIdle, decodeRS1}

	bc := newBarrierRunningCache(1, time.Second)
	bc.counts = map[string]int64{
		utils.GeneratePodKey(decodeRS0.Namespace, decodeRS0.Name): 0,
		utils.GeneratePodKey(decodeRS1.Namespace, decodeRS1.Name): 3,
	}
	prefillTracker := pd.NewPrefillRequestTracker()
	// Create a prefill spread above default PrefillLoadImbalanceMinSpread (16) so
	// step 4 narrows prefillPods to prefillIdle and aligns decodePods to rs-1.
	for i := 0; i < 20; i++ {
		prefillTracker.AddPrefillRequest(fmt.Sprintf("hot-%d", i), utils.GeneratePodKey(prefillHot.Namespace, prefillHot.Name))
	}

	r := &pdRouter{
		cache:                 bc,
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		prefillRequestTracker: prefillTracker,
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}

	ctx := types.NewRoutingContext(context.Background(), "pd", "model-a", "hello", "req-narrow", "user")
	ctx.Engine = "vllm"

	prefillPod, decodePod, err := r.filterPrefillDecodePods(ctx, readyPods)
	require.NoError(t, err)
	require.NotNil(t, prefillPod)
	require.NotNil(t, decodePod)
	assert.Equal(t, prefillIdle.Name, prefillPod.Name, "prefill imbalance fast path must pick idle prefill pod")
	assert.Equal(t, decodeRS1.Name, decodePod.Name, "decode selection must stay aligned to narrowed roleset rs-1")
	assert.Equal(t, int64(1), bc.calls.Load(), "narrowing decodePods under selectMu must reuse the prefetched counts without re-reading Redis")

	r.pendingDecodeTracker.RemovePendingDecode(ctx.RequestID)
	r.prefillRequestTracker.RemovePrefillRequest(ctx.RequestID)
}
