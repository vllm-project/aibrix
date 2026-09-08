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
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	r.prefillExecutor = prefill.NewDefaultExecutor(client, tracker, prefillRequestTimeout)

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
	heldPending := pending.GetPendingDecodeCount(decodePod.Name)

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
		assert.Equalf(t, int32(want), heldCounts[pod.Name], "%s tracked prefill count while held (all: %v)", pod.Name, heldCounts)
	}
	assert.Equal(t, float64(requests), heldPending, "pending decode count while held")

	// Every registration is released once its prefill call returns.
	for _, cnt := range tracker.GetPrefillRequestCountsForPods(prefillPods) {
		assert.Equal(t, int32(0), cnt)
	}
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(decodePod.Name))
}
