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

package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

type bodyRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
	status int
	reply  string
}

func (b *bodyRecorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, body)
		b.mu.Unlock()
		w.WriteHeader(b.status)
		_, _ = io.WriteString(w, b.reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (b *bodyRecorder) got() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]byte(nil), b.bodies...)
}

func epdPod(t *testing.T, name, roleset, role string, srv *httptest.Server) *v1.Pod {
	pod := trtRoutePod(t, name, srv)
	pod.Name = name
	pod.Labels[PDRoleSetIdentifier] = roleset
	pod.Labels[PDRoleIdentifier] = role
	pod.Labels[LLMEngineIdentifier] = VLLMEngine
	return pod
}

func TestRouteEncodesMediaOnTheRolesetEncoders(t *testing.T) {
	image1 := `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`
	image2 := `{"type":"image_url","image_url":{"url":"https://x/b.png"}}`
	withImages := `{"model":"qwen-vl","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"compare"},` +
		image1 + `,` + image2 + `]}]}`

	tests := []struct {
		name         string
		body         string
		encodeStatus int
		wantEncodes  int
		wantResult   string
	}{
		{name: "each media item goes to an encoder", body: withImages, encodeStatus: http.StatusOK, wantEncodes: 2, wantResult: "success"},
		{name: "text-only requests skip the encoders", body: `{"model":"qwen-vl","messages":[{"role":"user","content":"hi"}]}`,
			encodeStatus: http.StatusOK},
		{name: "a failed encode does not fail the request", body: withImages, encodeStatus: http.StatusInternalServerError,
			wantEncodes: 2, wantResult: "http_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter, cleanup := metrics.SetupCounterMetricsForTest(metrics.GatewayPDEncodeRequestTotal,
				[]string{"gateway_pod", "model", "status", "status_code"})
			defer cleanup()

			encoders := &bodyRecorder{status: tt.encodeStatus, reply: `{"choices":[]}`}
			otherEncoders := &bodyRecorder{status: http.StatusOK}
			prefills := &bodyRecorder{status: http.StatusOK, reply: `{"kv_transfer_params":{"remote_host":"127.0.0.1"}}`}
			decodes := &bodyRecorder{status: http.StatusOK}
			encodeSrv, otherSrv := encoders.server(t), otherEncoders.server(t)
			prefillSrv, decodeSrv := prefills.server(t), decodes.server(t)

			pods := []*v1.Pod{
				epdPod(t, "encode-0", "rs-a", PDRoleEncode, encodeSrv),
				epdPod(t, "encode-1", "rs-a", PDRoleEncode, encodeSrv),
				epdPod(t, "prefill-0", "rs-a", PDRolePrefill, prefillSrv),
				epdPod(t, "decode-0", "rs-a", PDRoleDecode, decodeSrv),
				// Encoders of a roleset without prefill and decode pods.
				epdPod(t, "encode-other", "rs-b", PDRoleEncode, otherSrv),
			}
			r, _ := newTokenLoadTestRouter(t, prefillSrv.Client())
			r.encodeExecutor = encode.NewExecutor(prefillSrv.Client())

			requestCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx := types.NewRoutingContext(requestCtx, RouterPD, "qwen-vl", "", "epd-request", "user")
			defer ctx.Delete()
			ctx.Engine, ctx.ReqPath = VLLMEngine, testChatCompletionsPath
			ctx.ReqBody = []byte(tt.body)

			addr, err := r.Route(ctx, &utils.PodArray{Pods: pods})
			require.NoError(t, err)
			assert.Equal(t, ctx.PodAddress(pods[3]), addr)

			got := encoders.got()
			require.Len(t, got, tt.wantEncodes)
			var sent []string
			for _, b := range got {
				sent = append(sent, gjson.GetBytes(b, "messages.0.content.0").Raw)
				assert.Equal(t, int64(1), gjson.GetBytes(b, "messages.0.content.#").Int())
			}
			if tt.wantEncodes > 0 {
				assert.ElementsMatch(t, []string{image1, image2}, sent)
				assert.Equal(t, float64(tt.wantEncodes),
					testutil.ToFloat64(counter.WithLabelValues("", "qwen-vl", tt.wantResult, map[string]string{"success": "200", "http_error": "500"}[tt.wantResult])))
			}
			assert.Empty(t, otherEncoders.got(), "encoders of another roleset must not be used")

			// Prefill still gets the media itself: vLLM finds the encoder output by its content.
			require.Len(t, prefills.got(), 1)
			assert.Equal(t, gjson.Get(tt.body, "messages").Raw, gjson.GetBytes(prefills.got()[0], "messages").Raw)
		})
	}
}

func TestRouteEncodesMediaForACombinedPod(t *testing.T) {
	encoders := &bodyRecorder{status: http.StatusOK}
	combined := &bodyRecorder{status: http.StatusOK}
	encodeSrv, combinedSrv := encoders.server(t), combined.server(t)
	pods := []*v1.Pod{
		epdPod(t, "encode-0", "rs-a", PDRoleEncode, encodeSrv),
		epdPod(t, "combined-0", "rs-a", "combined", combinedSrv),
	}
	r, _ := newTokenLoadTestRouter(t, encodeSrv.Client())
	r.encodeExecutor = encode.NewExecutor(encodeSrv.Client())
	// Bucketing picked the combined pod, so there is no prefill leg.
	r.podSelector = selector.NewDefaultSelector(func(*types.RoutingContext, []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
		return nil, pods[1], nil
	})

	requestCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx := types.NewRoutingContext(requestCtx, RouterPD, "qwen-vl", "", "combined-request", "user")
	defer ctx.Delete()
	ctx.Engine, ctx.ReqPath = VLLMEngine, testChatCompletionsPath
	ctx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)

	addr, err := r.Route(ctx, &utils.PodArray{Pods: pods})
	require.NoError(t, err)
	assert.Equal(t, ctx.PodAddress(pods[1]), addr)
	assert.Len(t, encoders.got(), 1)
	assert.Empty(t, combined.got(), "the gateway forwards the request to the combined pod, the router doesn't call it")
}
