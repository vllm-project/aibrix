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
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
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
	assert.Equal(t, int64(1), bc.calls.Load(), "narrowing decodePods under the selection lock must reuse the prefetched counts without re-reading Redis")

	r.pendingDecodeTracker.RemovePendingDecode(ctx.RequestID)
	r.prefillRequestTracker.RemovePrefillRequest(ctx.RequestID)
}

// TestSelectStripeIndex_DeterministicAndBounded verifies selectStripeIndex
// handles empty and arbitrary model names deterministically within bounds and
// spreads distinct model names across multiple stripes.
func TestSelectStripeIndex_DeterministicAndBounded(t *testing.T) {
	seen := make(map[uint32]struct{})
	for i := 0; i < 64; i++ {
		model := fmt.Sprintf("pd-model-%d", i)
		idx := selectStripeIndex(model)
		assert.Less(t, idx, uint32(pdSelectLockStripes))
		assert.Equal(t, idx, selectStripeIndex(model), "stripe index must be deterministic")
		seen[idx] = struct{}{}
	}
	assert.Less(t, selectStripeIndex(""), uint32(pdSelectLockStripes), "empty model name must map to a valid stripe")
	assert.GreaterOrEqual(t, len(seen), 16, "distinct model names should spread across multiple stripes")
}

// TestPDRouter_DistinctModelStripesDoNotBlockEachOther proves deterministically
// that holding one model's selection stripe lock does not block selection for a
// model on a different stripe, while a second request for the held model still
// waits until the stripe is released.
func TestPDRouter_DistinctModelStripesDoNotBlockEachOther(t *testing.T) {
	modelA := "model-alpha"
	modelB := "model-beta"
	for i := 0; selectStripeIndex(modelA) == selectStripeIndex(modelB); i++ {
		modelB = fmt.Sprintf("model-beta-%d", i)
	}
	require.NotEqual(t, selectStripeIndex(modelA), selectStripeIndex(modelB))

	podsA := []*v1.Pod{
		burstPod("a-prefill-0", "prefill", "127.0.1.1"),
		burstPod("a-decode-0", "decode", "127.0.1.100"),
	}
	podsB := []*v1.Pod{
		burstPod("b-prefill-0", "prefill", "127.0.2.1"),
		burstPod("b-decode-0", "decode", "127.0.2.100"),
	}

	r := &pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}

	// Hold modelA's stripe lock directly.
	lockA := r.selectLock(modelA)
	lockA.Lock()
	unlockedA := false
	defer func() {
		if !unlockedA {
			lockA.Unlock()
		}
	}()

	// A selection for modelB (on a distinct stripe) must complete while modelA's stripe is held.
	ctxB := types.NewRoutingContext(context.Background(), "pd", modelB, "hello", "req-b", "user")
	ctxB.Engine = "vllm"
	doneB := make(chan error, 1)
	go func() {
		p, d, err := r.filterPrefillDecodePods(ctxB, podsB)
		if err == nil && (p == nil || d == nil) {
			err = fmt.Errorf("unexpected nil pod selection")
		}
		doneB <- err
	}()

	select {
	case err := <-doneB:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("selection for modelB blocked while modelA's stripe lock was held")
	}
	r.pendingDecodeTracker.RemovePendingDecode(ctxB.RequestID)
	r.prefillRequestTracker.RemovePrefillRequest(ctxB.RequestID)

	// Meanwhile, a selection for modelA must block until lockA is released.
	ctxA := types.NewRoutingContext(context.Background(), "pd", modelA, "hello", "req-a", "user")
	ctxA.Engine = "vllm"
	doneA := make(chan error, 1)
	go func() {
		p, d, err := r.filterPrefillDecodePods(ctxA, podsA)
		if err == nil && (p == nil || d == nil) {
			err = fmt.Errorf("unexpected nil pod selection")
		}
		doneA <- err
	}()

	select {
	case err := <-doneA:
		t.Fatalf("selection for modelA completed while its own stripe lock was held (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked on modelA's stripe.
	}

	lockA.Unlock()
	unlockedA = true

	select {
	case err := <-doneA:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("selection for modelA did not resume after its stripe lock was released")
	}
	r.pendingDecodeTracker.RemovePendingDecode(ctxA.RequestID)
	r.prefillRequestTracker.RemovePrefillRequest(ctxA.RequestID)
}

// TestPDRouter_ConcurrentMultiModelBurstsPreservePerModelAtomicity drives
// simultaneous bursts across multiple independent PD models sharing one
// pdRouter and verifies that each model's prefill pods still receive exact
// even distribution under -race.
func TestPDRouter_ConcurrentMultiModelBurstsPreservePerModelAtomicity(t *testing.T) {
	const (
		modelCount      = 4
		prefillPerModel = 4
		reqsPerModel    = 64 // divisible by prefillPerModel
	)

	modelNames := make([]string, modelCount)
	modelPrefills := make([][]*v1.Pod, modelCount)
	modelPodLists := make([]*utils.PodArray, modelCount)
	for m := 0; m < modelCount; m++ {
		modelNames[m] = fmt.Sprintf("burst-model-%d", m)
		prefills := make([]*v1.Pod, 0, prefillPerModel)
		for i := 0; i < prefillPerModel; i++ {
			p := burstPod(fmt.Sprintf("m%d-prefill-%d", m, i), "prefill", fmt.Sprintf("127.0.%d.%d", m+1, i+1))
			p.Labels[PDRoleSetIdentifier] = fmt.Sprintf("rs-%d", m)
			prefills = append(prefills, p)
		}
		dec := burstPod(fmt.Sprintf("m%d-decode-0", m), "decode", fmt.Sprintf("127.0.%d.100", m+1))
		dec.Labels[PDRoleSetIdentifier] = fmt.Sprintf("rs-%d", m)
		modelPrefills[m] = prefills
		modelPodLists[m] = &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefills...), dec)}
	}

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

	totalReqs := modelCount * reqsPerModel
	ctxs := make([]*types.RoutingContext, totalReqs)
	errs := make([]error, totalReqs)
	for m := 0; m < modelCount; m++ {
		for j := 0; j < reqsPerModel; j++ {
			idx := m*reqsPerModel + j
			ctx := types.NewRoutingContext(context.Background(), "pd", modelNames[m], "hello", fmt.Sprintf("mm-%d-%d", m, j), "user")
			ctx.Engine = "vllm"
			ctx.ReqPath = testChatCompletionsPath
			ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`)
			ctxs[idx] = ctx
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for m := 0; m < modelCount; m++ {
		for j := 0; j < reqsPerModel; j++ {
			idx := m*reqsPerModel + j
			wg.Add(1)
			go func(m, idx int) {
				defer wg.Done()
				<-start
				_, errs[idx] = r.Route(ctxs[idx], modelPodLists[m])
			}(m, idx)
		}
	}
	close(start)

	allPrefills := make([]*v1.Pod, 0, modelCount*prefillPerModel)
	for _, ps := range modelPrefills {
		allPrefills = append(allPrefills, ps...)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		total := int32(0)
		for _, cnt := range tracker.GetPrefillRequestCountsForPods(allPrefills) {
			total += cnt
		}
		if int(total) == totalReqs {
			break
		}
		require.True(t, time.Now().Before(deadline), "only %d of %d prefill requests registered before deadline", total, totalReqs)
		time.Sleep(5 * time.Millisecond)
	}
	heldCounts := tracker.GetPrefillRequestCountsForPods(allPrefills)

	close(gate)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "Route for request %d failed", i)
	}

	wantPerPod := reqsPerModel / prefillPerModel
	for m := 0; m < modelCount; m++ {
		selected := map[string]int{}
		for j := 0; j < reqsPerModel; j++ {
			selected[ctxs[m*reqsPerModel+j].RespHeaders[HeaderPrefillTargetPod]]++
		}
		for _, pod := range modelPrefills[m] {
			assert.Equalf(t, wantPerPod, selected[pod.Name], "model %s pod %s selections (all: %v)", modelNames[m], pod.Name, selected)
			assert.Equalf(t, int32(wantPerPod), heldCounts[utils.GeneratePodKey(pod.Namespace, pod.Name)], "model %s pod %s tracked count", modelNames[m], pod.Name)
		}
	}
}

// TestPDRouter_CrossStripeTokenLoadAndPrefixCacheRaceFree exercises concurrent
// selections across distinct model stripes (including two model names sharing
// the exact same base PD pods, as in multi-LoRA deployments) under
// hybrid_cache_load prefill + token_load decode with the prefix-cache updater
// active. This verifies under -race that all shared trackers (prefixCacheIndexer,
// tokenLoadTracker, pendingPrefixCredits, countersMu, prefillRequestTracker,
// pendingDecodeTracker) remain race-free and drain cleanly without a single
// global lock.
func TestPDRouter_CrossStripeTokenLoadAndPrefixCacheRaceFree(t *testing.T) {
	// Pick two model names guaranteed to hash to distinct stripes that share
	// the same base pods, plus two disjoint models on distinct stripes.
	loraA := "lora-adapter-a"
	loraB := "lora-adapter-b"
	for i := 0; selectStripeIndex(loraA) == selectStripeIndex(loraB); i++ {
		loraB = fmt.Sprintf("lora-adapter-b-%d", i)
	}
	require.NotEqual(t, selectStripeIndex(loraA), selectStripeIndex(loraB))

	sharedPods := []*v1.Pod{
		burstPod("shared-prefill-0", "prefill", "127.0.10.1"),
		burstPod("shared-prefill-1", "prefill", "127.0.10.2"),
		burstPod("shared-decode-0", "decode", "127.0.10.100"),
		burstPod("shared-decode-1", "decode", "127.0.10.101"),
	}
	disjointC := []*v1.Pod{
		burstPod("c-prefill-0", "prefill", "127.0.11.1"),
		burstPod("c-prefill-1", "prefill", "127.0.11.2"),
		burstPod("c-decode-0", "decode", "127.0.11.100"),
		burstPod("c-decode-1", "decode", "127.0.11.101"),
	}
	disjointD := []*v1.Pod{
		burstPod("d-prefill-0", "prefill", "127.0.12.1"),
		burstPod("d-prefill-1", "prefill", "127.0.12.2"),
		burstPod("d-decode-0", "decode", "127.0.12.100"),
		burstPod("d-decode-1", "decode", "127.0.12.101"),
	}

	type modelSpec struct {
		name      string
		baseModel string
		podList   *utils.PodArray
	}
	models := []modelSpec{
		{name: loraA, baseModel: "shared-base-model", podList: &utils.PodArray{Pods: sharedPods}},
		{name: loraB, baseModel: "shared-base-model", podList: &utils.PodArray{Pods: sharedPods}},
		{name: "disjoint-model-c", podList: &utils.PodArray{Pods: disjointC}},
		{name: "disjoint-model-d", podList: &utils.PodArray{Pods: disjointD}},
	}

	gate := make(chan struct{})
	close(gate) // let prefill requests complete immediately while selections overlap
	prefixTable := prefixcacheindexer.NewPrefixHashTable()
	tokenLoad := pd.NewTokenLoadTracker()
	tracker := pd.NewPrefillRequestTracker()
	pending := pd.NewPendingDecodeTracker()
	client := &http.Client{Transport: &gatedTransport{gate: gate}}

	r := &pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         newHybridCacheLoadPrefillPolicy(prefixTable, tokenLoad),
		decodePolicy:          pd.TokenLoadDecodePolicy{},
		prefixCacheIndexer:    prefixTable,
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pending,
		tokenLoadTracker:      tokenLoad,
		httpClient:            client,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}
	r.podSelector = selector.NewDefaultSelector(r.filterPrefillDecodePods)
	r.prefillExecutor = prefill.NewDefaultExecutor(client, tracker, prefill.WithTokenLoadTracker(tokenLoad))
	r.startPrefixUpdater()
	t.Cleanup(func() { close(r.prefixUpdateCh) })

	const reqsPerModel = 32
	prompt := strings.Repeat("shared prefix prompt for cross-stripe race check ", 16)
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}],"stream":true}`, prompt))

	start := make(chan struct{})
	errCh := make(chan error, len(models)*reqsPerModel)
	var wg sync.WaitGroup
	for mIdx, spec := range models {
		for j := 0; j < reqsPerModel; j++ {
			wg.Add(1)
			go func(mIdx, j int, spec modelSpec) {
				defer wg.Done()
				reqID := fmt.Sprintf("xstripe-%d-%d", mIdx, j)
				ctx := types.NewRoutingContext(context.Background(), "pd", spec.name, prompt, reqID, "user")
				defer ctx.Delete()
				ctx.BaseModel = spec.baseModel
				ctx.Engine = "vllm"
				ctx.ReqPath = testChatCompletionsPath
				ctx.ReqBody = body
				ctx.ReqHeaders = map[string]string{
					constants.HeaderSessionKey: fmt.Sprintf("sess-%d-%d", mIdx, j%4),
				}
				<-start
				if _, err := r.Route(ctx, spec.podList); err != nil {
					errCh <- fmt.Errorf("model=%s req=%s: %w", spec.name, reqID, err)
					return
				}
				r.DoneRequestCount(ctx, reqID, spec.name, 0)
			}(mIdx, j, spec)
		}
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	// Verify all trackers drained cleanly after all requests completed.
	allPods := append(append(append([]*v1.Pod{}, sharedPods...), disjointC...), disjointD...)
	for _, cnt := range tracker.GetPrefillRequestCountsForPods(allPods) {
		assert.Equal(t, int32(0), cnt)
	}
	for _, pod := range allPods {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		assert.Equal(t, float64(0), pending.GetPendingDecodeCount(key))
		assert.Equal(t, float64(0), tokenLoad.GetDecodeLoad(key))
	}
	r.countersMu.RLock()
	totalSelections := int64(0)
	for _, cnt := range r.selectionCounts {
		totalSelections += cnt
	}
	r.countersMu.RUnlock()
	// Each Route increments selectionCounts for both prefill and decode pods.
	assert.Equal(t, int64(len(models)*reqsPerModel*2), totalSelections)
}

// TestPDRouter_LoRAAdaptersSharingBaseModelSerializeOnSameStripe verifies that
// two LoRA adapter names whose raw model names hash to different stripes share
// a single selection lock stripe via RoutingContext.BaseModel: holding the base
// model's stripe blocks both adapters (while an unrelated base model proceeds),
// and a concurrent mixed-adapter burst against shared pods splits evenly.
func TestPDRouter_LoRAAdaptersSharingBaseModelSerializeOnSameStripe(t *testing.T) {
	const baseModel = "qwen3-8b"
	loraA := "adapter-a"
	loraB := "adapter-b"
	for i := 0; selectStripeIndex(loraA) == selectStripeIndex(loraB); i++ {
		loraB = fmt.Sprintf("adapter-b-%d", i)
	}
	require.NotEqual(t, selectStripeIndex(loraA), selectStripeIndex(loraB),
		"adapter names must map to distinct stripes so the test proves BaseModel drives the key")

	otherBase := "llama3-70b"
	for i := 0; selectStripeIndex(otherBase) == selectStripeIndex(baseModel); i++ {
		otherBase = fmt.Sprintf("llama3-70b-%d", i)
	}
	require.NotEqual(t, selectStripeIndex(otherBase), selectStripeIndex(baseModel))

	t.Run("deterministic_lock_hold", func(t *testing.T) {
		sharedPods := []*v1.Pod{
			burstPod("shared-prefill-0", "prefill", "127.0.20.1"),
			burstPod("shared-decode-0", "decode", "127.0.20.100"),
		}
		otherPods := []*v1.Pod{
			burstPod("other-prefill-0", "prefill", "127.0.21.1"),
			burstPod("other-decode-0", "decode", "127.0.21.100"),
		}
		r := &pdRouter{
			cache:                 cache.NewForTest(),
			prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
			prefillRequestTracker: pd.NewPrefillRequestTracker(),
			pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
			prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
			selectionCounts:       map[string]int64{},
		}

		// Hold the base model's stripe lock.
		baseLock := r.selectLock(baseModel)
		baseLock.Lock()
		unlocked := false
		defer func() {
			if !unlocked {
				baseLock.Unlock()
			}
		}()

		// Unrelated base model must complete immediately.
		ctxOther := types.NewRoutingContext(context.Background(), "pd", otherBase, "hello", "req-other", "user")
		ctxOther.Engine = "vllm"
		doneOther := make(chan error, 1)
		go func() {
			_, _, err := r.filterPrefillDecodePods(ctxOther, otherPods)
			doneOther <- err
		}()
		select {
		case err := <-doneOther:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("unrelated base model blocked while baseModel stripe was held")
		}
		r.pendingDecodeTracker.RemovePendingDecode(ctxOther.RequestID)
		r.prefillRequestTracker.RemovePrefillRequest(ctxOther.RequestID)

		// Both LoRA adapters sharing baseModel must block until baseLock is released.
		startAdapter := func(adapter, reqID string) chan error {
			ctx := types.NewRoutingContext(context.Background(), "pd", adapter, "hello", reqID, "user")
			ctx.BaseModel = baseModel
			ctx.Engine = "vllm"
			done := make(chan error, 1)
			go func() {
				_, _, err := r.filterPrefillDecodePods(ctx, sharedPods)
				done <- err
			}()
			return done
		}
		doneA := startAdapter(loraA, "req-lora-a")
		doneB := startAdapter(loraB, "req-lora-b")

		select {
		case err := <-doneA:
			t.Fatalf("adapter %s completed while baseModel stripe was held (err=%v)", loraA, err)
		case err := <-doneB:
			t.Fatalf("adapter %s completed while baseModel stripe was held (err=%v)", loraB, err)
		case <-time.After(50 * time.Millisecond):
			// Expected: both adapters wait on baseModel's stripe.
		}

		baseLock.Unlock()
		unlocked = true

		for name, ch := range map[string]chan error{loraA: doneA, loraB: doneB} {
			select {
			case err := <-ch:
				require.NoError(t, err, "adapter %s failed after unlock", name)
			case <-time.After(2 * time.Second):
				t.Fatalf("adapter %s did not resume after baseModel stripe unlock", name)
			}
		}
		r.pendingDecodeTracker.RemovePendingDecode("req-lora-a")
		r.prefillRequestTracker.RemovePrefillRequest("req-lora-a")
		r.pendingDecodeTracker.RemovePendingDecode("req-lora-b")
		r.prefillRequestTracker.RemovePrefillRequest("req-lora-b")
	})

	t.Run("mixed_adapter_burst_splits_evenly", func(t *testing.T) {
		const (
			prefillCount = 4
			requests     = 64 // 32 loraA + 32 loraB across 4 shared pods -> 16 each
		)
		prefillPods := make([]*v1.Pod, 0, prefillCount)
		for i := 0; i < prefillCount; i++ {
			prefillPods = append(prefillPods, burstPod(fmt.Sprintf("lora-prefill-%d", i), "prefill", fmt.Sprintf("127.0.22.%d", i+1)))
		}
		decodePod := burstPod("lora-decode-0", "decode", "127.0.22.100")
		podList := &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefillPods...), decodePod)}

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
			model := loraA
			if i%2 == 1 {
				model = loraB
			}
			ctx := types.NewRoutingContext(context.Background(), "pd", model, "hello", fmt.Sprintf("lora-burst-%d", i), "user")
			ctx.BaseModel = baseModel
			ctx.Engine = "vllm"
			ctx.ReqPath = testChatCompletionsPath
			ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`)
			ctxs[i] = ctx
		}

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

		deadline := time.Now().Add(20 * time.Second)
		for {
			total := int32(0)
			for _, cnt := range tracker.GetPrefillRequestCountsForPods(prefillPods) {
				total += cnt
			}
			if int(total) == requests {
				break
			}
			require.True(t, time.Now().Before(deadline), "only %d of %d requests registered before deadline", total, requests)
			time.Sleep(5 * time.Millisecond)
		}

		close(gate)
		wg.Wait()

		for i, err := range errs {
			require.NoErrorf(t, err, "request %d failed", i)
		}
		selected := map[string]int{}
		for _, ctx := range ctxs {
			selected[ctx.RespHeaders[HeaderPrefillTargetPod]]++
		}
		want := requests / prefillCount
		for _, pod := range prefillPods {
			assert.Equalf(t, want, selected[pod.Name], "shared pod %s selections (all: %v)", pod.Name, selected)
		}
	})
}
