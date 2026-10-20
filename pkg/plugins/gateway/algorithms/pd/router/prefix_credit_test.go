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
	"net/http"
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
