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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

type fixedTRTServerInfo struct{}

func (fixedTRTServerInfo) Get(context.Context, *v1.Pod) (engine.TRTServerInfo, error) {
	return engine.TRTServerInfo{ContextInfoEndpoint: "tcp://ctx:5555", ContextDPRank: 1}, nil
}

func trtAsyncExecutor(t *testing.T) *DefaultExecutor {
	t.Helper()
	h, err := engine.NewTRTLLMHandler(engine.TRTGenerationFirst, fixedTRTServerInfo{})
	require.NoError(t, err)
	// The prefill deadline now comes from the request's resolved PD overrides;
	// TestMain installs the 30s process default these subtests inherit.
	return NewDefaultExecutor(&http.Client{}, pd.NewPrefillRequestTracker(),
		WithEngineHandler(h), WithTokenLoadTracker(pd.NewTokenLoadTrackerWithConfig(pd.TokenLoadConfig{TTL: 0}))).(*DefaultExecutor)
}

func TestTRTAsyncPrefillFailures(t *testing.T) {
	for _, class := range []string{pd.PrefillFailureHTTPStatus, pd.PrefillFailureTransport,
		pd.PrefillFailureTimeout, pd.PrefillFailureCanceled, pd.PrefillFailureBadResponse} {
		t.Run(class, func(t *testing.T) {
			started, disconnected := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				close(started)
				switch class {
				case pd.PrefillFailureHTTPStatus:
					w.WriteHeader(http.StatusInternalServerError)
				case pd.PrefillFailureBadResponse:
					_, _ = io.WriteString(w, "not json")
				default:
					<-req.Context().Done()
					close(disconnected)
				}
			}))
			defer srv.Close()
			pod := failFastPod(t, "trt-ctx", strings.TrimPrefix(srv.URL, "http://"))
			if class == pd.PrefillFailureTransport {
				srv.Close()
			}
			var aborts atomic.Int32
			decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				aborts.Add(1)
			}))
			defer decodeSrv.Close()
			exec := trtAsyncExecutor(t)
			ctx := failFastCtx("trt-failure", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, strings.TrimPrefix(decodeSrv.URL, "http://"))
			defer ctx.Delete()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx.Context, ctx.Engine = parent, pd.EngineTRTLLM
			if class == pd.PrefillFailureTimeout {
				// Deadlines are per request now, so the timeout case asks for one
				// instead of inheriting the 30s process default.
				overrides := *types.DefaultPDOverrides()
				overrides.PrefillRequestTimeout = 200 * time.Millisecond
				ctx.SetPDOverrides(&overrides)
			}
			// Even an unrelated rid must never trigger SGLang's abort endpoint.
			ctx.SetPDRequestID("not-a-trt-abort-id")
			exec.tracker.AddPrefillRequest(ctx.RequestID, pod.Name)
			exec.tokenLoad.AcquirePrefill(ctx.RequestID, pod.Name, 100)
			require.NoError(t, exec.Execute(ctx, pod, pd.EngineTRTLLM, LogContext{}))
			if class == pd.PrefillFailureCanceled {
				select {
				case <-started:
					cancel()
				case <-time.After(5 * time.Second):
					t.Fatal("prefill did not start")
				}
			}
			select {
			case <-ctx.PrefillFailed():
			case <-time.After(5 * time.Second):
				t.Fatal("prefill failure did not wake the gateway")
			}
			assert.Equal(t, class, ctx.PrefillFailure().Class)
			select {
			case <-ctx.PDLeg().AbortDone():
			case <-time.After(5 * time.Second):
				t.Fatal("failure handling did not finish")
			}
			require.Eventually(t, func() bool { return exec.tracker.GetPrefillRequestCountsForPod(pod.Name) == 0 }, 5*time.Second, time.Millisecond)
			assert.Zero(t, aborts.Load())
			active, _ := exec.tokenLoad.GetLoad(pod.Name)
			assert.Zero(t, active)
			if class == pd.PrefillFailureCanceled || class == pd.PrefillFailureTimeout {
				select {
				case <-disconnected:
				case <-time.After(5 * time.Second):
					t.Fatal("prefill HTTP connection was not canceled")
				}
			}
		})
	}
}

func TestTRTAsyncFailureAfterContextReuse(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		close(started)
		select {
		case <-release:
			w.WriteHeader(http.StatusInternalServerError)
		case <-req.Context().Done():
		}
	}))
	defer srv.Close()
	defer unblock()
	exec := trtAsyncExecutor(t)
	ctx := failFastCtx("old-trt-request", `{"prompt":"hi"}`, "127.0.0.1:1")
	ctx.Engine = pd.EngineTRTLLM
	pod := failFastPod(t, "ctx", strings.TrimPrefix(srv.URL, "http://"))
	exec.tracker.AddPrefillRequest(ctx.RequestID, pod.Name)
	require.NoError(t, exec.Execute(ctx, pod, pd.EngineTRTLLM, LogContext{}))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("prefill did not start")
	}
	oldLeg := ctx.PDLeg()
	reused := types.RecycleRoutingContextForTest(ctx, context.Background(), "pd", "model", "hello", "new-request", "user")
	defer reused.Delete()
	reused.ReqBody = []byte(`{"prompt":"new"}`)
	unblock()
	select {
	case <-oldLeg.PrefillFailed():
	case <-time.After(5 * time.Second):
		t.Fatal("old leg did not record failure")
	}
	require.Eventually(t, func() bool { return exec.tracker.GetPrefillRequestCountsForPod(pod.Name) == 0 }, 5*time.Second, time.Millisecond)
	assert.Nil(t, reused.PrefillFailure())
	assert.Equal(t, `{"prompt":"new"}`, string(reused.ReqBody))
	select {
	case <-reused.PrefillFailed():
		t.Fatal("old failure woke the new request")
	default:
	}
}
