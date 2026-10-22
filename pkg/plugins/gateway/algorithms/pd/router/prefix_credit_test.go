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
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// queuedPrefixCredits drains the prefix updates Route enqueued and returns the pods they
// credit. Nothing else drains prefixUpdateCh in these fixtures.
func queuedPrefixCredits(r *pdRouter) []string {
	var pods []string
	for {
		select {
		case job := <-r.prefixUpdateCh:
			pods = append(pods, job.pod)
		default:
			return pods
		}
	}
}

func prefixCreditPodList() *utils.PodArray {
	return &utils.PodArray{Pods: []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
		burstPod("decode-0", "decode", "127.0.0.100"),
	}}
}

// A request whose decode pod rejects it (the gateway's requestsInflight cap) sends no
// prefill, so it must not credit the prefill pod with its prompt either: the credit would
// draw the next requests with that prefix to a pod that never computed it.
func TestPDRouter_RejectedRequestCreditsNoPrefillPod(t *testing.T) {
	r, _, _ := newHybridCacheLoadTestRouter(t, openGateClient(), pd.HybridCacheLoadConfig{Factor: 0.5})
	ctx := hybridRequest(t, "rejected", 4000, "")
	ctx.SetTargetAdmitter(func(*v1.Pod) error { return types.ErrTargetReplicaInflightExceeded })

	_, err := r.Route(ctx, prefixCreditPodList())
	require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
	assert.Empty(t, queuedPrefixCredits(r), "a rejected request must not credit a prefill pod")
	_, pending := r.pendingPrefixCredits.Load(ctx.RequestID)
	assert.False(t, pending, "Route must take the request's pending credit")
}

// An admitted request credits its prefill pod before the prefill leg is sent, as before,
// so requests with the same prefix that arrive while that prefill runs follow it there.
func TestPDRouter_AdmittedRequestCreditsPrefillPodBeforePrefill(t *testing.T) {
	gate := make(chan struct{})
	transport := &countingGatedTransport{gate: gate}
	r, _, _ := newHybridCacheLoadTestRouter(t, &http.Client{Transport: transport}, pd.HybridCacheLoadConfig{Factor: 0.5})
	ctx := hybridRequest(t, "admitted", 4000, "")
	ctx.SetTargetAdmitter(func(*v1.Pod) error { return nil })

	done := make(chan error, 1)
	go func() {
		_, err := r.Route(ctx, prefixCreditPodList())
		done <- err
	}()
	require.Eventually(t, func() bool { return transport.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the prefill should be in flight")
	credited := queuedPrefixCredits(r)
	close(gate)
	require.NoError(t, <-done)
	assert.Equal(t, []string{burstPodKey(ctx.RespHeaders[HeaderPrefillTargetPod])}, credited,
		"the prefill pod is credited while its prefill is still running")
}

type selectiveFailingTransport struct {
	failingHost  string
	statusCode   int
	failingCalls atomic.Int32
	healthyCalls atomic.Int32
}

func (tr *selectiveFailingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if strings.Contains(req.URL.Host, tr.failingHost) {
		tr.failingCalls.Add(1)
		return &http.Response{
			StatusCode: tr.statusCode,
			Status:     http.StatusText(tr.statusCode),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"pod failing"}`)),
			Request:    req,
		}, nil
	}
	tr.healthyCalls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// TestPDRouter_FailedPrefillRemovesPrefixCredit verifies that when a prefill request fails,
// any prefix credit enqueued during selection is followed by a removal update so the failing
// pod does not retain unearned prefix cache credit.
func TestPDRouter_FailedPrefillRemovesPrefixCredit(t *testing.T) {
	transport := &selectiveFailingTransport{failingHost: "127.0.0.1", statusCode: http.StatusServiceUnavailable}
	r, _, _ := newHybridCacheLoadTestRouter(t, &http.Client{Transport: transport}, pd.HybridCacheLoadConfig{Factor: 0.5})

	singlePodList := &utils.PodArray{Pods: []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("decode-0", "decode", "127.0.0.100"),
	}}
	ctx := hybridRequest(t, "failed-prefill", 4000, "")

	_, err := r.Route(ctx, singlePodList)
	require.Error(t, err)

	// Collect the jobs Route enqueued
	var jobs []prefixUpdateJob
	for {
		select {
		case job := <-r.prefixUpdateCh:
			jobs = append(jobs, job)
		default:
			goto drained
		}
	}
drained:
	require.Len(t, jobs, 2, "must enqueue both initial credit and rollback removal on failure")
	assert.False(t, jobs[0].remove, "first job was credit")
	assert.Equal(t, burstPodKey("prefill-0"), jobs[0].pod)
	assert.True(t, jobs[1].remove, "second job was removal rollback")
	assert.Equal(t, burstPodKey("prefill-0"), jobs[1].pod)
}

// TestPDRouter_FailingPrefillPodEjectedDivertsTraffic reproduces Issue #3222:
// When prefill-1 fails every prefill, consecutive failure tracking ejects prefill-1
// after the threshold, diverting all subsequent requests to healthy prefill-0 instead
// of prefill-1 drawing 39/40 requests due to zero load.
func TestPDRouter_FailingPrefillPodEjectedDivertsTraffic(t *testing.T) {
	transport := &selectiveFailingTransport{failingHost: "127.0.0.2", statusCode: http.StatusServiceUnavailable}
	r, _, _ := newHybridCacheLoadTestRouter(t, &http.Client{Transport: transport}, pd.HybridCacheLoadConfig{Factor: 0.5})
	r.prefillRequestTracker.SetFailureEjectionConfig(3, 10*time.Second, 60*time.Second)

	podList := prefixCreditPodList() // prefill-0 on 127.0.0.1, prefill-1 on 127.0.0.2, decode-0 on 127.0.0.100

	var prefill1Attempts int
	var prefill0Successes int

	// Run 20 requests sharing the same prompt
	for i := 0; i < 20; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		ctx := hybridRequest(t, reqID, 4000, "")
		_, err := r.Route(ctx, podList)
		targetPrefill := ctx.RespHeaders[HeaderPrefillTargetPod]
		switch targetPrefill {
		case "prefill-1":
			prefill1Attempts++
			assert.Error(t, err)
		case "prefill-0":
			prefill0Successes++
			assert.NoError(t, err)
		}
	}

	// prefill-1 should fail exactly 3 times (the consecutive failure threshold),
	// after which it is ejected and all remaining 17 requests go to healthy prefill-0.
	assert.Equal(t, 3, prefill1Attempts, "failing pod must be ejected after reaching the threshold")
	assert.Equal(t, 17, prefill0Successes, "all subsequent requests must route to the healthy pod")
	assert.True(t, r.prefillRequestTracker.IsPrefillPodEjected(burstPodKey("prefill-1"), time.Now()))
}
