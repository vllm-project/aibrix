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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestSGLangClientCancelClosesPrefill(t *testing.T) {
	for _, tc := range []struct {
		name            string
		decodeResponded bool
		recycleContext  bool
	}{
		{name: "waiting_for_decode"},
		{name: "decode_responded", decodeResponded: true},
		{name: "routing_context_reused", recycleContext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, disconnected := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				close(started)
				select {
				case <-req.Context().Done():
					close(disconnected)
				case <-release:
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer prefillSrv.Close()
			// Unblock the handler even if a failed assertion exits before cancel
			// reaches it, so server cleanup cannot hang the test binary.
			defer close(release)

			sink, decodeSrv := newAbortSink(t)
			defer decodeSrv.Close()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := failFastCtx("sglang-client-cancel", `{"messages":[{"role":"user","content":"hello"}],"stream":true}`, strings.TrimPrefix(decodeSrv.URL, "http://"))
			defer ctx.Delete()
			ctx.Context = parent
			overrides := *types.DefaultPDOverrides()
			overrides.PrefillRequestTimeout = 300 * time.Second
			overrides.Abort.RetryDelay = 0
			ctx.SetPDOverrides(&overrides)
			pod := failFastPod(t, "prefill-cancel", strings.TrimPrefix(prefillSrv.URL, "http://"))
			podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
			exec := failFastExecutor()
			exec.tracker.AddPrefillRequest(ctx.RequestID, podKey)
			exec.tokenLoad.AcquirePrefill(ctx.RequestID, podKey, 100)
			require.NoError(t, exec.Execute(ctx, pod, engine.Resolve(sglangEngine), LogContext{}))
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("prefill request did not arrive")
			}

			leg := ctx.PDLeg()
			rid := leg.RID()
			if tc.decodeResponded {
				leg.MarkDecodeResponded()
			}
			if tc.recycleContext {
				ctx = types.RecycleRoutingContextForTest(ctx, context.Background(), "pd", "test-model", "", "new-request", "user")
				ctx.SetPDRequestID("new-request-rid")
			}
			cancel()
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Fatal("client cancellation did not close prefill HTTP request despite its 300s timeout")
			}
			select {
			case <-leg.PrefillFailed():
			case <-time.After(2 * time.Second):
				t.Fatal("prefill cancellation was not recorded")
			}
			assert.Equal(t, pd.PrefillFailureCanceled, leg.PrefillFailure().Class)
			select {
			case <-leg.AbortDone():
			case <-time.After(5 * time.Second):
				t.Fatal("decode abort handling did not finish")
			}
			if tc.decodeResponded {
				select {
				case body := <-sink.got:
					t.Fatalf("unexpected decode abort after response: %s", body)
				default:
				}
			} else {
				assert.Equal(t, rid, gjson.Get(sink.wait(t, time.Second), "rid").String())
			}
			require.Eventually(t, func() bool {
				active, _ := exec.tokenLoad.GetLoad(podKey)
				return exec.tracker.GetPrefillRequestCountsForPod(podKey) == 0 && active == 0
			}, 2*time.Second, time.Millisecond)
			assert.True(t, leg.PrefillSucceededAt().IsZero())
			if tc.recycleContext {
				assert.Nil(t, ctx.PrefillFailure())
				assert.Equal(t, "new-request-rid", ctx.PDRequestID())
				assert.NoError(t, ctx.Err())
			}
		})
	}
}
