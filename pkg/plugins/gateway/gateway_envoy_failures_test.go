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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This protects the distinction between backend HTTP errors and Envoy local
// replies used by request-outcome attribution on the pinned Envoy v1.39.6.
// Earlier versions can bypass ext_proc for these local replies. It does not test the blacklist
// implementation from a separate PR. No local-reply runtime override is set.
func TestEnvoyGeneratedFailureCallbacks(t *testing.T) {
	image := os.Getenv("AIBRIX_ENVOY_TEST_IMAGE")
	if image == "" && os.Getenv("AIBRIX_ENVOY_TEST_BINARY") == "" {
		t.Skip("set AIBRIX_ENVOY_TEST_IMAGE or AIBRIX_ENVOY_TEST_BINARY to run against real Envoy")
	}
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"connect_refused", 503}, {"pre_header_reset", 503}, {"timeout", 504}, {"backend_503_control", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			processor := &envoyObservedProcessor{}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "envoy-failure-test", Namespace: "default"}, Status: corev1.PodStatus{PodIP: "127.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			processor.gateway = NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: cache.NewWithPodsForTest([]*corev1.Pod{pod}, "test-model"), DisableRateLimiting: true})
			t.Cleanup(processor.gateway.Shutdown)
			ex := newEnvoyExchange(503, []string{"backend unavailable"}, "text/plain", false)
			processor.current.Store(ex)
			upstreamPort := reserveEnvoyPort(t)
			if tc.name != "connect_refused" {
				releaseUpstream := make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch tc.name {
					case "pre_header_reset":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							return
						}
						if tcp, ok := conn.(*net.TCPConn); ok {
							_ = tcp.SetLinger(0)
						}
						_ = conn.Close()
					case "timeout":
						select {
						case <-r.Context().Done():
						case <-releaseUpstream:
						}
					default:
						processor.serveUpstream(w, r)
					}
				}))
				t.Cleanup(upstream.Close)
				t.Cleanup(func() { close(releaseUpstream) })
				upstreamPort = upstream.Listener.Addr().(*net.TCPAddr).Port
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			extProcPb.RegisterExternalProcessorServer(server, processor)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			endpoint := startTestEnvoy(t, image, listener.Addr().(*net.TCPAddr).Port, upstreamPort, "1.39.6")
			client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
			t.Cleanup(client.CloseIdleConnections)
			resp, err := client.Do(envoyChatRequest(t, endpoint, false))
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.StatusCode)
			require.EqualValues(t, 1, ex.requestHeaders.Load(), "request must traverse ext_proc")
			t.Logf("status=%d request_headers=%d response_headers=%d response_body_callbacks=%v body=%q", resp.StatusCode, ex.requestHeaders.Load(), ex.responseHeaders.Load(), ex.bodyCallbacks(), body)
			require.EqualValues(t, 1, ex.responseHeaders.Load())
			require.NotEmpty(t, ex.bodyCallbacks())
		})
	}
}
