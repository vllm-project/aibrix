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

package prefill

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestExecuteHTTPSkipsEnvoyPseudoHeaders(t *testing.T) {
	var gotMethod, gotAuth, gotStrategy string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Header.Get(":method")
		gotAuth = r.Header.Get("Authorization")
		gotStrategy = r.Header.Get("routing-strategy")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	exec := NewDefaultExecutor(srv.Client(), pd.NewPrefillRequestTracker()).(*DefaultExecutor)
	got, err := exec.executeHTTP(srv.URL, &types.RoutingContext{
		Context:   context.Background(),
		RequestID: "req-1",
		ReqHeaders: map[string]string{
			":method":          "POST",
			"authorization":    "Bearer test",
			"routing-strategy": "pd",
		},
	}, []byte(`{"model":"m"}`), 5*time.Second)
	require.NoError(t, err)
	assert.True(t, gjson.GetBytes(got, "ok").Bool())
	assert.Empty(t, gotMethod, "HTTP/2 pseudo-headers must not be forwarded")
	assert.Equal(t, "Bearer test", gotAuth)
	assert.Equal(t, "pd", gotStrategy)
}

func TestExecuteHTTPClassifiesCallFailures(t *testing.T) {
	newContext := func() *types.RoutingContext {
		return &types.RoutingContext{Context: context.Background(), RequestID: "req-1"}
	}

	t.Run("connection failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		exec := NewDefaultExecutor(srv.Client(), pd.NewPrefillRequestTracker()).(*DefaultExecutor)
		_, err := exec.executeHTTP(url, newContext(), []byte(`{"model":"m"}`), 5*time.Second)

		var transportErr *pd.PrefillTransportError
		require.ErrorAs(t, err, &transportErr)
	})

	t.Run("response body read failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Flush the headers first so the client fails while reading the body,
			// not while waiting for the response.
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, `{"partial":`)
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()

		exec := NewDefaultExecutor(srv.Client(), pd.NewPrefillRequestTracker()).(*DefaultExecutor)
		_, err := exec.executeHTTP(srv.URL, newContext(), []byte(`{"model":"m"}`), 5*time.Second)

		var transportErr *pd.PrefillTransportError
		require.ErrorAs(t, err, &transportErr)
		assert.Contains(t, err.Error(), "failed to read prefill response body")
	})
}

func TestResponsePromptTokens(t *testing.T) {
	assert.Equal(t, 42.0, responsePromptTokens([]byte(`{"usage":{"prompt_tokens":42}}`)))
	assert.Equal(t, 29.0, responsePromptTokens([]byte(`{"usage":{"input_tokens":29}}`)),
		"vLLM Responses reports usage.input_tokens")
	assert.Equal(t, 11.0, responsePromptTokens([]byte(`{"usage":{"prompt_tokens":11,"input_tokens":22}}`)))
	assert.Equal(t, 22.0, responsePromptTokens([]byte(`{"usage":{"prompt_tokens":0,"input_tokens":22}}`)))
	assert.Equal(t, 0.0, responsePromptTokens([]byte(`{"usage":{"prompt_tokens":"11"}}`)))
}
