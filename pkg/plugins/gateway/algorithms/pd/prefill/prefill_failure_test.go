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

// Tests for what the fire-and-forget prefill leg owes the rest of the gateway
// when it fails: a classified failure recorded on the request's PD leg, and an
// abort aimed at the decode pod that would otherwise wait out its bootstrap
// timeout.

package prefill

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const sglangEngine = "sglang"

// failFastPod builds a pod whose model port points at addr ("ip:port"), so the
// executor's apiURL hits the given httptest server.
func failFastPod(t *testing.T, name, addr string) *v1.Pod {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{constants.ModelLabelPort: port},
		},
		Status: v1.PodStatus{PodIP: host},
	}
}

// failFastCtx builds the routing context as pdRouter.Route leaves it just
// before doPrefillRequest: an SGLang PD request whose decode leg is already
// pinned to decodeAddr.
func failFastCtx(requestID, body, decodeAddr string) *types.RoutingContext {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message", requestID, "user")
	ctx.ReqPath = "/v1/chat/completions"
	ctx.Engine = sglangEngine
	ctx.ReqBody = []byte(body)
	ctx.SetDecodeTarget(decodeAddr, "decode-1")
	return ctx
}

func failFastExecutor() *DefaultExecutor {
	return NewDefaultExecutor(&http.Client{}, pd.NewPrefillRequestTracker(),
		WithTokenLoadTracker(pd.NewTokenLoadTracker())).(*DefaultExecutor)
}

// abortSink is an httptest handler standing in for a decode pod: it records
// the /abort_request bodies it receives.
type abortSink struct{ got chan string }

func newAbortSink(t *testing.T) (*abortSink, *httptest.Server) {
	t.Helper()
	sink := &abortSink{got: make(chan string, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sink.got <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	return sink, srv
}

func (a *abortSink) wait(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case body := <-a.got:
		return body
	case <-time.After(d):
		t.Fatalf("no abort request received within %s", d)
		return ""
	}
}

func (a *abortSink) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case body := <-a.got:
		t.Fatalf("unexpected abort request: %s", body)
	case <-time.After(d):
	}
}

// failingPrefillServer answers every prefill POST with the given status.
func failingPrefillServer(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"prefill failed"}`))
	}))
}

func TestAsyncPrefillFailureRecordsFailureAndAbortsDecodeLeg(t *testing.T) {
	prefillSrv := failingPrefillServer(http.StatusInternalServerError)
	defer prefillSrv.Close()
	sink, decodeSrv := newAbortSink(t)
	defer decodeSrv.Close()

	decodeAddr := strings.TrimPrefix(decodeSrv.URL, "http://")
	exec := failFastExecutor()
	ctx := failFastCtx("req-abort", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, decodeAddr)
	prefillPod := failFastPod(t, "prefill-1", strings.TrimPrefix(prefillSrv.URL, "http://"))

	// The async leg is fire-and-forget: Execute returns before it finishes.
	require.NoError(t, exec.Execute(ctx, prefillPod, sglangEngine, LogContext{}))

	body := sink.wait(t, 5*time.Second)
	assert.Equal(t, ctx.PDRequestID(), gjson.Get(body, "rid").String(),
		"the abort must carry the rid injected into both legs")

	assert.Eventually(t, func() bool { return ctx.PrefillFailure() != nil }, 2*time.Second, 10*time.Millisecond)
	failure := ctx.PrefillFailure()
	require.NotNil(t, failure)
	assert.Equal(t, pd.PrefillFailureHTTPStatus, failure.Class)
	assert.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	assert.False(t, failure.At.IsZero())

	// The wakeup edge the gateway's stream goroutine parks on.
	select {
	case <-ctx.PrefillFailed():
	case <-time.After(2 * time.Second):
		t.Fatal("a failed prefill leg never woke the fail-fast path")
	}

	// The failure path still releases the router's ledgers.
	assert.Eventually(t, func() bool {
		return exec.tracker.GetPrefillRequestCountsForPod("prefill-1") == 0
	}, 2*time.Second, 10*time.Millisecond)
}

// A 200 with an unparseable body means the KV transfer completed: the failure
// is recorded, but the decode leg must be left alone.
func TestAsyncPrefillBadResponseDoesNotAbortDecodeLeg(t *testing.T) {
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer prefillSrv.Close()
	sink, decodeSrv := newAbortSink(t)
	defer decodeSrv.Close()

	exec := failFastExecutor()
	ctx := failFastCtx("req-badbody", `{"messages":[{"role":"user","content":"hi"}]}`, strings.TrimPrefix(decodeSrv.URL, "http://"))
	prefillPod := failFastPod(t, "prefill-1", strings.TrimPrefix(prefillSrv.URL, "http://"))

	require.NoError(t, exec.Execute(ctx, prefillPod, sglangEngine, LogContext{}))

	assert.Eventually(t, func() bool { return ctx.PrefillFailure() != nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, pd.PrefillFailureBadResponse, ctx.PrefillFailure().Class)
	sink.none(t, 300*time.Millisecond)
}

// A transport failure - the prefill pod is gone - classifies as such and is
// still terminal for the client request.
func TestAsyncPrefillTransportFailureIsTerminal(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close() // nothing is listening on deadAddr any more

	sink, decodeSrv := newAbortSink(t)
	defer decodeSrv.Close()

	exec := failFastExecutor()
	ctx := failFastCtx("req-dead", `{"messages":[{"role":"user","content":"hi"}]}`, strings.TrimPrefix(decodeSrv.URL, "http://"))

	require.NoError(t, exec.Execute(ctx, failFastPod(t, "prefill-1", deadAddr), sglangEngine, LogContext{}))

	assert.Eventually(t, func() bool { return ctx.PrefillFailure() != nil }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, pd.PrefillFailureTransport, ctx.PrefillFailure().Class)
	assert.True(t, pd.PrefillFailureIsTerminal(ctx.PrefillFailure().Class))
	assert.Equal(t, ctx.PDRequestID(), gjson.Get(sink.wait(t, 5*time.Second), "rid").String())
}

// TestPrefillGoroutineFailureAfterContextReuseDoesNotTouchNewRequest is the
// regression test for the pooled-context race. The SGLang prefill goroutine
// routinely outlives the client stream - its HTTP call hangs off the request
// context, so a client cancel fails the prefill leg at the very moment the
// stream goroutine releases the RoutingContext back to requestPool - and by the
// time it reports, the object may already belong to a different request.
//
// Without the per-incarnation types.PDLegState, the stale report would win the
// *new* request's prefill-failure CAS (reset() had just cleared it), close its
// fail-fast wakeup channel and aim the decode abort at the new request's rid,
// i.e. kill a healthy request.
func TestPrefillGoroutineFailureAfterContextReuseDoesNotTouchNewRequest(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePrefill := func() { releaseOnce.Do(func() { close(release) }) }
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"prefill failed"}`))
	}))
	defer prefillSrv.Close()
	// Registered after the Close above, therefore run before it: defers are
	// LIFO. Any require.* failure below calls FailNow, and the handler must be
	// unblocked by then or Close would wait on the in-flight request forever -
	// a hung test binary instead of a reported failure.
	defer releasePrefill()

	sink, decodeSrv := newAbortSink(t)
	defer decodeSrv.Close()

	exec := failFastExecutor()
	ctx1 := failFastCtx("req-old", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, strings.TrimPrefix(decodeSrv.URL, "http://"))
	prefillPod := failFastPod(t, "prefill-1", strings.TrimPrefix(prefillSrv.URL, "http://"))

	require.NoError(t, exec.Execute(ctx1, prefillPod, sglangEngine, LogContext{}))

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the prefill request never reached the prefill pod")
	}

	oldRID := ctx1.PDRequestID()
	require.NotEmpty(t, oldRID)
	leg1 := ctx1.PDLeg()
	require.NotNil(t, leg1)

	// The client went away: the stream released the routing context, and the
	// next request took the very same object back. Going through sync.Pool for
	// that would be flaky - under -race Put drops about one object in four - so
	// the recycle is done deterministically on this exact pointer, running the
	// same reset the pool path runs.
	ctx2 := types.RecycleRoutingContextForTest(ctx1, context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message", "req-new", "user")
	require.Same(t, ctx1, ctx2, "the recycle seam must hand back the same object; the aliasing is the point of this test")
	ctx2.SetPDRequestID("rid-of-the-new-request")
	require.NotSame(t, leg1, ctx2.PDLeg(), "the recycled context must carry a brand-new PD leg")

	// Only now does the prefill leg of the *old* request fail.
	releasePrefill()

	assert.Equal(t, oldRID, gjson.Get(sink.wait(t, 5*time.Second), "rid").String(),
		"the abort must carry the rid of the request that actually failed")

	assert.Eventually(t, func() bool { return leg1.PrefillFailure() != nil }, 2*time.Second, 10*time.Millisecond,
		"the failure must still be recorded on the retired leg")
	assert.Nil(t, ctx2.PrefillFailure(), "a stale prefill failure was recorded against the new request")
	select {
	case <-ctx2.PrefillFailed():
		t.Fatal("a stale prefill failure woke the fail-fast path of the new request")
	default:
	}
	assert.False(t, ctx2.DecodeResponded())
	assert.Equal(t, "rid-of-the-new-request", ctx2.PDRequestID(), "the stale report must not touch the live rid")
	addr, _ := ctx2.DecodeTarget()
	assert.Empty(t, addr, "reset must hand the new request a clean leg")
}
