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

package gateway

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

func TestStreamPanicRecoveryInterceptorFailsOnlyThePanickingStream(t *testing.T) {
	interceptor := StreamPanicRecoveryInterceptor()
	info := &grpc.StreamServerInfo{FullMethod: ProcessFullMethod}

	err := interceptor(nil, nil, info, func(any, grpc.ServerStream) error {
		panic("index out of range [3] with length 3")
	})

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "ExternalProcessor/Process")
	assert.NotContains(t, err.Error(), "index out of range", "the panic value belongs in the log, not in the reply")
}

func TestStreamPanicRecoveryInterceptorKeepsHandlerErrors(t *testing.T) {
	interceptor := StreamPanicRecoveryInterceptor()
	handlerErr := errors.New("client cancelled the stream")

	err := interceptor(nil, nil, &grpc.StreamServerInfo{FullMethod: ProcessFullMethod}, func(any, grpc.ServerStream) error {
		return handlerErr
	})

	assert.ErrorIs(t, err, handlerErr, "a handler error is not a panic and must pass through unchanged")
}

func TestRecoverStreamPanicCountsThePanic(t *testing.T) {
	counter, restore := metrics.SetupCounterMetricsForTest(metrics.GatewayRequestPanicTotal, []string{"pod_name"})
	defer restore()

	require.Error(t, recoverStreamPanic("boom", ProcessFullMethod))

	assert.Equal(t, 1.0, testutil.ToFloat64(counter.WithLabelValues(podName)))
}

// panickingContextStream stands in for a stream that is already broken: asking
// it for its context panics, so the panic happens inside Process.
type panickingContextStream struct {
	extProcPb.ExternalProcessor_ProcessServer
}

func (panickingContextStream) Context() context.Context {
	panic("stream context is gone")
}

// TestProcessRecoversPanicsRaisedOutsideTheInterceptor covers the fallback: the
// server stream interceptor is not the only way into Process, so a panic raised
// while Process runs has to come back as an internal status instead of taking
// the process down.
func TestProcessRecoversPanicsRaisedOutsideTheInterceptor(t *testing.T) {
	err := (&Server{}).Process(panickingContextStream{})

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// panickingExtProcServer is an ExtProc service whose only handler panics. It
// drives a real gRPC server through the recovery interceptor.
type panickingExtProcServer struct {
	extProcPb.UnimplementedExternalProcessorServer
	calls atomic.Int64
}

func (s *panickingExtProcServer) Process(extProcPb.ExternalProcessor_ProcessServer) error {
	s.calls.Add(1)
	panic("boom in Process")
}

// TestPanicInOneStreamDoesNotStopTheServer is the invariant this change exists
// for: a panic on the request path fails the stream it happened on, and the
// server keeps accepting new streams.
func TestPanicInOneStreamDoesNotStopTheServer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	impl := &panickingExtProcServer{}
	server := grpc.NewServer(grpc.StreamInterceptor(StreamPanicRecoveryInterceptor()))
	extProcPb.RegisterExternalProcessorServer(server, impl)
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	client := extProcPb.NewExternalProcessorClient(conn)

	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stream, err := client.Process(ctx)
		require.NoError(t, err, "attempt %d: opening a stream must still work", attempt)

		_, err = stream.Recv()
		cancel()

		require.Error(t, err, "attempt %d: the panicking stream must fail", attempt)
		assert.Equal(t, codes.Internal, status.Code(err), "attempt %d", attempt)
	}

	assert.Equal(t, int64(2), impl.calls.Load(), "the server must keep serving streams after a panic")
}
