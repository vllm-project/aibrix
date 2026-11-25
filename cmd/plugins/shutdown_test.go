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

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type shutdownListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *shutdownListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestShutdownGateway(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "finishes active stream"
		if force {
			name = "bounds a blocked stream"
		}
		t.Run(name, func(t *testing.T) {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"},
				Status: v1.PodStatus{PodIP: "127.0.0.1", Conditions: []v1.PodCondition{
					{Type: v1.PodReady, Status: v1.ConditionTrue},
				}},
			}
			store := cache.NewWithPodsForTest([]*v1.Pod{pod}, "test-model")
			gw := gateway.NewServerWithOptions(nil, nil, nil, gateway.ServerOptions{Cache: store})
			t.Cleanup(gw.Shutdown)
			httpListener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			httpAddr := httpListener.Addr().String()
			require.NoError(t, httpListener.Close())
			require.NoError(t, gw.StartHTTPServer(httpAddr))
			httpClient := &http.Client{
				Transport: &http.Transport{DisableKeepAlives: true},
				Timeout:   time.Second,
			}
			checkHTTP := func() {
				t.Helper()
				resp, err := httpClient.Get("http://" + httpAddr + "/v1/models")
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, http.StatusOK, resp.StatusCode)
			}
			checkHTTP()
			server := grpc.NewServer()
			t.Cleanup(server.Stop)
			extProcPb.RegisterExternalProcessorServer(server, gw)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			listener := &shutdownListener{Listener: ln, closed: make(chan struct{})}
			go func() { _ = server.Serve(listener) }()
			conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := extProcPb.NewExternalProcessorClient(conn).Process(ctx)
			require.NoError(t, err)
			exchange := func(req *extProcPb.ProcessingRequest) {
				t.Helper()
				require.NoError(t, stream.Send(req))
				resp, err := stream.Recv()
				require.NoError(t, err)
				require.Nil(t, resp.GetImmediateResponse())
			}
			exchange(&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":path", RawValue: []byte("/v1/chat/completions")},
					{Key: gateway.HeaderRoutingStrategy, RawValue: []byte("random")},
				}}},
			}})
			exchange(&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{
					Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
				},
			}})
			exchange(&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{Body: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")},
			}})

			timeout := 2 * time.Second
			if force {
				timeout = 50 * time.Millisecond
			}
			done := make(chan struct{})
			healthCheck := health.NewServer()
			healthCheck.SetServingStatus("gateway-plugin", healthPb.HealthCheckResponse_SERVING)
			started := time.Now()
			go func() {
				shutdownGateway(server, gw, healthCheck, timeout)
				close(done)
			}()
			select {
			case <-listener.closed:
			case <-ctx.Done():
				t.Fatal("shutdown did not stop accepting connections")
			}
			if !force {
				checkHTTP()
				exchange(&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{
					ResponseBody: &extProcPb.HttpBody{Body: []byte("data: [DONE]\n\n"), EndOfStream: true},
				}})
				_, err = stream.Recv()
				require.ErrorIs(t, err, io.EOF, "the existing stream must finish normally during shutdown")
			} else {
				_, err = stream.Recv()
				require.Error(t, err)
				require.NotErrorIs(t, err, io.EOF)
				require.GreaterOrEqual(t, time.Since(started), timeout, "shutdown must wait for its drain budget")
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("shutdown did not finish")
			}
			status, err := healthCheck.Check(ctx, &healthPb.HealthCheckRequest{Service: "gateway-plugin"})
			require.NoError(t, err)
			require.Equal(t, healthPb.HealthCheckResponse_NOT_SERVING, status.GetStatus())
			resp, err := httpClient.Get("http://" + httpAddr + "/v1/models")
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err, "gateway HTTP listener must close before shutdown returns")
		})
	}
}
