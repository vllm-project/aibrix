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

package routingalgorithms

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

func trtRoutePod(t *testing.T, role string, srv *httptest.Server) *v1.Pod {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	pod := burstPod(role+"-trt", role, host)
	pod.Labels[constants.ModelLabelPort] = port
	pod.Labels[LLMEngineIdentifier] = TensorRTLLM
	return pod
}

func newTRTGenerationFirstRouter(t *testing.T, client *http.Client) *pdRouter {
	t.Helper()
	r, _ := newTokenLoadTestRouter(t, client)
	h, err := engine.NewTRTLLMHandler(engine.TRTGenerationFirst, engine.NewTRTServerInfoCache(client))
	require.NoError(t, err)
	r.trtHandler = h
	r.prefillExecutor = prefill.NewDefaultExecutor(client, r.prefillRequestTracker,
		prefill.WithTokenLoadTracker(r.tokenLoadTracker), prefill.WithEngineHandler(h))
	return r
}

func TestTRTGenerationFirstRouteOverlapsPrefillAndDecode(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions?test=1"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				counter, cleanup := metrics.SetupCounterMetricsForTest(metrics.GatewayPrefillRequestSuccessTotal,
					[]string{"gateway_pod", "model", "status", "status_code"})
				defer cleanup()
				prefillBody := make(chan []byte, 1)
				decodeBody := make(chan []byte, 1)
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				var infoCalls atomic.Int32
				pSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/server_info" {
						infoCalls.Add(1)
						_, _ = io.WriteString(w, `{"disaggregated_params":{"ctx_info_endpoint":"tcp://ctx:5555","ctx_dp_rank":3}}`)
						return
					}
					b, _ := io.ReadAll(req.Body)
					prefillBody <- b
					select {
					case <-release:
						_, _ = io.WriteString(w, `{"prompt_token_ids":[999],"disaggregated_params":{"stale":true}}`)
					case <-req.Context().Done():
					}
				}))
				defer pSrv.Close()
				defer unblock()
				response := `{"choices":[{"text":"hello"}]}`
				if stream {
					response = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
				}
				var decodeCalls atomic.Int32
				dSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					decodeCalls.Add(1)
					b, _ := io.ReadAll(req.Body)
					decodeBody <- b
					_, _ = io.WriteString(w, response)
				}))
				defer dSrv.Close()
				p, d := trtRoutePod(t, "prefill", pSrv), trtRoutePod(t, "decode", dSrv)
				r := newTRTGenerationFirstRouter(t, pSrv.Client())
				requestCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ctx := types.NewRoutingContext(requestCtx, RouterPD, "trt-parallel", "hello", "parallel-request", "user")
				defer ctx.Delete()
				ctx.Engine, ctx.ReqPath = TensorRTLLM, path
				input := `"messages":[{"role":"user","content":"hello"}]`
				if strings.HasPrefix(path, "/v1/completions") {
					input = `"prompt":"hello"`
				}
				ctx.ReqBody = []byte(fmt.Sprintf(`{%s,"stream":%t,"max_tokens":37}`, input, stream))
				addr, err := r.Route(ctx, &utils.PodArray{Pods: []*v1.Pod{p, d}})
				require.NoError(t, err, "Route must not wait for the blocked prefill response")
				assert.Equal(t, strings.TrimPrefix(dSrv.URL, "http://"), addr)
				assert.EqualValues(t, 1, r.prefillRequestTracker.GetPrefillRequestCountsForPod(p.Name))
				assert.Equal(t, 0.0, testutil.ToFloat64(counter.WithLabelValues("", ctx.Model, pdRoutePrefillRequestSuccess, "200")))
				active, _ := r.tokenLoadTracker.GetLoad(p.Name)
				assert.Greater(t, active, 0.0)
				select {
				case b := <-prefillBody:
					assert.Equal(t, "context_only", gjson.GetBytes(b, "disaggregated_params.request_type").String())
					assert.Equal(t, "1", gjson.GetBytes(b, "max_tokens").Raw)
					assert.Equal(t, "false", gjson.GetBytes(b, "stream").Raw)
					assert.Equal(t, "1", gjson.GetBytes(b, "disaggregated_params.schedule_style").Raw)
					assert.Equal(t, gjson.GetBytes(ctx.ReqBody, "disaggregated_params.disagg_request_id").Raw,
						gjson.GetBytes(b, "disaggregated_params.disagg_request_id").Raw)
				case <-requestCtx.Done():
					t.Fatal("prefill never arrived")
				}
				// Emulate Envoy's forwarding, not a second POST from the router.
				forwarded := append([]byte(nil), ctx.ReqBody...)
				req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://"+addr+path, bytes.NewReader(forwarded))
				require.NoError(t, err)
				resp, err := dSrv.Client().Do(req)
				require.NoError(t, err)
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.NoError(t, err)
				assert.Equal(t, response, string(got))
				b := <-decodeBody
				assert.Equal(t, forwarded, b)
				assert.Equal(t, "generation_only", gjson.GetBytes(b, "disaggregated_params.request_type").String())
				assert.Equal(t, "3", gjson.GetBytes(b, "disaggregated_params.ctx_dp_rank").Raw)
				assert.Equal(t, "37", gjson.GetBytes(b, "max_tokens").Raw)
				assert.Equal(t, stream, gjson.GetBytes(b, "stream").Bool())
				unblock()
				require.Eventually(t, func() bool { return r.prefillRequestTracker.GetPrefillRequestCountsForPod(p.Name) == 0 }, 5*time.Second, time.Millisecond)
				assert.Equal(t, forwarded, ctx.ReqBody, "late prefill response must not overwrite the decode request")
				assert.Equal(t, 1.0, testutil.ToFloat64(counter.WithLabelValues("", ctx.Model, pdRoutePrefillRequestSuccess, "200")))
				active, _ = r.tokenLoadTracker.GetLoad(p.Name)
				assert.Zero(t, active)
				assert.EqualValues(t, 1, infoCalls.Load())
				// The router dispatched exactly one request: the context leg. The
				// generation leg above was this test's own Envoy emulation, so a
				// second call here would mean the router posted it a second time.
				assert.EqualValues(t, 1, decodeCalls.Load(), "the router must not POST the decode leg itself")
			})
		}
	}
}

func TestTRTGenerationFirstMissingMetadataFailsBeforeDispatch(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = io.WriteString(w, `{"disaggregated_params":{"ctx_info_endpoint":"tcp://ctx:5555"}}`)
	}))
	defer srv.Close()
	r := newTRTGenerationFirstRouter(t, srv.Client())
	p, d := trtRoutePod(t, "prefill", srv), burstPod("decode", "decode", "127.0.0.2")
	ctx := tokenLoadRequest(t, "missing-info", 400)
	defer ctx.Delete()
	ctx.Engine = TensorRTLLM
	original := string(ctx.ReqBody)
	_, err := r.Route(ctx, &utils.PodArray{Pods: []*v1.Pod{p, d}})
	require.ErrorContains(t, err, "ctx_dp_rank")
	assert.Zero(t, posts.Load())
	assert.Equal(t, original, string(ctx.ReqBody))
	assert.Zero(t, r.prefillRequestTracker.GetPrefillRequestCountsForPod(p.Name))
	assert.Zero(t, r.pendingDecodeTracker.GetPendingDecodeCount(d.Name))
	active, kv := r.tokenLoadTracker.GetLoad(p.Name)
	assert.Zero(t, active)
	assert.Zero(t, kv)
}

// An unrecognized AIBRIX_TRT_SCHEDULE_STYLE degrades to context_first with an
// error log, following the other env-driven knobs. Construction must not fail:
// the router manager would then register a nil provider for "pd" and every pd
// request would hit a recovered panic instead of a working context-first route.
func TestPDRouterFallsBackOnInvalidTRTScheduleStyle(t *testing.T) {
	t.Setenv("AIBRIX_TRT_SCHEDULE_STYLE", "typo")
	r, err := NewPDRouterWithCacheAndPrefixIndexer(cache.NewForTest(), nil)
	require.NoError(t, err)
	router, ok := r.(*pdRouter)
	require.True(t, ok)
	require.NotNil(t, router.trtHandler)
	assert.False(t, router.trtHandler.IsAsync(), "a typo must leave the default context-first dispatch")
}
