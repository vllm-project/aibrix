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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Opt in with AIBRIX_ENVOY_TEST_IMAGE=envoyproxy/envoy:v1.33.2. Only the Envoy
// process runs in Docker; upstream HTTP and the real Gateway Process run here.
// Host networking connects the three loopback listeners; no Kubernetes is used.
func TestEnvoyStreamedUpstreamErrors(t *testing.T) {
	image := os.Getenv("AIBRIX_ENVOY_TEST_IMAGE")
	if image == "" {
		t.Skip("set AIBRIX_ENVOY_TEST_IMAGE to run against real Envoy")
	}
	processor := &envoyObservedProcessor{}
	upstream := httptest.NewServer(http.HandlerFunc(processor.serveUpstream))
	t.Cleanup(upstream.Close)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "envoy-test", Namespace: "default"},
		Status: corev1.PodStatus{PodIP: "127.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	processor.gateway = NewServerWithOptions(nil, nil, nil, ServerOptions{
		Cache: cache.NewWithPodsForTest([]*corev1.Pod{pod}, "test-model"), DisableRateLimiting: true,
	})
	t.Cleanup(processor.gateway.Shutdown)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(grpcServer, processor)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	endpoint := startTestEnvoy(t, image, listener.Addr().(*net.TCPAddr).Port, upstream.Listener.Addr().(*net.TCPAddr).Port)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	const nested = `{"error":{"message":"invalid parameter","type":"invalid_request_error","param":"top_p","code":400}}`
	for _, tc := range []struct {
		name          string
		code          int
		parts         []string
		contentType   string
		contentLength bool
		want          string
	}{
		{"nested_split", 400, []string{nested[:25], nested[25:]}, "application/json", false, `"invalid parameter"`},
		{"body_then_empty_end", 400, []string{nested}, "application/json", false, `"invalid parameter"`},
		{"flat_with_content_length", 400, []string{`{"message":"invalid `, `parameter","type":"invalid_request_error"}`}, "application/json", true, `"invalid parameter"`},
		{"plain_text", 400, []string{"invalid ", "parameter"}, "text/plain", false, `"invalid parameter"`},
		{"empty_body", 400, nil, "text/plain", false, `""`},
		{"headers_only", 400, nil, "text/plain", true, `""`},
		{"rate_limit", 429, []string{`{"error":{"message":"rate `, `limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`}, "application/json", true, `"rate limited"`},
		{"unavailable", 503, []string{"backend ", "unavailable"}, "text/plain", false, `"backend unavailable"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := newEnvoyExchange(tc.code, tc.parts, tc.contentType, tc.contentLength)
			processor.current.Store(ex)
			resp, err := client.Do(envoyChatRequest(t, endpoint, false))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, readErr := io.ReadAll(resp.Body)
			t.Logf("status=%d read_error=%v body=%s callbacks=%v immediate_responses=%d", resp.StatusCode, readErr, body, ex.bodyCallbacks(), ex.immediate.Load())
			require.NoError(t, readErr, "client must not see a reset or truncated response")
			require.Equal(t, tc.code, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
			require.JSONEq(t, fmt.Sprintf(`{"error":{"message":%s}}`, tc.want), envoyErrorMessage(t, body))
			if tc.name == "headers_only" {
				assert.EqualValues(t, 1, ex.immediate.Load(), "a local reply is safe while headers have not been released")
				assert.Empty(t, ex.bodyCallbacks(), "Content-Length: 0 must be handled without a body callback")
			} else {
				assert.Zero(t, ex.immediate.Load(), "streamed errors must use body mutations")
				assert.Equal(t, "true", resp.Header.Get(HeaderErrorResponseUnknown), "documented error marker must reach streamed clients")
			}
			if tc.name == "nested_split" {
				require.GreaterOrEqual(t, len(ex.bodyCallbacks()), 2)
			}
			if tc.name == "body_then_empty_end" {
				callbacks := ex.bodyCallbacks()
				require.GreaterOrEqual(t, len(callbacks), 2)
				assert.Equal(t, "0/end=true", callbacks[len(callbacks)-1])
			}
		})
	}
	t.Run("SSE_first_chunk_before_upstream_end", func(t *testing.T) {
		ex := newEnvoyExchange(200, nil, "text/event-stream", false)
		ex.sse = true
		processor.current.Store(ex)
		resp, err := client.Do(envoyChatRequest(t, endpoint, true))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		reader := bufio.NewReader(resp.Body)
		first, err := reader.ReadString('\n')
		require.NoError(t, err, "first SSE event must arrive before upstream is allowed to finish")
		require.Contains(t, first, `"hello"`)
		close(ex.finishSSE)
		rest, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(rest), "data: [DONE]")
		assert.Zero(t, ex.immediate.Load())
		t.Logf("first SSE event arrived before upstream completion; callbacks=%v", ex.bodyCallbacks())
	})
}

func envoyErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &parsed))
	result, err := json.Marshal(parsed)
	require.NoError(t, err)
	return string(result)
}

func envoyChatRequest(t *testing.T, endpoint string, stream bool) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"model":"test-model","messages":[{"role":"user","content":"envoy regression"}],"max_tokens":8,"stream":%t}`, stream)
	req, err := http.NewRequest(http.MethodPost, endpoint+PathChatCompletions, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderRoutingStrategy, "random")
	return req
}

type envoyExchange struct {
	code          int
	parts         []string
	contentType   string
	contentLength bool
	sse           bool
	headersSent   chan struct{}
	headerOnce    sync.Once
	bodySeen      chan struct{}
	finishSSE     chan struct{}
	immediate     atomic.Int64
	mu            sync.Mutex
	callbacks     []string
}

func newEnvoyExchange(code int, parts []string, typ string, length bool) *envoyExchange {
	return &envoyExchange{code: code, parts: parts, contentType: typ, contentLength: length,
		headersSent: make(chan struct{}), bodySeen: make(chan struct{}, 16), finishSSE: make(chan struct{})}
}
func (ex *envoyExchange) bodyCallbacks() []string {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return append([]string(nil), ex.callbacks...)
}

type envoyObservedProcessor struct {
	extProcPb.UnimplementedExternalProcessorServer
	gateway *Server
	current atomic.Pointer[envoyExchange]
}

func (p *envoyObservedProcessor) Process(stream extProcPb.ExternalProcessor_ProcessServer) error {
	return p.gateway.Process(&envoyObservedStream{ExternalProcessor_ProcessServer: stream, exchange: p.current.Load()})
}
func (p *envoyObservedProcessor) serveUpstream(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	ex := p.current.Load()
	if ex == nil {
		http.Error(w, "no test exchange", 500)
		return
	}
	w.Header().Set("Content-Type", ex.contentType)
	if ex.contentLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(strings.Join(ex.parts, ""))))
	}
	w.WriteHeader(ex.code)
	w.(http.Flusher).Flush()
	if ex.contentLength && len(strings.Join(ex.parts, "")) == 0 {
		return // An upstream ending on headers has no body/acknowledgement to wait for.
	}
	select {
	case <-ex.headersSent:
	case <-r.Context().Done():
		return
	}
	if ex.sse {
		_, _ = io.WriteString(w, "data: {\"id\":\"envoy-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-ex.finishSSE:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	for _, part := range ex.parts {
		_, _ = io.WriteString(w, part)
		w.(http.Flusher).Flush()
		// Wait until real Envoy has sent this piece to ext_proc before writing
		// the next one. TCP coalescing cannot hide the multi-message regression.
		select {
		case <-ex.bodySeen:
		case <-r.Context().Done():
			return
		}
	}
}

type envoyObservedStream struct {
	extProcPb.ExternalProcessor_ProcessServer
	exchange *envoyExchange
}

func (s *envoyObservedStream) Recv() (*extProcPb.ProcessingRequest, error) {
	req, err := s.ExternalProcessor_ProcessServer.Recv()
	if err == nil && req.GetResponseBody() != nil {
		b := req.GetResponseBody()
		s.exchange.mu.Lock()
		s.exchange.callbacks = append(s.exchange.callbacks, fmt.Sprintf("%d/end=%t", len(b.GetBody()), b.GetEndOfStream()))
		s.exchange.mu.Unlock()
		if len(b.GetBody()) > 0 {
			s.exchange.bodySeen <- struct{}{}
		}
	}
	return req, err
}
func (s *envoyObservedStream) Send(resp *extProcPb.ProcessingResponse) error {
	err := s.ExternalProcessor_ProcessServer.Send(resp)
	if resp.GetImmediateResponse() != nil {
		s.exchange.immediate.Add(1)
	}
	if err == nil && resp.GetResponseHeaders() != nil {
		s.exchange.headerOnce.Do(func() { close(s.exchange.headersSent) })
	}
	return err
}

func envoyDockerCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", append([]string{"--host=unix:///var/run/docker.sock"}, args...)...)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	return cmd
}
func reserveEnvoyPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}
func startTestEnvoy(t *testing.T, image string, extProcPort, upstreamPort int) string {
	t.Helper()
	port, admin := reserveEnvoyPort(t), reserveEnvoyPort(t)
	config := fmt.Sprintf(`admin:
  address:
    socket_address: {address: 127.0.0.1, port_value: %d}
static_resources:
  listeners:
  - name: http
    address:
      socket_address: {address: 127.0.0.1, port_value: %d}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: regression
          route_config:
            name: local
            virtual_hosts:
            - name: upstream
              domains: ["*"]
              routes:
              - match: {prefix: /}
                route: {cluster: upstream, timeout: 10s}
          http_filters:
          - name: envoy.filters.http.ext_proc
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
              grpc_service:
                envoy_grpc: {cluster_name: ext_proc}
              failure_mode_allow: false
              message_timeout: 2s
              processing_mode:
                request_header_mode: SEND
                response_header_mode: SEND
                request_body_mode: BUFFERED
                response_body_mode: STREAMED
                request_trailer_mode: SKIP
                response_trailer_mode: SKIP
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: ext_proc
    type: STATIC
    connect_timeout: 1s
    http2_protocol_options: {}
    load_assignment:
      cluster_name: ext_proc
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: {address: 127.0.0.1, port_value: %d}
  - name: upstream
    type: STATIC
    connect_timeout: 1s
    load_assignment:
      cluster_name: upstream
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: {address: 127.0.0.1, port_value: %d}
`, admin, port, extProcPort, upstreamPort)
	path := filepath.Join(t.TempDir(), "envoy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))
	// The container reads this non-secret configuration as a different UID.
	require.NoError(t, os.Chmod(path, 0644))
	name := fmt.Sprintf("aibrix-envoy-3372-%d", port)
	output, err := envoyDockerCommand("run", "--detach", "--network=host", "--name", name,
		"--mount", "type=bind,src="+path+",dst=/tmp/envoy.yaml,readonly", image,
		"-c", "/tmp/envoy.yaml", "--concurrency", "1", "--log-level", "warning").CombinedOutput()
	require.NoError(t, err, "docker run: %s", output)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := envoyDockerCommand("logs", name).CombinedOutput()
			t.Logf("Envoy logs: %s", logs)
		}
		_ = envoyDockerCommand("rm", "--force", name).Run()
	})
	readyClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer readyClient.CloseIdleConnections()
	require.Eventually(t, func() bool {
		r, err := readyClient.Get(fmt.Sprintf("http://127.0.0.1:%d/ready", admin))
		if err != nil {
			return false
		}
		defer func() { _ = r.Body.Close() }()
		return r.StatusCode == http.StatusOK
	}, 20*time.Second, 100*time.Millisecond, "Envoy did not become ready")
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}
