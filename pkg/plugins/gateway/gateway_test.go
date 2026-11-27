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
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/errresp"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_buildEnvoyProxyHeaders(t *testing.T) {
	headers := []*configPb.HeaderValueOption{}

	headers = errresp.BuildEnvoyProxyHeaders(headers, "key1", "value1", "key2")
	assert.Equal(t, 0, len(headers))

	headers = errresp.BuildEnvoyProxyHeaders(headers, "key1", "value1", "key2", "value2")
	assert.Equal(t, 2, len(headers))

	headers = errresp.BuildEnvoyProxyHeaders(headers, "key3", "value3")
	assert.Equal(t, 3, len(headers))
}

func Test_getMetricErr(t *testing.T) {
	t.Run("uses Header.Value when present", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: metricHeaderErr, Value: "bad"}},
				},
			},
		}
		assert.Equal(t, "gateway_req_headers_bad", getMetricErr(ir, "gateway_req_headers"))
	})

	t.Run("uses Header.RawValue when Value empty", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: metricHeaderErr, RawValue: []byte("oops")}},
				},
			},
		}
		assert.Equal(t, "gateway_rsp_headers_oops", getMetricErr(ir, "gateway_rsp_headers"))
	})

	t.Run("returns label underscore when header missing", func(t *testing.T) {
		ir := &extProcPb.ImmediateResponse{
			Headers: &extProcPb.HeaderMutation{
				SetHeaders: []*configPb.HeaderValueOption{
					{Header: &configPb.HeaderValue{Key: "x-other", RawValue: []byte("1")}},
				},
			},
		}
		assert.Equal(t, "gateway_rsp_body", getMetricErr(ir, "gateway_rsp_body"))
	})
}

func TestHandleProcessingRequest_NoResponseGenerated_ReturnsInternalErrorAndCleansUp(t *testing.T) {
	mc := &MockCache{}
	// routerCtx is nil in this scenario; traceTerm defaults to 0.
	mc.On("DoneRequestCount", (*types.RoutingContext)(nil), "rid", "m", int64(0)).Return()

	s := &Server{
		cache: mc,
	}
	st := &processState{
		ctx:       context.Background(),
		requestID: "rid",
		model:     "m",
	}

	// ProcessingRequest with no concrete oneof set => default branch, no resp produced.
	req := &extProcPb.ProcessingRequest{}

	resp, err := s.handleProcessingRequest(st, req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	stErr, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Internal, stErr.Code())
	assert.Contains(t, stErr.Message(), "no response generated")

	mc.AssertExpectations(t)
}

// mockProcessServer implements extProcPb.ExternalProcessor_ProcessServer for testing.
type mockProcessServer struct {
	mock.Mock
	ctx context.Context
}

func (m *mockProcessServer) Send(resp *extProcPb.ProcessingResponse) error {
	args := m.Called(resp)
	return args.Error(0)
}

func (m *mockProcessServer) Recv() (*extProcPb.ProcessingRequest, error) {
	args := m.Called()
	req, _ := args.Get(0).(*extProcPb.ProcessingRequest)
	return req, args.Error(1)
}

func (m *mockProcessServer) Context() context.Context     { return m.ctx }
func (m *mockProcessServer) SetHeader(metadata.MD) error  { return nil }
func (m *mockProcessServer) SendHeader(metadata.MD) error { return nil }
func (m *mockProcessServer) SetTrailer(metadata.MD)       {}
func (m *mockProcessServer) SendMsg(interface{}) error    { return nil }
func (m *mockProcessServer) RecvMsg(interface{}) error    { return nil }

// newProcessTestServer creates a minimal Server for Process tests.
func newProcessTestServer(shutdownCh <-chan struct{}, c *MockCache) *Server {
	return &Server{
		shutdownCh:          shutdownCh,
		cache:               c,
		requestCountTracker: map[string]int{},
	}
}

func closedShutdownCh() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func openShutdownCh() <-chan struct{} {
	return make(chan struct{})
}

// TestProcess_ServerShutdown verifies that Process exits immediately when the
// shutdown channel is closed before any message is received.
func TestProcess_ServerShutdown(t *testing.T) {
	mc := &MockCache{}
	// st.model is empty (idle stream); DoneRequestCount must not be called.

	srv := &mockProcessServer{ctx: context.Background()}
	s := newProcessTestServer(closedShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), "server shutdown in progress")

	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

// TestProcess_ContextCancelled verifies that Process exits when the client
// context is cancelled before any message is received.
func TestProcess_ContextCancelled(t *testing.T) {
	mc := &MockCache{}
	// st.model is empty (idle stream); DoneRequestCount must not be called.

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := &mockProcessServer{ctx: ctx}
	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.ErrorIs(t, err, context.Canceled)

	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

// TestSendProcessingResponse_SendFailure verifies that a transport failure is
// always propagated, even for early rejections where no model was resolved.
// Accounting (metrics + DoneRequestCount) still only runs when a model was set,
// since AddRequestCount never ran for model-less requests.
func TestSendProcessingResponse_SendFailure(t *testing.T) {
	sendErr := errors.New("transport closed")
	resp := &extProcPb.ProcessingResponse{}

	t.Run("empty model propagates error without accounting", func(t *testing.T) {
		mc := &MockCache{}
		s := newProcessTestServer(openShutdownCh(), mc)
		srv := &mockProcessServer{ctx: context.Background()}
		srv.On("Send", resp).Return(sendErr).Once()

		st := &processState{ctx: context.Background(), requestID: "req-no-model", model: ""}
		err := s.sendProcessingResponse(srv, st, resp)

		assert.ErrorIs(t, err, sendErr)
		mc.AssertNotCalled(t, "DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		srv.AssertExpectations(t)
	})

	t.Run("known model propagates error with accounting", func(t *testing.T) {
		mc := &MockCache{}
		mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Once()
		s := newProcessTestServer(openShutdownCh(), mc)
		srv := &mockProcessServer{ctx: context.Background()}
		srv.On("Send", resp).Return(sendErr).Once()

		st := &processState{ctx: context.Background(), requestID: "req-model", model: "test-model"}
		err := s.sendProcessingResponse(srv, st, resp)

		assert.ErrorIs(t, err, sendErr)
		mc.AssertExpectations(t)
		srv.AssertExpectations(t)
	})

	t.Run("send success returns nil", func(t *testing.T) {
		mc := &MockCache{}
		s := newProcessTestServer(openShutdownCh(), mc)
		srv := &mockProcessServer{ctx: context.Background()}
		srv.On("Send", resp).Return(nil).Once()

		st := &processState{ctx: context.Background(), requestID: "req-ok", model: ""}
		assert.NoError(t, s.sendProcessingResponse(srv, st, resp))
		srv.AssertExpectations(t)
	})
}

// TestProcess_RecvEOF_NotCompleted verifies that an EOF received before the
// request completes is surfaced as io.EOF (client closed stream prematurely).
func TestProcess_RecvEOF_NotCompleted(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Equal(t, io.EOF, err)

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

func TestProcess_CleansRequestBufferOnExitBeforeResponseEnd(t *testing.T) {
	mc := &MockCache{}
	mc.On("AddRequestCount", mock.Anything, mock.Anything, mock.Anything).Return(int64(0))
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mc.On("HasModel", mock.Anything).Return(true)
	pods := &utils.PodArray{Pods: []*v1.Pod{
		{
			Status: v1.PodStatus{
				PodIP:      "1.2.3.4",
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		},
	}}
	mc.On("ListPodsByModel", mock.Anything).Return(pods, nil)

	const requestID = "4bf92f3577b34da6a3ce929d0e0e4736"
	requestBuffers.Delete(requestID)

	srv := &mockProcessServer{ctx: context.Background()}
	req := &extProcPb.ProcessingRequest{}
	recvCount := 0
	srv.On("Recv").Return(req, nil).Run(func(args mock.Arguments) {
		switch recvCount {
		case 0:
			req.Request = &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{
						Headers: []*configPb.HeaderValue{
							{Key: ":routing-strategy", Value: "random", RawValue: []byte("random")},
							{Key: ":path", Value: "/v1/chat/completions", RawValue: []byte("/v1/chat/completions")},
							{Key: HeaderTraceParent, Value: "00-" + requestID + "-00f067aa0ba902b7-01", RawValue: []byte("00-" + requestID + "-00f067aa0ba902b7-01")},
						},
					},
				},
			}
		case 1:
			req.Request = &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{
					Body: []byte(`{"model": "test", "messages": [{"role": "user", "content": "hello"}]}`),
				},
			}
		case 2:
			req.Request = &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{
					Body:        []byte(`{"model": "test", "usage": {"prompt_tokens": `),
					EndOfStream: false,
				},
			}
		}
		recvCount++
	}).Times(3)
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Once()
	srv.On("Send", mock.Anything).Return(nil)

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Equal(t, io.EOF, err)
	_, ok := requestBuffers.Load(requestID)
	assert.False(t, ok, "expected request buffer to be removed when Process exits before response end")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_CleansStreamUsageWhenStreamIsCutOff covers a continuous-usage stream
// that ends before its final usage, e.g. on a client disconnect: the running usage
// kept since the first chunk must be released when Process exits.
func TestProcess_CleansStreamUsageWhenStreamIsCutOff(t *testing.T) {
	mc := &MockCache{}
	mc.On("AddRequestCount", mock.Anything, mock.Anything, mock.Anything).Return(int64(0))
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mc.On("HasModel", mock.Anything).Return(true)
	pods := &utils.PodArray{Pods: []*v1.Pod{
		{
			Status: v1.PodStatus{
				PodIP:      "1.2.3.4",
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		},
	}}
	mc.On("ListPodsByModel", mock.Anything).Return(pods, nil)

	const requestID = "5ce92f3577b34da6a3ce929d0e0e4737"
	t.Cleanup(func() {
		requestBuffers.Delete(requestID)
		streamBuffers.Delete(requestID)
		streamUsage.Delete(requestID)
	})

	srv := &mockProcessServer{ctx: context.Background()}
	req := &extProcPb.ProcessingRequest{}
	recvCount := 0
	srv.On("Recv").Return(req, nil).Run(func(args mock.Arguments) {
		switch recvCount {
		case 0:
			req.Request = &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{
						Headers: []*configPb.HeaderValue{
							{Key: ":routing-strategy", Value: "random", RawValue: []byte("random")},
							{Key: ":path", Value: "/v1/chat/completions", RawValue: []byte("/v1/chat/completions")},
							{Key: HeaderTraceParent, Value: "00-" + requestID + "-00f067aa0ba902b7-01", RawValue: []byte("00-" + requestID + "-00f067aa0ba902b7-01")},
						},
					},
				},
			}
		case 1:
			req.Request = &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{
					Body: []byte(`{"model": "test", "stream": true, "stream_options": {"include_usage": true, "continuous_usage_stats": true}, "messages": [{"role": "user", "content": "hello"}]}`),
				},
			}
		case 2:
			// A running usage alongside content: not final, so it is kept.
			req.Request = &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{
					Body: []byte(`data: {"id": "1", "choices": [{"index": 0, "delta": {"content": "hi"}}], "usage": {"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11}}` + "\n\n"),
				},
			}
		}
		recvCount++
	}).Times(3)
	// The client goes away before the stream ends.
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Run(func(args mock.Arguments) {
		_, kept := streamUsage.Load(requestID)
		assert.True(t, kept, "the running usage should be held until the stream ends")
		_, requestBuffered := requestBuffers.Load(requestID)
		_, streamBuffered := streamBuffers.Load(requestID)
		assert.True(t, !requestBuffered && !streamBuffered && HasRequestBuffers(requestID),
			"HasRequestBuffers should report the running usage on its own")
	}).Once()
	srv.On("Send", mock.Anything).Return(nil)

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Equal(t, io.EOF, err)
	assert.False(t, HasRequestBuffers(requestID), "Process must release the running usage of a stream that was cut off")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvGRPCCanceled verifies that a gRPC Canceled error from Recv
// is treated as a normal stream closure and returned as codes.Canceled.
func TestProcess_RecvGRPCCanceled(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Canceled, "client disconnected")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Canceled, st.Code())

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvGRPCError verifies that an unexpected gRPC error from Recv
// is propagated back to the caller.
func TestProcess_RecvGRPCError(t *testing.T) {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Internal, "something broke")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvNonGRPCError verifies that a non-gRPC error from Recv is
// wrapped and returned as a codes.Unknown gRPC error.
func TestProcess_RecvNonGRPCError(t *testing.T) {
	mc := &MockCache{}
	// DoneRequestCount is NOT called for non-gRPC recv errors.

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), errors.New("transport error")).Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Contains(t, st.Message(), "recv stream error")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_RecvEOF_DuringShutdown verifies that an EOF received while a
// server shutdown is in progress results in a codes.Unavailable error.
func TestProcess_RecvEOF_DuringShutdown(t *testing.T) {
	mc := &MockCache{}
	// st.model is empty (idle stream); DoneRequestCount must not be called.

	shutdownCh := make(chan struct{})

	srv := &mockProcessServer{ctx: context.Background()}
	// Close the shutdown channel when Recv is called so that handleRecvError
	// picks it up (preRecvCheck already passed via the default branch).
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), io.EOF).Once().
		Run(func(args mock.Arguments) { close(shutdownCh) })

	s := newProcessTestServer(shutdownCh, mc)

	err := s.Process(srv)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), "server shutdown in progress")

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_CompletedExitsLoop verifies that if a request completes successfully
// (which sets st.completed = true), the loop exits gracefully returning nil,
// even if the client context is cancelled concurrently.
func TestProcess_CompletedExitsLoop(t *testing.T) {
	mc := &MockCache{}
	// A successful response with usage data is finalized through
	// DoneRequestTrace only; Process must not follow it with DoneRequestCount.
	mc.On("AddRequestCount", mock.Anything, mock.Anything, mock.Anything).Return(int64(0))
	mc.On("DoneRequestTrace", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mc.On("HasModel", mock.Anything).Return(true)
	pods := &utils.PodArray{Pods: []*v1.Pod{
		{
			Status: v1.PodStatus{
				PodIP:      "1.2.3.4",
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		},
	}}
	mc.On("ListPodsByModel", mock.Anything).Return(pods, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &mockProcessServer{ctx: ctx}

	req := &extProcPb.ProcessingRequest{}
	recvCount := 0
	// we need to fully test s.Process, so we mock a full stream request with header body and resp
	srv.On("Recv").Return(req, nil).Run(func(args mock.Arguments) {
		switch recvCount {
		case 0:
			// mock on RequestHeaders
			req.Request = &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{
					Headers: &configPb.HeaderMap{
						Headers: []*configPb.HeaderValue{
							{Key: ":routing-strategy", Value: "random", RawValue: []byte("random")},
							{Key: ":path", Value: "/v1/chat/completions", RawValue: []byte("/v1/chat/completions")},
						},
					},
				},
			}
		case 1:
			// mock on RequestBody，contains stream=true
			req.Request = &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{
					Body: []byte(`{"model": "test", "messages": [{"role": "user", "content": "hello"}], "stream": true}`),
				},
			}
		case 2:
			// mock on ResponseBody，with [DONE]
			req.Request = &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{
					Body:        []byte("data: [DONE]\n\n"),
					EndOfStream: true, // if we get [Done], completed == True
				},
			}
			cancel()
		}
		recvCount++
	})

	srv.On("Send", mock.Anything).Return(nil).Run(nil)

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	// Core assertion: Since st.completed has already been set to true,
	// even if ctx has been canceled, Process should exit gracefully (return nil) instead of throwing a context.Canceled error.
	assert.NoError(t, err)

	srv.AssertExpectations(t)
	mc.AssertExpectations(t)
}

// TestProcess_ShutdownWhileRecvBlocked verifies that Process exits promptly when
// shutdownCh is closed while srv.Recv() is blocking (the idle-stream case that
// previously caused GracefulStop to hang until SIGKILL).
func TestProcess_ShutdownWhileRecvBlocked(t *testing.T) {
	mc := &MockCache{}
	// st.model is empty (idle stream); DoneRequestCount must not be called.

	shutdownCh := make(chan struct{})

	srv := &mockProcessServer{ctx: context.Background()}
	// Recv blocks until shutdownCh is closed, then returns an error.
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Unavailable, "stream closed")).
		Run(func(args mock.Arguments) {
			// Simulate Envoy holding an idle stream open; close shutdown concurrently.
			close(shutdownCh)
			// Recv itself returns after shutdown (as it would when the gRPC server
			// eventually tears down the connection), but the select should have
			// already fired the shutdownCh case and returned before this matters.
			time.Sleep(10 * time.Millisecond)
		})

	s := newProcessTestServer(shutdownCh, mc)

	done := make(chan error, 1)
	go func() { done <- s.Process(srv) }()

	select {
	case err := <-done:
		assert.Error(t, err)
		st, ok := status.FromError(err)
		assert.True(t, ok)
		assert.Equal(t, codes.Unavailable, st.Code())
		assert.Contains(t, st.Message(), "server shutdown in progress")
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not exit within 2s after shutdownCh was closed (would have hung GracefulStop)")
	}

	mc.AssertExpectations(t)
}

func TestProcess_RequestIDFromSpanContext(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)

	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), spanContext)

	mc := &MockCache{}
	mc.On(
		"DoneRequestCount",
		mock.Anything,
		traceID.String(),
		"",
		int64(0),
	).Return().Once()

	srv := &mockProcessServer{ctx: ctx}
	srv.On("Recv").
		Return((*extProcPb.ProcessingRequest)(nil), io.EOF).
		Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err = s.Process(srv)

	assert.ErrorIs(t, err, io.EOF)
	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

func TestProcess_RequestIDFallsBackToUUID(t *testing.T) {
	var gotRequestID string

	mc := &MockCache{}
	mc.On(
		"DoneRequestCount",
		mock.Anything,
		mock.AnythingOfType("string"),
		"",
		int64(0),
	).Run(func(args mock.Arguments) {
		gotRequestID = args.String(1)
	}).Return().Once()

	srv := &mockProcessServer{ctx: context.Background()}
	srv.On("Recv").
		Return((*extProcPb.ProcessingRequest)(nil), io.EOF).
		Once()

	s := newProcessTestServer(openShutdownCh(), mc)

	err := s.Process(srv)

	assert.ErrorIs(t, err, io.EOF)
	assert.NotEmpty(t, gotRequestID)

	_, parseErr := uuid.Parse(gotRequestID)
	assert.NoError(t, parseErr, "request ID should fall back to a UUID")

	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

func requestOutcomeCounters(counters []capturedCounter) []capturedCounter {
	var outcomes []capturedCounter
	for _, c := range counters {
		if c.name == metrics.GatewayRequestModelSuccessTotal || c.name == metrics.GatewayRequestModelFailTotal {
			outcomes = append(outcomes, c)
		}
	}
	return outcomes
}

// TestProcess_TerminalOutcome counts terminal metric increments from real handlers.
func TestProcess_TerminalOutcome(t *testing.T) {
	const requestBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	const missingBody = `{"model":"missing-model","messages":[{"role":"user","content":"hi"}]}`
	const errorBody = `{"error":{"message":"invalid parameter","type":"BadRequestError","code":400}}`
	const successBody = `{"model":"test-model","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	canceled := status.Error(codes.Canceled, "Envoy closed stream")
	for _, tt := range []struct {
		name, requestBody, upstreamStatus, responseBody, wantStatus, wantCode string
		end                                                                   error
	}{
		{"StreamingEngine400Completes", strings.TrimSuffix(requestBody, "}") + `,"stream":true}`, "400", errorBody, "gateway_rsp_body_fail", "400", nil},
		{"CompletedImmediateResponseProactiveExit", requestBody, "200", `{"model":`, "gateway_rsp_body_fail", "500", nil},
		{"Engine400Completes", requestBody, "400", errorBody, "gateway_rsp_body_fail", "400", nil},
		{"ModelNotFoundThenCanceled", missingBody, "", "", "gateway_req_body_fail", "400", canceled},
		{"CompletedBeforeCanceled", requestBody, "200", successBody, "gateway_rsp_body_success", "200", nil},
		{"ClientDisconnectEOF", requestBody, "", "", "client_cancelled_eof", "499", io.EOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			counters := captureCounters(t)
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"}, Status: v1.PodStatus{
				PodIP: "1.2.3.4", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			}}
			s := &Server{cache: cache.NewWithPodsForTest([]*v1.Pod{pod}, "test-model"), shutdownCh: openShutdownCh()}
			srv := &mockProcessServer{ctx: context.Background()}
			requests := []*extProcPb.ProcessingRequest{
				{Request: &extProcPb.ProcessingRequest_RequestHeaders{RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":path", RawValue: []byte(PathChatCompletions)},
					{Key: HeaderRoutingStrategy, RawValue: []byte("random")},
				}}}}},
				{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte(tt.requestBody)}}},
			}
			if tt.upstreamStatus != "" {
				requests = append(requests,
					&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte(tt.upstreamStatus)},
					}}}}},
					&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{Body: []byte(tt.responseBody), EndOfStream: tt.upstreamStatus != "400"}}})
				if tt.upstreamStatus == "400" {
					requests = append(requests, errorBodyChunk("", true))
				}
			}
			for _, req := range requests {
				srv.On("Recv").Return(req, nil).Once()
			}
			if tt.end != nil {
				srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), tt.end).Once()
			}
			immediateResponses := 0
			srv.On("Send", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				if imm := args.Get(0).(*extProcPb.ProcessingResponse).GetImmediateResponse(); imm != nil {
					immediateResponses++
					assert.Equal(t, tt.wantCode, fmt.Sprint(int(imm.GetStatus().GetCode())))
					if tt.requestBody == missingBody {
						assert.Contains(t, imm.GetBody(), "model_not_found")
					}
				}
			})
			err := s.Process(srv)
			switch tt.end {
			case nil:
				assert.NoError(t, err)
				srv.AssertNumberOfCalls(t, "Recv", len(requests))
			case io.EOF:
				assert.ErrorIs(t, err, io.EOF)
			default:
				assert.Equal(t, codes.Canceled, status.Code(err))
			}
			wantImmediate := 0
			if strings.HasSuffix(tt.wantStatus, "_fail") && tt.upstreamStatus != "400" {
				wantImmediate = 1
			}
			assert.Equal(t, wantImmediate, immediateResponses)
			outcomes := requestOutcomeCounters(counters())
			require.Len(t, outcomes, 1, "unexpected terminal counter increments: %+v", outcomes)
			wantMetric := metrics.GatewayRequestModelFailTotal
			if tt.wantCode == "200" {
				wantMetric = metrics.GatewayRequestModelSuccessTotal
			}
			assert.Equal(t, wantMetric, outcomes[0].name)
			assert.Equal(t, tt.wantStatus, outcomes[0].labels["status"])
			assert.Equal(t, tt.wantCode, outcomes[0].labels["status_code"])
			srv.AssertExpectations(t)
		})
	}
}

func TestTerminalFailure_ResponseOutcome(t *testing.T) {
	for _, tt := range []struct {
		name, wantStatus, wantCode string
		end                        error
	}{
		{"IncompleteEOF", "client_cancelled_eof", "499", io.EOF},
		{"GRPCError", "gateway_request_fail", "13", status.Error(codes.Internal, "recv failed")},
		{"ContextBeforeRecv", "context_cancelled", "499", context.Canceled},
	} {
		for _, priorResponse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/priorResponse=%t", tt.name, priorResponse), func(t *testing.T) {
				counters := captureCounters(t)
				mc := &MockCache{}
				mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Once()
				s := newProcessTestServer(openShutdownCh(), mc)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				st := &processState{ctx: ctx, model: "test-model", routerCtx: types.NewRoutingContext(ctx, "random", "test-model", "", "", "")}
				st.routerCtx.ReqPath = PathChatCompletions
				defer st.routerCtx.Delete()
				defer st.releaseModelInFlight()
				wantStatus, wantCode := tt.wantStatus, tt.wantCode
				if priorResponse {
					mc.On("HasModel", "test-model").Return(false).Once()
					req := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)},
					}}
					resp, err := s.handleProcessingRequest(st, req)
					require.NoError(t, err)
					require.NotNil(t, resp.GetImmediateResponse())
					wantStatus, wantCode = "gateway_req_body_fail", "400"
				}
				if tt.end == context.Canceled {
					cancel()
					assert.ErrorIs(t, s.preRecvCheck(st), tt.end)
				} else {
					assert.ErrorIs(t, s.handleRecvError(st, tt.end), tt.end)
				}
				require.Len(t, counters(), 1)
				assert.Equal(t, metrics.GatewayRequestModelFailTotal, counters()[0].name)
				assert.Equal(t, wantStatus, counters()[0].labels["status"])
				assert.Equal(t, wantCode, counters()[0].labels["status_code"])
				mc.AssertExpectations(t)
			})
		}
	}
}

func TestProcessOnce_CancelDuringReceive(t *testing.T) {
	counters := captureCounters(t)
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Once()
	s := newProcessTestServer(openShutdownCh(), mc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &mockProcessServer{ctx: ctx}
	// Recv starts after preRecvCheck; cancellation then unblocks the receive.
	srv.On("Recv").Return((*extProcPb.ProcessingRequest)(nil), status.Error(codes.Canceled, "closed")).
		Run(func(mock.Arguments) { cancel() }).Once()
	assert.Equal(t, codes.Canceled, status.Code(s.processOnce(srv, &processState{ctx: ctx, model: "test-model"})))
	require.Len(t, counters(), 1)
	assert.Equal(t, metrics.GatewayRequestModelFailTotal, counters()[0].name)
	assert.Equal(t, "client_cancelled", counters()[0].labels["status"])
	assert.Equal(t, "499", counters()[0].labels["status_code"])
	mc.AssertExpectations(t)
	srv.AssertExpectations(t)
}

func TestSendProcessingResponse_AfterResponseOutcome(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			counters := captureCounters(t)
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"}}
			s := &Server{cache: cache.NewWithPodsForTest([]*v1.Pod{pod}, "test-model")}
			st := &processState{ctx: context.Background(), model: "test-model", isRespError: !completed, respErrorCode: 400,
				routerCtx: types.NewRoutingContext(context.Background(), "random", "test-model", "", "", "")}
			st.routerCtx.ReqPath = PathChatCompletions
			defer st.routerCtx.Delete()
			defer st.releaseModelInFlight()
			req := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), EndOfStream: true},
			}}
			resp, err := s.handleProcessingRequest(st, req)
			require.NoError(t, err)
			require.Nil(t, resp.GetImmediateResponse())
			sendErr := errors.New("transport closed")
			srv := &mockProcessServer{ctx: st.ctx}
			srv.On("Send", resp).Return(sendErr).Once()
			assert.ErrorIs(t, s.sendProcessingResponse(srv, st, resp), sendErr)
			outcomes := requestOutcomeCounters(counters())
			require.Len(t, outcomes, 2)
			wantMetric, wantStatus, wantCode := metrics.GatewayRequestModelFailTotal, "gateway_rsp_body_fail", "400"
			if completed {
				wantMetric, wantStatus, wantCode = metrics.GatewayRequestModelSuccessTotal, "gateway_rsp_body_success", "200"
			}
			assert.Equal(t, wantMetric, outcomes[0].name)
			assert.Equal(t, wantStatus, outcomes[0].labels["status"])
			assert.Equal(t, wantCode, outcomes[0].labels["status_code"])
			assert.Equal(t, metrics.GatewayRequestModelFailTotal, outcomes[1].name)
			assert.Equal(t, "send_envoy_proxy", outcomes[1].labels["status"])
			assert.Equal(t, "499", outcomes[1].labels["status_code"])
			srv.AssertExpectations(t)
		})
	}
}

func TestHandleRecvError_CompletedCanceled(t *testing.T) {
	testHandleRecvErrorCanceled(t, true, metrics.GatewayRequestModelSuccessTotal, "gateway_request_success", "200")
}

func TestHandleRecvError_IncompleteCanceled(t *testing.T) {
	testHandleRecvErrorCanceled(t, false, metrics.GatewayRequestModelFailTotal, "client_cancelled", "499")
}

func testHandleRecvErrorCanceled(t *testing.T, completed bool, wantMetric, wantStatus, wantCode string) {
	t.Helper()
	counters := captureCounters(t)
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Once()
	s := newProcessTestServer(openShutdownCh(), mc)
	st := &processState{model: "test-model", completed: completed}
	for i := 0; i < 2; i++ {
		assert.Equal(t, codes.Canceled, status.Code(s.handleRecvError(st, status.Error(codes.Canceled, "closed"))))
	}
	require.Len(t, counters(), 1)
	assert.Equal(t, wantMetric, counters()[0].name)
	assert.Equal(t, wantStatus, counters()[0].labels["status"])
	assert.Equal(t, wantCode, counters()[0].labels["status_code"])
	mc.AssertExpectations(t)
}
