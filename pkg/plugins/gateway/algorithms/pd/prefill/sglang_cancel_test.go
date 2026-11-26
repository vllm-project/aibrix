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
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

// responseObservedTransport lets the test end the stream after HTTP headers
// arrived, while the executor is still waiting for the prefill response body.
type responseObservedTransport struct {
	http.RoundTripper
	responseReceived chan struct{}
}

func (t responseObservedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err == nil {
		close(t.responseReceived)
	}
	return resp, err
}

func TestSGLangStreamEndClosesPrefill(t *testing.T) {
	for _, tc := range []struct {
		name            string
		decodeResponded bool
		recycleContext  bool
		bodyPending     bool
	}{
		{name: "waiting_for_decode"},
		{name: "decode_completed_before_prefill_headers", decodeResponded: true},
		{name: "decode_completed_during_prefill_body", decodeResponded: true, bodyPending: true},
		{name: "routing_context_reused", recycleContext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := klog.CaptureState()
			defer state.Restore()
			flags := flag.NewFlagSet("stream-end", flag.ContinueOnError)
			klog.InitFlags(flags)
			require.NoError(t, flags.Set("v", "4"))
			var logs bytes.Buffer
			klog.LogToStderr(false)
			klog.SetOutput(&logs)

			var successes, failures atomic.Int32
			original := metrics.IncrementCounterMetricFnForTest
			defer func() { metrics.IncrementCounterMetricFnForTest = original }()
			metrics.IncrementCounterMetricFnForTest = func(name, _ string, _ float64, _ []string, _ ...string) {
				switch name {
				case metrics.GatewayPrefillRequestSuccessTotal:
					successes.Add(1)
				case metrics.GatewayPrefillRequestFailTotal:
					failures.Add(1)
				}
			}

			started, disconnected := make(chan struct{}), make(chan struct{})
			responseReceived := make(chan struct{})
			release := make(chan struct{})
			prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				if tc.bodyPending {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"usage":`)
					w.(http.Flusher).Flush()
				}
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
			if tc.bodyPending {
				exec.httpClient.Transport = responseObservedTransport{http.DefaultTransport, responseReceived}
			}
			exec.tracker.AddPrefillRequest(ctx.TrackerKey(), podKey)
			exec.tokenLoad.AcquirePrefill(ctx.TrackerKey(), podKey, 100)
			require.NoError(t, exec.Execute(ctx, pod, engine.Resolve(sglangEngine), LogContext{}))
			defer func(leg *types.PDLegState) {
				cancel()
				waitForDecodeAbort(t, leg)
			}(ctx.PDLeg())
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("prefill request did not arrive")
			}
			if tc.bodyPending {
				select {
				case <-responseReceived:
				case <-time.After(2 * time.Second):
					t.Fatal("prefill response headers did not arrive")
				}
			}

			leg := ctx.PDLeg()
			rid := leg.RID()
			if tc.decodeResponded {
				// Model a normal ext_proc finish: decode has responded, and
				// returning from the stream handler cancels its context below.
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
			case <-leg.AbortDone():
			case <-time.After(2 * time.Second):
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
			klog.Flush()
			if tc.decodeResponded {
				assert.Nil(t, leg.PrefillFailure())
				select {
				case <-leg.PrefillFailed():
					t.Fatal("stream completion must not signal a prefill failure")
				default:
				}
				assert.Zero(t, failures.Load())
				assert.NotContains(t, logs.String(), "prefill_request_failed")
				assert.Contains(t, logs.String(), "prefill_request_outcome_unobserved")
			} else {
				require.NotNil(t, leg.PrefillFailure())
				assert.Equal(t, pd.PrefillFailureCanceled, leg.PrefillFailure().Class)
				assert.EqualValues(t, 1, failures.Load())
				assert.Contains(t, logs.String(), "prefill_request_failed")
			}
			assert.Zero(t, successes.Load(), "an unobserved prefill response is not a success")
			assert.True(t, leg.PrefillSucceededAt().IsZero())
			if tc.recycleContext {
				assert.Nil(t, ctx.PrefillFailure())
				assert.Equal(t, "new-request-rid", ctx.PDRequestID())
				assert.NoError(t, ctx.Err())
			}
		})
	}
}
