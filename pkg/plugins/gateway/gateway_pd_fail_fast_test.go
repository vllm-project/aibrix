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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

// blockingProcessServer is an ext_proc stream fake that hands out a scripted
// list of messages and then parks in Recv until the stream context dies, which
// is what the real stream does while Envoy waits on the decode pod.
type blockingProcessServer struct {
	ctx  context.Context
	msgs chan *extProcPb.ProcessingRequest

	mu   sync.Mutex
	sent []*extProcPb.ProcessingResponse
}

func newBlockingProcessServer(ctx context.Context, msgs ...*extProcPb.ProcessingRequest) *blockingProcessServer {
	ch := make(chan *extProcPb.ProcessingRequest, len(msgs))
	for _, m := range msgs {
		ch <- m
	}
	return &blockingProcessServer{ctx: ctx, msgs: ch}
}

func (f *blockingProcessServer) Recv() (*extProcPb.ProcessingRequest, error) {
	select {
	case m := <-f.msgs:
		return m, nil
	case <-f.ctx.Done():
		return nil, status.Error(codes.Canceled, "context canceled")
	}
}

func (f *blockingProcessServer) Send(resp *extProcPb.ProcessingResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, resp)
	return nil
}

func (f *blockingProcessServer) sentResponses() []*extProcPb.ProcessingResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*extProcPb.ProcessingResponse(nil), f.sent...)
}

func (f *blockingProcessServer) Context() context.Context     { return f.ctx }
func (f *blockingProcessServer) SetHeader(metadata.MD) error  { return nil }
func (f *blockingProcessServer) SendHeader(metadata.MD) error { return nil }
func (f *blockingProcessServer) SetTrailer(metadata.MD)       {}
func (f *blockingProcessServer) SendMsg(interface{}) error    { return nil }
func (f *blockingProcessServer) RecvMsg(interface{}) error    { return nil }

// capturedCounter is one emission through metrics.IncrementCounterMetricFnForTest.
type capturedCounter struct {
	name   string
	labels map[string]string
}

// captureCounters redirects counter emission for the duration of one test and
// returns an accessor for what was emitted.
func captureCounters(t *testing.T) func() []capturedCounter {
	t.Helper()

	var mu sync.Mutex
	var got []capturedCounter

	original := metrics.IncrementCounterMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name, help string, value float64, labelNames []string, labelValues ...string) {
		mu.Lock()
		defer mu.Unlock()
		labels := map[string]string{}
		for i, ln := range labelNames {
			if i < len(labelValues) {
				labels[ln] = labelValues[i]
			}
		}
		got = append(got, capturedCounter{name: name, labels: labels})
	}
	t.Cleanup(func() { metrics.IncrementCounterMetricFnForTest = original })

	return func() []capturedCounter {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedCounter(nil), got...)
	}
}

func findCounter(counters []capturedCounter, name string) (capturedCounter, bool) {
	for _, c := range counters {
		if c.name == name {
			return c, true
		}
	}
	return capturedCounter{}, false
}

// newFailFastState builds the processState the loop would hold just after
// RequestHeaders/RequestBody have been answered for a PD request: a routing
// context is attached, the prefill leg is in flight, and nothing has come back
// from the decode pod yet.
func newFailFastState(ctx context.Context) *processState {
	routerCtx := types.NewRoutingContext(ctx, routing.RouterPD, "test-model", "hello", "req-fail-fast", "")
	routerCtx.SetPDRequestID("req-fail-fast-00000000deadbeef")
	return &processState{
		ctx:       ctx,
		requestID: "req-fail-fast",
		model:     "test-model",
		routerCtx: routerCtx,
	}
}

// runProcessLoop mirrors the loop in Process so a test can drive processOnce
// with a pre-populated processState instead of routing a real request.
func runProcessLoop(s *Server, srv extProcPb.ExternalProcessor_ProcessServer, st *processState) error {
	for {
		if err := s.processOnce(srv, st); err != nil {
			return err
		}
		if st.completed {
			return nil
		}
	}
}

func newFailFastServer(t *testing.T) (*Server, *MockCache) {
	t.Helper()
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	return &Server{
		shutdownCh:          make(chan struct{}),
		cache:               mc,
		requestCountTracker: map[string]int{},
	}, mc
}

// TestProcessErrorsStreamWhenPrefillFailsBeforeDecodeResponse is the case the
// whole feature exists for: the prefill leg dies while the loop is parked
// waiting for decode response headers that will never come.
func TestProcessErrorsStreamWhenPrefillFailsBeforeDecodeResponse(t *testing.T) {
	counters := captureCounters(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, mc := newFailFastServer(t)
	srv := newBlockingProcessServer(ctx)

	st := newFailFastState(ctx)

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	// Let the loop reach the select before the failure is recorded, so the
	// wakeup is an edge and not an already-closed channel.
	time.Sleep(50 * time.Millisecond)
	st.routerCtx.SetPrefillFailure(&types.PrefillFailure{
		Class:   pd.PrefillFailureTransport,
		Message: "Post \"http://10.0.0.1:8000/v1/chat/completions\": dial tcp: connect: connection refused",
		At:      time.Now(),
	})

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("processing loop did not return within 1s of the prefill failure")
	}

	require.Error(t, err)
	grpcStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Aborted, grpcStatus.Code())
	assert.Contains(t, grpcStatus.Message(), pd.PrefillFailureTransport)

	// The client is answered with an ImmediateResponse before the stream dies,
	// so a filter that honours it sees the mapped status and body.
	sent := srv.sentResponses()
	require.Len(t, sent, 1)
	immediate := sent[0].GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, immediate.GetStatus().GetCode())
	assert.Contains(t, immediate.GetBody(), pd.PrefillFailureTransport)
	assert.Contains(t, immediate.GetBody(), "connection refused")

	headers := map[string]string{}
	for _, h := range immediate.GetHeaders().GetSetHeaders() {
		headers[h.GetHeader().GetKey()] = string(h.GetHeader().GetRawValue())
	}
	assert.Equal(t, "true", headers[HeaderErrorPDPrefill], "expected the %s marker header", HeaderErrorPDPrefill)
	assert.Equal(t, st.requestID, headers[HeaderRequestID], "the response must stay correlatable with the gateway logs")

	emitted := counters()
	failure, ok := findCounter(emitted, metrics.GatewayPDPrefillFailureTotal)
	require.True(t, ok, "expected %s to be emitted", metrics.GatewayPDPrefillFailureTotal)
	assert.Equal(t, pd.PrefillFailureTransport, failure.labels["class"])
	assert.Equal(t, prefillFailFastStageBefore, failure.labels["stage"])

	requestFail, ok := findCounter(emitted, metrics.GatewayRequestModelFailTotal)
	require.True(t, ok, "a fail-fast must be counted as a failed request")
	assert.Equal(t, prefillFailFastStatus, requestFail.labels["status"])
	assert.Equal(t, "503", requestFail.labels["status_code"])
	for _, c := range emitted {
		assert.NotEqual(t, metrics.GatewayRequestModelSuccessTotal, c.name,
			"a fail-fast must never count as a gateway request success")
	}

	// The request is finalized at the failure rather than at the decode pod's
	// bootstrap timeout.
	mc.AssertCalled(t, "DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestProcessIgnoresBadResponsePrefillFailure: a prefill HTTP 200 the gateway
// could not parse still completed the KV transfer, so the decode leg is alive
// and the client must not be failed.
func TestProcessIgnoresBadResponsePrefillFailure(t *testing.T) {
	counters := captureCounters(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newBlockingProcessServer(ctx)

	st := newFailFastState(ctx)

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	time.Sleep(50 * time.Millisecond)
	st.routerCtx.SetPrefillFailure(&types.PrefillFailure{
		Class:   pd.PrefillFailureBadResponse,
		Message: "unmarshal prefill response: unexpected end of JSON input",
		At:      time.Now(),
	})

	select {
	case err := <-done:
		t.Fatalf("loop returned on a bad_response prefill failure: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	assert.Empty(t, srv.sentResponses(), "no response may be synthesised for bad_response")
	_, ok := findCounter(counters(), metrics.GatewayPDPrefillFailureTotal)
	assert.False(t, ok, "bad_response is not a fail-fast and must not be counted as one")

	// Unblock the loop the way Envoy would.
	cancel()
	select {
	case err := <-done:
		grpcStatus, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.Canceled, grpcStatus.Code())
	case <-time.After(time.Second):
		t.Fatal("loop did not return after the stream context was cancelled")
	}
}

// TestProcessDoesNotErrorAfterDecodeResponded: once the decode pod is writing
// to the client the response cannot be replaced, so the failure is recorded and
// the stream is left alone.
func TestProcessDoesNotErrorAfterDecodeResponded(t *testing.T) {
	counters := captureCounters(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, _ := newFailFastServer(t)
	srv := newBlockingProcessServer(ctx)

	st := newFailFastState(ctx)
	st.stream = true
	st.routerCtx.MarkDecodeResponded()

	done := make(chan error, 1)
	go func() { done <- runProcessLoop(s, srv, st) }()

	time.Sleep(50 * time.Millisecond)
	st.routerCtx.SetPrefillFailure(&types.PrefillFailure{
		Class:   pd.PrefillFailureTransport,
		Message: "connection reset by peer",
		At:      time.Now(),
	})

	select {
	case err := <-done:
		t.Fatalf("loop failed a stream the decode pod was already answering: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	assert.Empty(t, srv.sentResponses(), "an in-flight response must not be replaced")

	failure, ok := findCounter(counters(), metrics.GatewayPDPrefillFailureTotal)
	require.True(t, ok, "the after-response path still records the failure")
	assert.Equal(t, pd.PrefillFailureTransport, failure.labels["class"])
	assert.Equal(t, prefillFailFastStageAfter, failure.labels["stage"])

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not return after the stream context was cancelled")
	}
}

// TRT's SSE headers can precede KV arrival. Unlike SGLang, a terminal CTX
// failure after headers must still reset the stream, not leave a GEN waiter.
func TestTRTPrefillFailureResetsStream(t *testing.T) {
	for _, afterHeaders := range []bool{false, true} {
		name := "before_headers"
		if afterHeaders {
			name = "after_headers"
		}
		t.Run(name, func(t *testing.T) {
			counters := captureCounters(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, _ := newFailFastServer(t)
			srv := newBlockingProcessServer(ctx)
			st := newFailFastState(ctx)
			defer st.routerCtx.Delete()
			st.routerCtx.Engine = pd.EngineTRTLLM
			st.stream = true
			if afterHeaders {
				st.routerCtx.MarkDecodeResponded()
			}
			st.routerCtx.SetPrefillFailure(&types.PrefillFailure{Class: pd.PrefillFailureTimeout, Message: "CTX timed out"})
			done := make(chan error, 1)
			go func() { done <- runProcessLoop(s, srv, st) }()
			select {
			case err := <-done:
				assert.Equal(t, codes.Aborted, status.Code(err))
			case <-time.After(5 * time.Second):
				t.Fatal("TRT generation leg left waiting for KV")
			}
			if afterHeaders {
				assert.Empty(t, srv.sentResponses(), "cannot replace headers already sent to the client")
			} else {
				require.Len(t, srv.sentResponses(), 1)
				assert.NotNil(t, srv.sentResponses()[0].GetImmediateResponse())
			}
			_, ok := findCounter(counters(), metrics.GatewayRequestModelFailTotal)
			assert.True(t, ok, "stream reset must be counted as a failed request")
		})
	}
}

func TestTRTBadPrefillResponseDoesNotResetStream(t *testing.T) {
	s, _ := newFailFastServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := newFailFastState(ctx)
	defer st.routerCtx.Delete()
	st.routerCtx.Engine = pd.EngineTRTLLM
	st.routerCtx.MarkDecodeResponded()
	st.routerCtx.SetPrefillFailure(&types.PrefillFailure{Class: pd.PrefillFailureBadResponse})
	srv := newBlockingProcessServer(ctx)
	require.NoError(t, s.handlePrefillFailFast(srv, st))
	assert.Empty(t, srv.sentResponses())
}

// TestRecvGoroutineDoesNotLeak checks that the Recv goroutine the loop starts -
// which now outlives a single processOnce call, because a fail-fast decision
// can return without consuming its message - still dies with the stream.
// goleak is not a module dependency here, so this counts goroutines with a
// settle loop instead.
func TestRecvGoroutineDoesNotLeak(t *testing.T) {
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
		srv := newBlockingProcessServer(ctx)
		done := make(chan error, 1)
		go func() { done <- s.Process(srv) }()

		time.Sleep(5 * time.Millisecond)
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Process did not return after the stream context was cancelled")
		}
	}

	after := settle()
	assert.LessOrEqual(t, after, before+2,
		"stream reader goroutines leaked: %d before, %d after 20 streams", before, after)
}

// TestPrefillFailFastStatusMapping pins the class -> client status table.
func TestPrefillFailFastStatusMapping(t *testing.T) {
	cases := []struct {
		name     string
		failure  *types.PrefillFailure
		expected envoyTypePb.StatusCode
	}{
		{"timeout", &types.PrefillFailure{Class: pd.PrefillFailureTimeout}, envoyTypePb.StatusCode_ServiceUnavailable},
		{"transport", &types.PrefillFailure{Class: pd.PrefillFailureTransport}, envoyTypePb.StatusCode_ServiceUnavailable},
		{"request_setup", &types.PrefillFailure{Class: pd.PrefillFailureRequestSetup}, envoyTypePb.StatusCode_ServiceUnavailable},
		{"canceled", &types.PrefillFailure{Class: pd.PrefillFailureCanceled}, envoyTypePb.StatusCode_ServiceUnavailable},
		{"http 400 passes through", &types.PrefillFailure{Class: pd.PrefillFailureHTTPStatus, StatusCode: 400}, envoyTypePb.StatusCode_BadRequest},
		{"http 500 passes through", &types.PrefillFailure{Class: pd.PrefillFailureHTTPStatus, StatusCode: 500}, envoyTypePb.StatusCode_InternalServerError},
		{"http 200 degrades to 502", &types.PrefillFailure{Class: pd.PrefillFailureHTTPStatus, StatusCode: 200}, envoyTypePb.StatusCode_BadGateway},
		{"http 499 unknown to envoy degrades to 502", &types.PrefillFailure{Class: pd.PrefillFailureHTTPStatus, StatusCode: 499}, envoyTypePb.StatusCode_BadGateway},
		{"nil", nil, envoyTypePb.StatusCode_ServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, prefillFailureStatusCode(tc.failure))
		})
	}
}

// TestPrefillFailFastTruncatesUpstreamMessage keeps an arbitrarily long engine
// error from being copied wholesale into a client-facing body.
func TestPrefillFailFastTruncatesUpstreamMessage(t *testing.T) {
	long := strings.Repeat("x", maxPrefillFailureMessage*2)
	got := truncatePrefillMessage(long)
	assert.Len(t, got, maxPrefillFailureMessage+3)
	assert.True(t, strings.HasSuffix(got, "..."))
	assert.Equal(t, "short", truncatePrefillMessage("short"))
}

// bufferedRequestBody is a message of the kind Envoy sends before the decode
// pod answers, and which the gateway can reply to on its own. What the reply
// says does not matter here - only that the loop produced one, and did so
// before it acted on the prefill failure.
func bufferedRequestBody() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`),
			},
		},
	}
}

// TestProcessFailsFastAfterBufferedMessageWinsTheTie is the regression test for
// a wakeup that was disarmed by the tie-break instead of by the handling: when
// a message from Envoy was already sitting in the Recv channel as the prefill
// leg died, the failure was marked "done" while nothing had been done about it,
// and the client waited out the decode pod's bootstrap timeout after all.
//
// Both cases of the outer select are ready in this scenario and Go picks
// between them uniformly, so the round is repeated: the two arms must behave
// identically, and over this many rounds the tie-break inside the wakeup arm is
// taken with near-certainty.
func TestProcessFailsFastAfterBufferedMessageWinsTheTie(t *testing.T) {
	const rounds = 20

	for i := 0; i < rounds; i++ {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			s, _ := newFailFastServer(t)
			// No further messages: once the buffered one is consumed, the only
			// thing that can end the stream is the fail-fast.
			srv := newBlockingProcessServer(ctx)
			st := newFailFastState(ctx)

			// The message Envoy has already sent and is waiting on, in the
			// channel the in-flight Recv would have delivered it through.
			st.recvCh = make(chan recvResult, 1)
			st.recvCh <- recvResult{req: bufferedRequestBody()}

			// ... and the prefill leg dies before the loop looks at either.
			st.routerCtx.SetPrefillFailure(&types.PrefillFailure{
				Class:   pd.PrefillFailureTransport,
				Message: "dial tcp: connect: connection refused",
				At:      time.Now(),
			})

			require.NoError(t, s.processOnce(srv, st), "round %d: the buffered message must be answered, not failed", i)
			require.Len(t, srv.sentResponses(), 1, "round %d: the message Envoy is waiting on must be answered first", i)
			require.False(t, st.prefillFailFastDone,
				"round %d: the wakeup must stay armed until the failure has actually been handled", i)

			err := s.processOnce(srv, st)
			require.Error(t, err, "round %d: the prefill failure must still end the stream", i)
			assert.Equal(t, codes.Aborted, status.Code(err))
			assert.True(t, st.prefillFailFastDone, "round %d: the handled failure must disarm the wakeup", i)

			sent := srv.sentResponses()
			require.Len(t, sent, 2, "round %d", i)
			immediate := sent[1].GetImmediateResponse()
			require.NotNil(t, immediate, "round %d: the client must still be told the request failed", i)
			assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, immediate.GetStatus().GetCode())

			headers := map[string]string{}
			for _, h := range immediate.GetHeaders().GetSetHeaders() {
				headers[h.GetHeader().GetKey()] = string(h.GetHeader().GetRawValue())
			}
			assert.Equal(t, "true", headers[HeaderErrorPDPrefill], "round %d: expected the %s marker header", i, HeaderErrorPDPrefill)
		}()
	}
}
