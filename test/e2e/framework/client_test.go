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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
