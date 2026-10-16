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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// The router charges the decode pod an estimate from the request body's size
// (60,000 tokens here, as for a request with a ~240 KB inline image). Once the
// prefill response reports usage.prompt_tokens, the charge becomes that count.
func TestExecuteCorrectsDecodeChargeFromPrefillUsage(t *testing.T) {
	const (
		decodeKey = "default/decode-1"
		estimate  = 60_000.0
	)
	cases := []struct {
		name   string
		engine string
		status int
		body   string
		want   float64
	}{
		{name: "vllm sync prefill reports usage", engine: "vllm", status: http.StatusOK,
			body: `{"choices":[],"usage":{"prompt_tokens":1052,"completion_tokens":1,"total_tokens":1053}}`, want: 1052},
		{name: "sglang async prefill reports usage", engine: sglangEngine, status: http.StatusOK,
			body: `{"choices":[],"usage":{"prompt_tokens":1052,"completion_tokens":1,"total_tokens":1053}}`, want: 1052},
		{name: "response without usage keeps the estimate", engine: "vllm", status: http.StatusOK,
			body: `{"choices":[]}`, want: estimate},
		{name: "non-numeric prompt_tokens keeps the estimate", engine: "vllm", status: http.StatusOK,
			body: `{"usage":{"prompt_tokens":"1052"}}`, want: estimate},
		{name: "zero prompt_tokens keeps the estimate", engine: "vllm", status: http.StatusOK,
			body: `{"usage":{"prompt_tokens":0}}`, want: estimate},
		{name: "failed prefill keeps the estimate", engine: "vllm", status: http.StatusInternalServerError,
			body: `{"error":"boom","usage":{"prompt_tokens":1052}}`, want: estimate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer prefillSrv.Close()
			_, decodeSrv := newAbortSink(t)
			defer decodeSrv.Close()

			exec := failFastExecutor()
			ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message",
				"req-"+strings.ReplaceAll(tc.name, " ", "-"), "user")
			ctx.ReqPath = "/v1/chat/completions"
			ctx.Engine = tc.engine
			ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
			ctx.SetDecodeTarget(strings.TrimPrefix(decodeSrv.URL, "http://"), "decode-1")
			exec.tokenLoad.AcquireDecodeWithTTL(ctx.RequestID, decodeKey, estimate, 0)

			prefillPod := failFastPod(t, "prefill-1", strings.TrimPrefix(prefillSrv.URL, "http://"))
			_ = exec.Execute(ctx, prefillPod, engine.Resolve(tc.engine), LogContext{})
			// The prefill call is done (sync, or the async goroutine) once the
			// prefill request count is back to 0.
			require.Eventually(t, func() bool {
				return exec.tracker.GetPrefillRequestCountsForPod(utils.GeneratePodKey(prefillPod.Namespace, prefillPod.Name)) == 0
			}, 2*time.Second, 10*time.Millisecond)
			assert.Equal(t, tc.want, exec.tokenLoad.GetDecodeLoad(decodeKey))
		})
	}
}

// Without a decode charge (another decode policy), a prefill response with
// usage changes nothing.
func TestExecuteWithoutDecodeChargeIgnoresUsage(t *testing.T) {
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1052}}`))
	}))
	defer prefillSrv.Close()

	exec := failFastExecutor()
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message", "req-no-charge", "user")
	ctx.ReqPath = "/v1/chat/completions"
	ctx.Engine = "vllm"
	ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	prefillPod := failFastPod(t, "prefill-1", strings.TrimPrefix(prefillSrv.URL, "http://"))
	require.NoError(t, exec.Execute(ctx, prefillPod, engine.Resolve("vllm"), LogContext{}))
	assert.Zero(t, exec.tokenLoad.GetDecodeLoad("default/decode-1"))
	tokens, charges, _ := exec.tokenLoad.DecodeLedgerState("default/decode-1")
	assert.Zero(t, tokens)
	assert.Zero(t, charges)
}
