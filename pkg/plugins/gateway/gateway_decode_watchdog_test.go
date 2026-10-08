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

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

const watchdogTestRID = "req-watchdog-00000000deadbeef"

// watchdogAbortRecorder stands in for the decode pod's /abort_request endpoint.
// Its channel is buffered so the handler never blocks, which keeps a failing
// assertion from hanging the test binary on the server's deferred Close.
type watchdogAbortRecorder struct {
	got chan string
}

// newWatchdogAbortRecorder returns the recorder and the host:port to hand to
// the leg as its decode target.
func newWatchdogAbortRecorder(t *testing.T) (*watchdogAbortRecorder, string) {
	t.Helper()
	rec := &watchdogAbortRecorder{got: make(chan string, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/abort_request" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		rec.got <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, strings.TrimPrefix(srv.URL, "http://")
}

func (a *watchdogAbortRecorder) wait(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case body := <-a.got:
		return body
	case <-time.After(d):
		t.Fatalf("no /abort_request reached the decode pod within %s", d)
		return ""
	}
}

func (a *watchdogAbortRecorder) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case body := <-a.got:
		t.Fatalf("unexpected /abort_request: %s", body)
	case <-time.After(d):
	}
}

// newWatchdogProcessServer is newBlockingProcessServer with room to push
// messages in while the loop is running.
func newWatchdogProcessServer(ctx context.Context, capacity int) *blockingProcessServer {
	return &blockingProcessServer{ctx: ctx, msgs: make(chan *extProcPb.ProcessingRequest, capacity)}
}

// newWatchdogState builds the processState the loop holds for a streaming
// SGLang PD request whose decode leg is in flight: a rid (unless rid is empty),
// a decode target and the given watchdog timeouts as the request's overrides,
// on top of the process defaults so the abort itself stays enabled. The prefill
// leg has not reported yet; the tests mark it themselves.
func newWatchdogState(ctx context.Context, rid, decodeAddr string, watchdog types.PDWatchdogOverrides) *processState {
	routerCtx := types.NewRoutingContext(ctx, routing.RouterPD, "test-model", "hello", "req-watchdog", "")
	if rid != "" {
		routerCtx.SetPDRequestID(rid)
	}
	routerCtx.SetDecodeTarget(decodeAddr, "decode-1")
	overrides := *types.DefaultPDOverrides()
	overrides.Watchdog = watchdog
	routerCtx.SetPDOverrides(&overrides)
	return &processState{
		ctx:       ctx,
		requestID: "req-watchdog",
		model:     "test-model",
		routerCtx: routerCtx,
		stream:    true,
	}
}

func responseHeadersMsg(statusCode string) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{{Key: ":status", RawValue: []byte(statusCode)}},
				},
			},
		},
	}
}

// responseBodyChunk is one streamed SSE chunk from the decode pod.
func responseBodyChunk(chunk string) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{Body: []byte(chunk)},
		},
	}
}

// counterWithLabel finds the first emission of name whose label key has value.
func counterWithLabel(counters []capturedCounter, name, key, value string) (capturedCounter, bool) {
	for _, c := range counters {
		if c.name == name && c.labels[key] == value {
			return c, true
		}
	}
	return capturedCounter{}, false
}

// awaitAbortsCounted blocks until n decode aborts have gone through the
// capture hook.
//
// The watchdog's abort runs in its own goroutine, so without this a test could
// return - and t.Cleanup put the real counter function back - while that
// goroutine is still reading the hook, which the race detector rightly calls a
// data race. Waiting for the counter is waiting for the last thing the
// goroutine touches.
func awaitAbortsCounted(t *testing.T, counters func() []capturedCounter, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		counted := 0
		for _, c := range counters() {
			if c.name == metrics.GatewayPDDecodeAbortTotal {
				counted++
			}
		}
		return counted >= n
	}, 5*time.Second, 10*time.Millisecond, "expected %d decode abort(s) to be counted", n)
}

// awaitLoopCancelled cancels the stream and waits for a loop that is still
// parked to return.
func awaitLoopCancelled(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not return after the stream context was cancelled")
	}
}

// TestDecodeWatchdogFirstResponseFailsStream is the case the feature exists
// for: the prefill leg succeeded - so the decode pod took the KV transfer and
// owes the client a response - and then the decode pod says nothing at all.
func TestDecodeWatchdogFirstResponseFailsStream(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
		types.PDWatchdogOverrides{FirstResponseTimeout: 150 * time.Millisecond})
	leg := st.routerCtx.PDLeg()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	// Let the loop reach the select first, so the success is an edge on the
	// wakeup channel rather than an already-closed one: the arming path that
	// matters in production is the one that has to interrupt a parked select.
	time.Sleep(50 * time.Millisecond)
	armed := time.Now()
	leg.MarkPrefillSucceeded()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not fire within 5s of the prefill leg succeeding")
	}
	assert.GreaterOrEqual(t, time.Since(armed), 100*time.Millisecond,
		"the watchdog fired well before its deadline")

	require.Error(t, err)
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())
	assert.Contains(t, grpcStatus.Message(), "sent nothing within")

	// Nothing had been written to the client yet, so it is answered the same
	// way a prefill fail-fast answers it.
	sent := srv.sentResponses()
	require.Len(t, sent, 1)
	immediate := sent[0].GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, envoyTypePb.StatusCode_GatewayTimeout, immediate.GetStatus().GetCode())
	assert.Contains(t, immediate.GetBody(), "decode-1")
	assert.Contains(t, immediate.GetBody(), "did not start responding")

	headers := map[string]string{}
	for _, h := range immediate.GetHeaders().GetSetHeaders() {
		headers[h.GetHeader().GetKey()] = string(h.GetHeader().GetRawValue())
	}
	assert.Equal(t, "true", headers[HeaderErrorPDDecode])
	assert.Equal(t, st.requestID, headers[HeaderRequestID])

	// The decode pod is told to drop the request so it stops holding KV pages
	// for a client that is being disconnected - once.
	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, watchdogTestRID, gjson.Get(body, "rid").String())
	recorder.none(t, 300*time.Millisecond)

	awaitAbortsCounted(t, counters, 1)
	select {
	case <-leg.AbortDone():
	case <-time.After(time.Second):
		t.Fatal("AbortDone was not closed after the watchdog abort finished")
	}

	emitted := counters()
	watchdog, ok := findCounter(emitted, metrics.GatewayPDDecodeWatchdogTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDDecodeWatchdogTotal)
	assert.Equal(t, decodeWatchdogPhaseFirstResponse, watchdog.labels["phase"])

	abort, _ := findCounter(emitted, metrics.GatewayPDDecodeAbortTotal)
	assert.Equal(t, pd.AbortTriggerWatchdogFirstResponse, abort.labels["prefill_failure_class"])
	assert.Equal(t, "ok", abort.labels["result"])

	fail, ok := counterWithLabel(emitted, metrics.GatewayRequestModelFailTotal, "status", decodeWatchdogStatus)
	require.True(t, ok, "a watchdog kill must be counted as a failed request")
	assert.Equal(t, "504", fail.labels["status_code"])
	for _, c := range emitted {
		assert.NotEqual(t, metrics.GatewayRequestModelSuccessTotal, c.name,
			"a watchdog kill must never count as a gateway request success")
	}
}

// TestDecodeWatchdogFirstResponseDisarmedByFirstChunk: the first token is
// exactly what the watchdog was waiting for, so the first body chunk must
// disarm it - including when it arrives after the timer is already running.
func TestDecodeWatchdogFirstResponseDisarmedByFirstChunk(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
		types.PDWatchdogOverrides{FirstResponseTimeout: 300 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	time.Sleep(50 * time.Millisecond)
	st.routerCtx.PDLeg().MarkPrefillSucceeded()
	// Well inside the 300ms budget, and with the timer already armed.
	time.Sleep(50 * time.Millisecond)
	srv.msgs <- responseHeadersMsg("200")
	srv.msgs <- responseBodyChunk("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")

	// Past the deadline the disarm prevented.
	select {
	case err := <-done:
		t.Fatalf("the watchdog killed a stream the decode pod had answered: %v", err)
	case <-time.After(600 * time.Millisecond):
	}

	for _, resp := range srv.sentResponses() {
		assert.Nil(t, resp.GetImmediateResponse(), "a live stream must not be answered by the gateway")
	}
	_, fired := findCounter(counters(), metrics.GatewayPDDecodeWatchdogTotal)
	assert.False(t, fired, "the watchdog must not fire on a decode pod that answered")
	recorder.none(t, 100*time.Millisecond)

	awaitLoopCancelled(t, cancel, done)
}

// TestDecodeWatchdogFirstResponseFiresAfterHeadersWithoutBody is the SGLang
// shape of a decode pod that wedges before its first token: Starlette sends the
// response headers of a StreamingResponse before it starts iterating the
// generator, so the headers arrive and then nothing does. The headers must not
// disarm the first-response deadline. And since they already went to the
// client, the kill cannot be a 504: the stream is cut instead, the way a
// stream-idle kill is.
func TestDecodeWatchdogFirstResponseFiresAfterHeadersWithoutBody(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	// The idle budget is a minute on purpose: if the headers had moved the
	// watchdog to the stream-idle phase this test would time out.
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr, types.PDWatchdogOverrides{
		FirstResponseTimeout: 200 * time.Millisecond,
		StreamIdleTimeout:    time.Minute,
	})
	leg := st.routerCtx.PDLeg()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	time.Sleep(50 * time.Millisecond)
	armed := time.Now()
	leg.MarkPrefillSucceeded()
	time.Sleep(50 * time.Millisecond)
	srv.msgs <- responseHeadersMsg("200")

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("response headers alone disarmed the first-response watchdog")
	}
	assert.GreaterOrEqual(t, time.Since(armed), 150*time.Millisecond,
		"the watchdog fired well before its deadline")

	require.Error(t, err)
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())
	assert.Contains(t, grpcStatus.Message(), "sent headers but no body within")

	// The only response is the one to the headers: nothing is synthesised on
	// top of a status line the client already has.
	sent := srv.sentResponses()
	require.Len(t, sent, 1)
	assert.Nil(t, sent[0].GetImmediateResponse(),
		"a response whose headers went out must not be replaced by an ImmediateResponse")

	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, watchdogTestRID, gjson.Get(body, "rid").String())
	recorder.none(t, 300*time.Millisecond)

	awaitAbortsCounted(t, counters, 1)

	emitted := counters()
	watchdog, ok := findCounter(emitted, metrics.GatewayPDDecodeWatchdogTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDDecodeWatchdogTotal)
	assert.Equal(t, decodeWatchdogPhaseFirstResponse, watchdog.labels["phase"])

	abort, _ := findCounter(emitted, metrics.GatewayPDDecodeAbortTotal)
	assert.Equal(t, pd.AbortTriggerWatchdogFirstResponse, abort.labels["prefill_failure_class"])

	fail, ok := counterWithLabel(emitted, metrics.GatewayRequestModelFailTotal, "status", decodeWatchdogStatus)
	require.True(t, ok, "a watchdog kill must be counted as a failed request")
	assert.Equal(t, "504", fail.labels["status_code"])
}

// TestDecodeWatchdogFirstResponseNotArmedForNonStreamingByDefault: on a
// non-streaming request SGLang sends the response headers only once generation
// is finished, so "nothing from the decode pod yet" is the normal state for the
// entire decode - a healthy long answer would be killed mid-generation if the
// streaming time-to-first-token budget applied to it. With the non-streaming
// budget at its default of 0, the watchdog must stay disarmed no matter how
// small the streaming budget is.
func TestDecodeWatchdogFirstResponseNotArmedForNonStreamingByDefault(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
		types.PDWatchdogOverrides{FirstResponseTimeout: 100 * time.Millisecond})
	st.stream = false
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	// Five times the streaming budget: it would have fired long ago.
	select {
	case err := <-done:
		t.Fatalf("the streaming first-response budget was applied to a non-streaming request: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	assert.Empty(t, srv.sentResponses(), "a healthy non-streaming request must not be answered by the watchdog")
	_, fired := findCounter(counters(), metrics.GatewayPDDecodeWatchdogTotal)
	assert.False(t, fired, "a disarmed watchdog must not emit its counter")
	recorder.none(t, 100*time.Millisecond)

	// The loop is parked, not finished: cancelling the stream is what ends it.
	awaitLoopCancelled(t, cancel, done)
}

// TestDecodeWatchdogResponseTimeoutFiresForNonStreaming: an operator who knows
// their max_tokens and throughput can put a bound on the whole generation. When
// it expires the kill is the same as the streaming one, except for the wording
// of the 504 - nothing "started" late here, the answer never finished.
func TestDecodeWatchdogResponseTimeoutFiresForNonStreaming(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	// The streaming budget is a minute on purpose: if it were the one being
	// consulted this test would time out instead of passing.
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr, types.PDWatchdogOverrides{
		FirstResponseTimeout: time.Minute,
		ResponseTimeout:      150 * time.Millisecond,
	})
	st.stream = false

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	time.Sleep(50 * time.Millisecond)
	armed := time.Now()
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the non-streaming watchdog did not fire within 5s of the prefill leg succeeding")
	}
	assert.GreaterOrEqual(t, time.Since(armed), 100*time.Millisecond,
		"the watchdog fired well before its deadline")

	require.Error(t, err)
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())

	sent := srv.sentResponses()
	require.Len(t, sent, 1)
	immediate := sent[0].GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, envoyTypePb.StatusCode_GatewayTimeout, immediate.GetStatus().GetCode())
	assert.Contains(t, immediate.GetBody(), "did not finish responding")
	assert.NotContains(t, immediate.GetBody(), "did not start responding")
	// The budget it reports is the non-streaming one, not the minute.
	assert.Contains(t, immediate.GetBody(), (150 * time.Millisecond).String())

	// The decode pod is holding KV pages for an answer nobody will read.
	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, watchdogTestRID, gjson.Get(body, "rid").String())

	awaitAbortsCounted(t, counters, 1)

	emitted := counters()
	watchdog, ok := findCounter(emitted, metrics.GatewayPDDecodeWatchdogTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDDecodeWatchdogTotal)
	// Both modes report the same phase; the log line's stream field splits them.
	assert.Equal(t, decodeWatchdogPhaseFirstResponse, watchdog.labels["phase"])
}

// TestDecodeWatchdogStreamIdleClosesStream: the decode pod answered, sent a
// chunk and then went silent. The client cannot be given a fresh response at
// this point, so the stream is closed instead of being left hanging.
func TestDecodeWatchdogStreamIdleClosesStream(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	srv.msgs <- responseHeadersMsg("200")
	srv.msgs <- responseBodyChunk("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")

	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr, types.PDWatchdogOverrides{
		FirstResponseTimeout: time.Minute,
		StreamIdleTimeout:    150 * time.Millisecond,
	})
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not fire within 5s of the decode pod going silent")
	}
	assert.GreaterOrEqual(t, time.Since(started), 100*time.Millisecond,
		"the watchdog fired well before its deadline")

	require.Error(t, err)
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())
	assert.Contains(t, grpcStatus.Message(), "stopped sending for")

	// Both of the decode pod's messages were answered normally, and nothing
	// was synthesised on top of a response that is already half-delivered.
	sent := srv.sentResponses()
	require.Len(t, sent, 2)
	for _, resp := range sent {
		assert.Nil(t, resp.GetImmediateResponse(),
			"a half-delivered response must not be replaced by an ImmediateResponse")
	}

	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, watchdogTestRID, gjson.Get(body, "rid").String())
	recorder.none(t, 300*time.Millisecond)

	awaitAbortsCounted(t, counters, 1)

	emitted := counters()
	watchdog, ok := findCounter(emitted, metrics.GatewayPDDecodeWatchdogTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDDecodeWatchdogTotal)
	assert.Equal(t, decodeWatchdogPhaseStreamIdle, watchdog.labels["phase"])

	abort, _ := findCounter(emitted, metrics.GatewayPDDecodeAbortTotal)
	assert.Equal(t, pd.AbortTriggerWatchdogStreamIdle, abort.labels["prefill_failure_class"])

	fail, ok := counterWithLabel(emitted, metrics.GatewayRequestModelFailTotal, "status", decodeWatchdogStatus)
	require.True(t, ok, "a watchdog kill must be counted as a failed request")
	assert.Equal(t, "504", fail.labels["status_code"])
	for _, c := range emitted {
		assert.NotEqual(t, metrics.GatewayRequestModelSuccessTotal, c.name,
			"a truncated response must never count as a gateway request success")
	}
}

// TestDecodeWatchdogStreamIdleRearmsOnEveryChunk: a pod that keeps streaming is
// never killed, however long the response runs. 50ms chunks against a 200ms
// idle budget for 600ms - three times the budget - so a missing re-arm fails
// this test deterministically.
func TestDecodeWatchdogStreamIdleRearmsOnEveryChunk(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 32)
	srv.msgs <- responseHeadersMsg("200")

	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr, types.PDWatchdogOverrides{
		FirstResponseTimeout: time.Minute,
		StreamIdleTimeout:    200 * time.Millisecond,
	})
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	const chunks = 12
	for i := 0; i < chunks; i++ {
		select {
		case err := <-done:
			t.Fatalf("the watchdog killed a streaming response after %d chunks: %v", i, err)
		default:
		}
		srv.msgs <- responseBodyChunk("data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
		time.Sleep(50 * time.Millisecond)
	}
	recorder.none(t, 0)

	// The pod stops mid-response: now the idle budget does run out.
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not fire after the stream went idle")
	}
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())

	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, watchdogTestRID, gjson.Get(body, "rid").String())
	awaitAbortsCounted(t, counters, 1)
}

// TestDecodeWatchdogStreamIdleNotArmedForNonStreaming: a non-streaming answer
// arrives as the finished body, so once its first chunk is in there is nothing
// left to be idle between. The stream-idle budget must not apply to it, however
// small, and the first-response budget is spent once the body started.
func TestDecodeWatchdogStreamIdleNotArmedForNonStreaming(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	srv.msgs <- responseHeadersMsg("200")
	srv.msgs <- responseBodyChunk(`{"id":"cmpl-1","choices":[{"message":{"content":"partial`)

	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr, types.PDWatchdogOverrides{
		ResponseTimeout:   time.Minute,
		StreamIdleTimeout: 50 * time.Millisecond,
	})
	st.stream = false
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	// Eight times the idle budget.
	select {
	case err := <-done:
		t.Fatalf("the stream-idle budget was applied to a non-streaming response: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	_, fired := findCounter(counters(), metrics.GatewayPDDecodeWatchdogTotal)
	assert.False(t, fired, "a disarmed watchdog must not emit its counter")
	recorder.none(t, 100*time.Millisecond)

	awaitLoopCancelled(t, cancel, done)
}

// TestDecodeAbortClientHasNoTimeout: every watchdog abort carries its own
// AIBRIX_DECODE_ABORT_TIMEOUT deadline on the request context, which a profile
// can raise. A client-level Timeout would cap that value without a word.
func TestDecodeAbortClientHasNoTimeout(t *testing.T) {
	assert.Zero(t, decodeAbortClient.Timeout)
}

// TestDecodeWatchdogNotArmedWithoutRID: only SGLang PD requests carry a
// gateway-owned rid, and only for them does a successful prefill mean the
// decode pod owes a response. Everything else must select exactly as before.
// TestDecodeWatchdogInertBeforeRouting covers the loop's first passes, before
// RequestHeaders has built the routing context: st.routerCtx is still nil there
// and every watchdog input must read as "nothing armed" rather than panic.
func TestDecodeWatchdogInertBeforeRouting(t *testing.T) {
	st := &processState{ctx: context.Background(), stream: true}

	require.NotPanics(t, func() {
		assert.Nil(t, st.routerCtx.PrefillSucceeded())
		deadline, phase := st.decodeWatchdogDeadline()
		assert.True(t, deadline.IsZero())
		assert.Empty(t, phase)
		st.stopDecodeWatchdog()
	})
}

func TestDecodeWatchdogNotArmedWithoutRID(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, "", decodeAddr, types.PDWatchdogOverrides{
		FirstResponseTimeout: 50 * time.Millisecond,
		ResponseTimeout:      50 * time.Millisecond,
	})
	// Everything the watchdog needs except the rid.
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	select {
	case err := <-done:
		t.Fatalf("the watchdog fired on a stream with no rid: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	assert.Empty(t, srv.sentResponses())
	_, fired := findCounter(counters(), metrics.GatewayPDDecodeWatchdogTotal)
	assert.False(t, fired, "a non-SGLang stream must not arm the watchdog")
	recorder.none(t, 100*time.Millisecond)

	awaitLoopCancelled(t, cancel, done)
}

// TestDecodeWatchdogDisabledByZeroTimeout: 0 is the documented off switch.
func TestDecodeWatchdogDisabledByZeroTimeout(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
		types.PDWatchdogOverrides{FirstResponseTimeout: 0, ResponseTimeout: 50 * time.Millisecond})
	st.routerCtx.PDLeg().MarkPrefillSucceeded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	select {
	case err := <-done:
		t.Fatalf("a disabled watchdog still fired: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	assert.Empty(t, srv.sentResponses())
	_, fired := findCounter(counters(), metrics.GatewayPDDecodeWatchdogTotal)
	assert.False(t, fired, "a disabled watchdog must not emit its counter")
	recorder.none(t, 100*time.Millisecond)

	awaitLoopCancelled(t, cancel, done)
}

// TestDecodeWatchdogDoesNotLeakAcrossStreams runs the whole watchdog path -
// arm, fire, abort, close - twenty times and checks nothing is left behind.
// goleak is not a module dependency here, so this counts goroutines with a
// settle loop, the same way TestRecvGoroutineDoesNotLeak does.
func TestDecodeWatchdogDoesNotLeakAcrossStreams(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	s, _ := newFailFastServer(t)

	settle := func() int {
		var n int
		for i := 0; i < 100; i++ {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
			n = runtime.NumGoroutine()
			if i > 5 && n == runtime.NumGoroutine() {
				break
			}
		}
		return n
	}

	before := settle()

	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		srv := newWatchdogProcessServer(ctx, 4)
		st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
			types.PDWatchdogOverrides{FirstResponseTimeout: 60 * time.Millisecond})
		st.routerCtx.PDLeg().MarkPrefillSucceeded()

		done := make(chan error, 1)
		go func() { done <- runProcessLoop(s, srv, st) }()

		select {
		case err := <-done:
			grpcStatus, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("stream %d: the watchdog did not fire", i)
		}

		// The abort goroutine is the one thing that outlives the loop; wait for
		// it so the count below measures leaks, not work still in flight.
		recorder.wait(t, 5*time.Second)
		awaitAbortsCounted(t, counters, i+1)

		// What Process's defer does, which is also the timer's only stop.
		st.stopDecodeWatchdog()
		select {
		case <-st.watchdog.C:
			t.Fatalf("stream %d: the fired timer must be left drained", i)
		default:
		}
		cancel()
	}

	// The aborts share one pooled keep-alive connection, whose read and write
	// loops (and the test server's side of it) are live goroutines by design
	// and would otherwise be counted as the leak this test is looking for.
	decodeAbortClient.CloseIdleConnections()

	after := settle()
	assert.LessOrEqual(t, after, before+2,
		"goroutines leaked: %d before, %d after 20 watchdog streams", before, after)
}

// TestDecodeWatchdogArmedAfterNonTerminalPrefillFailure: an SGLang prefill leg
// that answered 200 with a body the gateway could not parse is recorded as a
// bad_response failure, which fail-fast ignores because the KV transfer
// completed. The prefill goroutine then marks the leg succeeded as well, and
// the watchdog must still catch a decode pod that never answers.
func TestDecodeWatchdogArmedAfterNonTerminalPrefillFailure(t *testing.T) {
	counters := captureCounters(t)
	recorder, decodeAddr := newWatchdogAbortRecorder(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newWatchdogProcessServer(ctx, 4)
	st := newWatchdogState(ctx, watchdogTestRID, decodeAddr,
		types.PDWatchdogOverrides{FirstResponseTimeout: 150 * time.Millisecond})
	leg := st.routerCtx.PDLeg()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	// The order of the prefill goroutine: the failure first, then the success.
	time.Sleep(50 * time.Millisecond)
	leg.SetPrefillFailure(&types.PrefillFailure{Class: pd.PrefillFailureBadResponse, Message: "not json"})
	time.Sleep(50 * time.Millisecond)
	leg.MarkPrefillSucceeded()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not fire after a non-terminal prefill failure")
	}
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, grpcStatus.Code())

	sent := srv.sentResponses()
	require.Len(t, sent, 1)
	immediate := sent[0].GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, envoyTypePb.StatusCode_GatewayTimeout, immediate.GetStatus().GetCode())
	assert.Contains(t, immediate.GetBody(), "did not start responding")

	assert.Equal(t, watchdogTestRID, gjson.Get(recorder.wait(t, 5*time.Second), "rid").String())
	awaitAbortsCounted(t, counters, 1)
	select {
	case <-leg.AbortDone():
	case <-time.After(time.Second):
		t.Fatal("AbortDone was not closed after the watchdog abort finished")
	}

	emitted := counters()
	_, failFast := findCounter(emitted, metrics.GatewayPDPrefillFailureTotal)
	assert.False(t, failFast, "fail-fast must leave a non-terminal prefill failure alone")
	watchdog, ok := findCounter(emitted, metrics.GatewayPDDecodeWatchdogTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDDecodeWatchdogTotal)
	assert.Equal(t, decodeWatchdogPhaseFirstResponse, watchdog.labels["phase"])
}
