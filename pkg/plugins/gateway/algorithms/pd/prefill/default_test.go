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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	exec := NewDefaultExecutor(srv.Client(), pd.NewPrefillRequestTracker(), 5).(*DefaultExecutor)
	got, err := exec.executeHTTP(srv.URL, &types.RoutingContext{
		Context:   context.Background(),
		RequestID: "req-1",
		ReqHeaders: map[string]string{
			":method":          "POST",
			"authorization":    "Bearer test",
			"routing-strategy": "pd",
		},
	}, []byte(`{"model":"m"}`))
	require.NoError(t, err)
	assert.Equal(t, true, got["ok"])
	assert.Empty(t, gotMethod, "HTTP/2 pseudo-headers must not be forwarded")
	assert.Equal(t, "Bearer test", gotAuth)
	assert.Equal(t, "pd", gotStrategy)
}
