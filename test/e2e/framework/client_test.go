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

package e2eframework

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestNewRequestIDIsNonEmptyAndUnique(t *testing.T) {
	require.NotEmpty(t, NewRequestID(""))

	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		requestID := NewRequestID("pd")
		require.NotEmpty(t, requestID)
		require.True(t, strings.HasPrefix(requestID, "pd-"))
		_, duplicate := seen[requestID]
		require.False(t, duplicate, "duplicate request ID %q", requestID)
		seen[requestID] = struct{}{}
	}
}

func TestSendPDRequestSendsRawRequestAndReturnsResponse(t *testing.T) {
	body := []byte(`{"model":"model","messages":[{"role":"user","content":"hello"}]}`)
	handlerErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var failures []string
		check := func(name, want, got string) {
			if want != got {
				failures = append(failures, fmt.Sprintf("%s: want %q, got %q", name, want, got))
			}
		}
		check("path", "/v1/chat/completions", r.URL.Path)
		check("method", http.MethodPost, r.Method)
		check("authorization", "Bearer test-key", r.Header.Get("Authorization"))
		check("content type", "application/json", r.Header.Get("Content-Type"))
		check("routing strategy", "pd", r.Header.Get("routing-strategy"))
		check("request ID", "request-1", r.Header.Get("x-request-id"))
		got, err := io.ReadAll(r.Body)
		if err != nil {
			failures = append(failures, fmt.Sprintf("reading request body: %v", err))
		} else if !bytes.Equal(body, got) {
			failures = append(failures, fmt.Sprintf("request body changed: got %q", got))
		}
		if len(failures) > 0 {
			handlerErr <- fmt.Errorf("handler observations: %s", strings.Join(failures, "; "))
		} else {
			handlerErr <- nil
		}
		w.Header().Set("x-gateway", "decode-pod")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"response-1"}`))
	}))
	defer server.Close()

	result, err := SendPDRequest(
		context.Background(),
		Config{GatewayURL: server.URL + "/", APIKey: "test-key"},
		"pd",
		"request-1",
		body,
	)

	require.NoError(t, <-handlerErr)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, result.StatusCode)
	require.Equal(t, "decode-pod", result.Headers.Get("x-gateway"))
	require.Equal(t, []byte(`{"id":"response-1"}`), result.Body)
	require.Equal(t, "request-1", result.RequestID)
}

func TestSendPDRequestReturnsResponseDetailsOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-gateway-error", "prefill")
		http.Error(w, "prefill rejected", http.StatusBadRequest)
	}))
	defer server.Close()

	result, err := SendPDRequest(
		context.Background(),
		Config{GatewayURL: server.URL, APIKey: "test-key"},
		"pd",
		"request-2",
		[]byte(`{}`),
	)

	require.Error(t, err)
	require.ErrorContains(t, err, "400")
	require.ErrorContains(t, err, "prefill rejected")
	require.Equal(t, http.StatusBadRequest, result.StatusCode)
	require.Equal(t, "prefill", result.Headers.Get("x-gateway-error"))
	require.Equal(t, "request-2", result.RequestID)
	require.Equal(t, "prefill rejected\n", string(result.Body))
}

func TestSendPDRequestWithHeadersPreservesFailureInjectionHeaders(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("x-aibrix-mock-fail")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"prefill handoff failed"}}`))
	}))
	defer server.Close()

	result, err := SendPDRequestWithHeaders(
		context.Background(),
		Config{GatewayURL: server.URL, APIKey: "test-key"},
		"pd",
		"request-3",
		[]byte(`{"model":"m"}`),
		http.Header{"x-aibrix-mock-fail": []string{"prefill"}},
	)

	require.Error(t, err)
	require.Equal(t, "prefill", <-seen)
	require.Equal(t, http.StatusInternalServerError, result.StatusCode)
}

func TestStableAdapterInference(t *testing.T) {
	const routeError = `{"error":{"code":"service_unavailable","message":"route is pending"}}`
	const externalError = `{"error":{"code":"external_router_unavailable"}}`
	const wrongAdapter = `{"model":"a","choices":[{"message":{"content":"ok"}}]}`
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		routingHeader bool
		wantErr       string
	}{
		{"gateway route pending", http.StatusServiceUnavailable, routeError, true, ""},
		{"upstream unavailable", http.StatusServiceUnavailable, routeError, false, "503"},
		{"other routing error", http.StatusServiceUnavailable, externalError, true, "503"},
		{"unexpected gateway error", http.StatusInternalServerError, "upstream failed", true, "upstream failed"},
		{"empty gateway error", http.StatusInternalServerError, "", false, "500"},
		{"wrong adapter", http.StatusOK, wrongAdapter, false, `returned model "a"`},
		{"empty choices", http.StatusOK, `{"model":"b","choices":[]}`, false, "returned no choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			failedProbe := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				wantModel := "a"
				if n%2 == 0 {
					wantModel = "b"
				}
				var req struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != wantModel {
					t.Errorf("request %d: got model %q, want %q (decode error %v)", n, req.Model, wantModel, err)
				}
				w.Header().Set("Content-Type", "application/json")
				if n == 4 { // B's second probe interrupts one previously successful round.
					failedProbe <- r.UserAgent()
					if tc.routingHeader {
						w.Header().Set("x-error-routing", "true")
					}
					w.Header().Set("request-id", "plugin-id")
					w.Header().Set("x-request-id", "envoy-id")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_, _ = fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"content":"ok"}}]}`, req.Model)
			}))
			defer server.Close()
			check := stableAdapterInferenceCondition(t, NewOpenAIClient(server.URL, "test-key"), "a", "b")
			ctx := context.Background()
			ready, err := check(ctx)
			require.NoError(t, err)
			require.False(t, ready)
			ready, err = check(ctx)
			require.False(t, ready)
			require.Equal(t, int32(4), calls.Load())
			probe := <-failedProbe
			require.True(t, strings.HasPrefix(probe, "aibrix-e2e/"))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.ErrorContains(t, err, probe)
				var probeErr *inferenceProbeError
				require.ErrorAs(t, err, &probeErr)
				require.Equal(t, probe, probeErr.probe)
				if tc.status >= 400 {
					require.ErrorContains(t, err, "plugin-id")
					require.ErrorContains(t, err, "envoy-id")
				} else {
					require.EqualError(t, err, fmt.Sprintf("inference for b (%s): %s", probe, tc.wantErr))
				}
				return
			}
			require.NoError(t, err)
			for _, want := range []bool{false, false, true} {
				ready, err = check(ctx)
				require.NoError(t, err)
				require.Equal(t, want, ready)
			}
		})
	}
}

func TestFindProbeAccessLog(t *testing.T) {
	const probe = "aibrix-e2e/x"
	const match = `{"user-agent":"aibrix-e2e/x","response_code":500,"response_code_details":"direct_response",` +
		`"response_flags":"-","route_name":"adapter","upstream_cluster":"-"}`
	for _, tc := range []struct {
		name string
		logs string
		want string
	}{
		{"exact match", match, match},
		{"prefix is not a match", `{"user-agent":"aibrix-e2e/x-other"}`, ""},
		{"probe in another field", `{"user-agent":"other","path":"aibrix-e2e/x"}`, ""},
		{"non-JSON lines", "aibrix-e2e/x\ninvalid JSON\n" + match, match},
		{"multiple access logs", `{"user-agent":"other"}` + "\n" + match + "\n" + `{"user-agent":"later"}`, match},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, found := findProbeAccessLog([]byte(tc.logs), probe)
			require.Equal(t, tc.want != "", found)
			require.Equal(t, tc.want, line)
		})
	}
}

func TestWaitForEnvoyProbeLog(t *testing.T) {
	const probe = "aibrix-e2e/x"
	const match = `{"user-agent":"aibrix-e2e/x","response_code":500}`
	for _, tc := range []struct {
		name     string
		appears  bool
		logError bool
	}{
		{"delayed access log", true, false},
		{"missing access log", false, false},
		{"log read error before list error", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists, reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/pods":
					const selector = "gateway.envoyproxy.io/owning-gateway-namespace=gateway-test," +
						"gateway.envoyproxy.io/owning-gateway-name=aibrix-eg"
					if got := r.URL.Query().Get("labelSelector"); got != selector {
						t.Errorf("pod selector: got %q, want %q", got, selector)
					}
					n := lists.Add(1)
					if tc.logError && n > 3 {
						http.Error(w, "pod list access denied", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if n == 1 {
						_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
						return
					}
					_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[`+
						`{"metadata":{"namespace":"envoy-system","name":"proxy"}}]}`)
				case "/api/v1/namespaces/envoy-system/pods/proxy/log":
					for key, want := range map[string]string{"container": "envoy", "sinceSeconds": "300", "tailLines": "1000"} {
						if got := r.URL.Query().Get(key); got != want {
							t.Errorf("log option %s: got %q, want %q", key, got, want)
						}
					}
					n := reads.Add(1)
					if tc.logError {
						http.Error(w, "log access denied", http.StatusForbidden)
						return
					}
					if tc.appears && n >= 2 {
						_, _ = io.WriteString(w, match)
						return
					}
					_, _ = io.WriteString(w, `{"user-agent":"aibrix-e2e/x-other"}`)
				default:
					t.Errorf("unexpected request path %q", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
			require.NoError(t, err)
			timeout := 500 * time.Millisecond
			if tc.appears {
				timeout = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			start := time.Now()
			pod, line, err := waitForEnvoyProbeLog(ctx, client, "gateway-test", probe, 5*time.Millisecond)
			if tc.appears {
				require.NoError(t, err)
				require.Equal(t, "envoy-system/proxy", pod)
				require.Equal(t, match, line)
				require.GreaterOrEqual(t, reads.Load(), int32(2))
				require.GreaterOrEqual(t, lists.Load(), int32(3))
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Empty(t, pod)
				require.Empty(t, line)
				require.Less(t, time.Since(start), 5*time.Second)
				if tc.logError {
					require.ErrorContains(t, err, "log access denied")
					require.ErrorContains(t, err, "pod list access denied")
					require.ErrorContains(t, err, "envoy-system/proxy")
				}
			}
		})
	}
}

func TestLoadKubernetesConfig(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "kubeconfig")
	const contents = `apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://api.example.invalid
users:
- name: test-user
  user:
    token: fixture-token
contexts:
- name: test-context
  context:
    cluster: test-cluster
    user: test-user
current-context: test-context
`
	require.NoError(t, os.WriteFile(configFile, []byte(contents), 0o600))
	config, err := loadKubernetesConfig(configFile)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.invalid", config.Host)
	require.Equal(t, "fixture-token", config.BearerToken)
	require.Equal(t, float32(e2eClientQPS), config.QPS)
	require.Equal(t, e2eClientBurst, config.Burst)

	config, err = loadKubernetesConfig("")
	require.ErrorContains(t, err, "kubeConfig not set")
	require.Nil(t, config)
	config, err = loadKubernetesConfig(configFile + ".missing")
	require.ErrorContains(t, err, "error building Kubernetes client configuration")
	require.Nil(t, config)
}
