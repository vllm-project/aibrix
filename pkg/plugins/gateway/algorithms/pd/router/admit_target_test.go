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
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// countingGatedTransport counts prefill calls and holds each one open until gate closes.
type countingGatedTransport struct {
	calls atomic.Int32
	gate  <-chan struct{}
}

func (tr *countingGatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls.Add(1)
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

// capOneAdmitter stands in for the gateway's requestsInflight admission with a cap of one
// running request per pod.
func capOneAdmitter(running *sync.Map) func(pod *v1.Pod) error {
	return func(pod *v1.Pod) error {
		count, _ := running.LoadOrStore(pod.Name, new(atomic.Int32))
		if !count.(*atomic.Int32).CompareAndSwap(0, 1) {
			return types.ErrTargetReplicaInflightExceeded
		}
		return nil
	}
}

// TestPDRouter_FullDecodePodRejectsBeforePrefill is the reproduction from the issue: with a
// cap of one on the decode pod, a request that arrives while another request's prefill is
// still running must be rejected without sending a prefill of its own. Admitting the decode
// pod only after Route returns let both requests run a prefill before one was rejected.
func TestPDRouter_FullDecodePodRejectsBeforePrefill(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	podList := &utils.PodArray{Pods: []*v1.Pod{prefillPod, decodePod}}

	gate := make(chan struct{})
	transport := &countingGatedTransport{gate: gate}
	r, tokenLoad := newTokenLoadTestRouter(t, &http.Client{Transport: transport})

	var running sync.Map
	first := tokenLoadRequest(t, "first", 4000)
	first.SetTargetAdmitter(capOneAdmitter(&running))
	second := tokenLoadRequest(t, "second", 4000)
	second.SetTargetAdmitter(capOneAdmitter(&running))

	firstDone := make(chan error, 1)
	go func() {
		_, err := r.Route(first, podList)
		firstDone <- err
	}()
	require.Eventually(t, func() bool { return transport.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the first request's prefill should be in flight")

	secondDone := make(chan error, 1)
	go func() {
		_, err := r.Route(second, podList)
		secondDone <- err
	}()
	var secondErr error
	select {
	case secondErr = <-secondDone:
	case <-time.After(time.Second):
		// The second request reached its own prefill instead of being rejected; let both
		// finish so the assertions below report it.
		close(gate)
		secondErr = <-secondDone
		gate = nil
	}
	assert.True(t, errors.Is(secondErr, types.ErrTargetReplicaInflightExceeded), "got %v", secondErr)
	assert.Equal(t, int32(1), transport.calls.Load(), "a rejected request must not send a prefill")
	assert.Equal(t, 1, r.prefillRequestTracker.GetPrefillRequestCountsForPod(utils.GeneratePodKey(prefillPod.Namespace, prefillPod.Name)),
		"only the admitted request should count against the prefill pod")

	if gate != nil {
		close(gate)
	}
	require.NoError(t, <-firstDone)
	assert.Equal(t, decodePod.Name, first.TargetPod().Name)

	// The rejected request released everything Select registered for it.
	active, _ := tokenLoad.GetLoad(utils.GeneratePodKey(prefillPod.Namespace, prefillPod.Name))
	assert.Equal(t, float64(0), active)
	assert.Equal(t, 0, r.prefillRequestTracker.GetPrefillRequestCountsForPod(utils.GeneratePodKey(prefillPod.Namespace, prefillPod.Name)))
}

// TestPDRouter_AdmitsDecodePodBeforePrefill checks the order for an admitted request: the
// decode pod is admitted once, before the prefill leg is sent, and stays the target.
func TestPDRouter_AdmitsDecodePodBeforePrefill(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	podList := &utils.PodArray{Pods: []*v1.Pod{prefillPod, decodePod}}

	gate := make(chan struct{})
	close(gate)
	transport := &countingGatedTransport{gate: gate}
	r, _ := newTokenLoadTestRouter(t, &http.Client{Transport: transport})

	ctx := tokenLoadRequest(t, "admitted", 4000)
	var admitted []string
	var prefillsAtAdmission int32 = -1
	ctx.SetTargetAdmitter(func(pod *v1.Pod) error {
		admitted = append(admitted, pod.Name)
		prefillsAtAdmission = transport.calls.Load()
		return nil
	})

	_, err := r.Route(ctx, podList)
	require.NoError(t, err)
	assert.Equal(t, []string{decodePod.Name}, admitted)
	assert.Equal(t, int32(0), prefillsAtAdmission, "the decode pod must be admitted before the prefill is sent")
	assert.Equal(t, int32(1), transport.calls.Load())
	assert.Equal(t, decodePod.Name, ctx.TargetPod().Name)
}
