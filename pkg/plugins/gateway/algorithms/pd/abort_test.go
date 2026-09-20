/*
Copyright 2024 The Aibrix Team.

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

// Tests for the decode-side half of PD fail-fast: classifying a prefill leg
// failure and aborting the decode leg that will never receive its KV cache.

package pd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// abortRecorder is an httptest handler standing in for a decode pod: it
// records /abort_request bodies and can block to simulate a wedged pod.
type abortRecorder struct {
	got     chan string
	release chan struct{}
	// cancelled receives once for every parked handler whose client went away
	// before releasing it. It is how a test observes that the gateway cut its
	// own abort short instead of holding the connection for the full timeout.
	cancelled chan struct{}
	// releaseOnce makes releaseHandlers idempotent so a test can register it as
	// a defer *and* call it explicitly, without a double-close panic.
	releaseOnce sync.Once
}

func newAbortRecorder() *abortRecorder {
	return &abortRecorder{got: make(chan string, 8), cancelled: make(chan struct{}, 8)}
}

func (a *abortRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sglangAbortRequestPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		a.got <- string(body)
		if a.release != nil {
			select {
			case <-a.release:
			case <-r.Context().Done():
				a.cancelled <- struct{}{}
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// releaseHandlers unblocks every handler parked on release. It is idempotent,
// so a test registers it as a defer right after the server's deferred Close -
// defers run last-in-first-out, so this one runs *before* Close - and may also
// call it explicitly at the point the handler should proceed. Without it a
// require.* failure would FailNow with the handler still parked and the
// deferred Close would wait for that in-flight request forever, hanging the
// whole test binary instead of reporting the failure.
func (a *abortRecorder) releaseHandlers() {
	if a.release == nil {
		return
	}
	a.releaseOnce.Do(func() { close(a.release) })
}

// none fails if another abort arrives within d.
func (a *abortRecorder) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case body := <-a.got:
		t.Fatalf("unexpected extra abort request: %s", body)
	case <-time.After(d):
	}
}

func (a *abortRecorder) wait(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case body := <-a.got:
		return body
	case <-time.After(d):
		t.Fatalf("no abort request received within %s", d)
		return ""
	}
}

// abortTestLeg builds a PD leg state as the request path leaves it: a
// gateway-owned rid and the decode pod the abort must be aimed at.
func abortTestLeg(t *testing.T, requestID, rid, decodeAddr string) *types.PDLegState {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message", requestID, "user")
	leg := ctx.PDLeg()
	require.NotNil(t, leg)
	leg.SetRID(rid)
	leg.SetDecodeTarget(decodeAddr, "decode-1")
	return leg
}

// withAbortRetryDelay pins the delay between the two abort attempts for one
// test (0 = single attempt) and restores the configured value afterwards.
func withAbortRetryDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	restore := decodeAbortRetryDelayNanos.Swap(int64(delay))
	t.Cleanup(func() { decodeAbortRetryDelayNanos.Store(restore) })
}

// waitAbortDone joins the fire-and-forget abort goroutine. Without it the
// goroutine can still be emitting its log line and counter sample while the
// test returns and its fixtures are torn down.
func waitAbortDone(t *testing.T, leg *types.PDLegState) {
	t.Helper()
	select {
	case <-leg.AbortDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the decode abort goroutine did not finish")
	}
}

// withAbortTimeout pins AIBRIX_DECODE_ABORT_TIMEOUT for one test.
func withAbortTimeout(t *testing.T, seconds int64) {
	t.Helper()
	restore := decodeAbortTimeoutSeconds.Swap(seconds)
	t.Cleanup(func() { decodeAbortTimeoutSeconds.Store(restore) })
}

// ---------------------------------------------------------------------------
// failure classification
// ---------------------------------------------------------------------------

func TestClassifyPrefillFailure(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantClass    string
		wantCode     int
		wantTerminal bool
	}{
		{"nil", nil, "", 0, false},
		{"http 500", &PrefillHTTPError{StatusCode: 500, Body: "boom"}, PrefillFailureHTTPStatus, 500, true},
		{"http 503 wrapped", fmt.Errorf("wrapped: %w", &PrefillHTTPError{StatusCode: 503}), PrefillFailureHTTPStatus, 503, true},
		{"timeout", fmt.Errorf("do: %w", context.DeadlineExceeded), PrefillFailureTimeout, 0, true},
		{"canceled", fmt.Errorf("do: %w", context.Canceled), PrefillFailureCanceled, 0, true},
		{"transport", fmt.Errorf("connection refused"), PrefillFailureTransport, 0, true},
		{"setup", &PrefillSetupError{Err: fmt.Errorf("bad url")}, PrefillFailureRequestSetup, 0, true},
		{"bad response", &PrefillBodyError{Err: fmt.Errorf("not json")}, PrefillFailureBadResponse, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, code := classifyPrefillFailure(tt.err)
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantTerminal, PrefillFailureIsTerminal(class))
		})
	}
}

// A net.Error timeout that never reached the context deadline (dial timeout,
// response-header timeout) is still a timeout, not a generic transport error.
func TestClassifyPrefillFailureNetTimeout(t *testing.T) {
	class, code := classifyPrefillFailure(&net.DNSError{Err: "i/o timeout", IsTimeout: true})
	assert.Equal(t, PrefillFailureTimeout, class)
	assert.Zero(t, code)
}

func TestRecordPrefillFailureKeepsFirst(t *testing.T) {
	leg := abortTestLeg(t, "req-1", "req-1-aaaaaaaabbbbbbbb", "")

	first := RecordPrefillFailure(leg, &PrefillHTTPError{StatusCode: 500, Body: "boom"})
	require.NotNil(t, first)
	assert.Equal(t, PrefillFailureHTTPStatus, first.Class)
	assert.Equal(t, 500, first.StatusCode)
	assert.False(t, first.At.IsZero())

	// A second failure still describes the error it was handed, but the leg
	// keeps the first one: it is the one the gateway already acted on.
	second := RecordPrefillFailure(leg, context.Canceled)
	require.NotNil(t, second)
	assert.Equal(t, PrefillFailureCanceled, second.Class)
	assert.Same(t, first, leg.PrefillFailure())

	assert.Nil(t, RecordPrefillFailure(leg, nil))

	select {
	case <-leg.PrefillFailed():
	default:
		t.Fatal("recording a failure must wake the fail-fast path exactly once")
	}
}

// ---------------------------------------------------------------------------
// decode abort
// ---------------------------------------------------------------------------

func TestAbortDecodeOnPrefillFailure(t *testing.T) {
	// One attempt only: the retry path has its own tests below.
	withAbortRetryDelay(t, 0)

	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	rid := "req-abort-aaaaaaaabbbbbbbb"
	leg := abortTestLeg(t, "req-abort", rid, strings.TrimPrefix(decodeSrv.URL, "http://"))

	failure := OnPrefillLegFailed(decodeSrv.Client(), leg, "req-abort", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError, Body: `{"error":"prefill blew up"}`})

	require.NotNil(t, failure)
	assert.Equal(t, PrefillFailureHTTPStatus, failure.Class)
	assert.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	assert.Same(t, failure, leg.PrefillFailure(), "the failure must be recorded for the gateway to act on")

	body := recorder.wait(t, 5*time.Second)
	assert.Equal(t, rid, gjson.Get(body, "rid").String(), "abort must carry the rid injected into both legs")
	assert.False(t, gjson.Get(body, "abort_all").Bool(), "abort_all must never be set")
	waitAbortDone(t, leg)
	recorder.none(t, 300*time.Millisecond)
}

// A 200 with an unparseable body means the KV transfer completed: the decode
// leg must be left alone.
func TestNoAbortWhenPrefillReturnedUnparseableBody(t *testing.T) {
	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	leg := abortTestLeg(t, "req-badbody", "req-badbody-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))

	failure := OnPrefillLegFailed(decodeSrv.Client(), leg, "req-badbody", "test-model",
		&PrefillBodyError{Err: fmt.Errorf("prefill response is not a JSON object")})
	require.NotNil(t, failure)
	assert.Equal(t, PrefillFailureBadResponse, failure.Class)
	assert.NotNil(t, leg.PrefillFailure(), "the failure is still recorded; only the abort is suppressed")
	recorder.none(t, 300*time.Millisecond)
}

func TestAbortSkippedWhenDecodeAlreadyStreaming(t *testing.T) {
	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	leg := abortTestLeg(t, "req-streaming", "req-streaming-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))
	// The decode pod has already answered: this is what the response-headers /
	// response-body paths in the gateway set.
	leg.MarkDecodeResponded()

	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-streaming", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))
	assert.NotNil(t, leg.PrefillFailure())
	recorder.none(t, 300*time.Millisecond)
}

func TestAbortSkippedWithoutRIDOrDecodeTarget(t *testing.T) {
	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()
	decodeAddr := strings.TrimPrefix(decodeSrv.URL, "http://")

	t.Run("no rid", func(t *testing.T) {
		// Non-SGLang PD engines carry no gateway-owned rid, so there is nothing
		// /abort_request could match on.
		leg := abortTestLeg(t, "req-norid", "", decodeAddr)
		require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-norid", "test-model", context.DeadlineExceeded))
		recorder.none(t, 300*time.Millisecond)
	})

	t.Run("no decode target", func(t *testing.T) {
		leg := abortTestLeg(t, "req-notarget", "req-notarget-aaaaaaaabbbbbbbb", "")
		require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-notarget", "test-model", context.DeadlineExceeded))
		recorder.none(t, 300*time.Millisecond)
	})
}

// The abort must not gate the prefill goroutine: a decode pod that accepts the
// connection and then hangs (the observed failure mode) may not hold it open.
func TestAbortTimeoutDoesNotDelayAnything(t *testing.T) {
	withAbortRetryDelay(t, 0)
	withAbortTimeout(t, 1)

	recorder := newAbortRecorder()
	recorder.release = make(chan struct{})
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()
	defer recorder.releaseHandlers()

	leg := abortTestLeg(t, "req-hang", "req-hang-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))

	start := time.Now()
	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-hang", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))
	assert.Less(t, time.Since(start), 500*time.Millisecond, "OnPrefillLegFailed must return without waiting on the abort")

	recorder.wait(t, 5*time.Second)
	recorder.releaseHandlers()
	waitAbortDone(t, leg)
}

func TestDecodeAbortDisabledByZeroTimeout(t *testing.T) {
	withAbortRetryDelay(t, 0)
	withAbortTimeout(t, 0)

	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	leg := abortTestLeg(t, "req-disabled", "req-disabled-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))

	failure := OnPrefillLegFailed(decodeSrv.Client(), leg, "req-disabled", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusBadGateway})
	require.NotNil(t, failure)
	assert.Equal(t, http.StatusBadGateway, failure.StatusCode, "the failure is still recorded and reported")
	recorder.none(t, 300*time.Millisecond)
}

// TestDecodeAbortEnvHonoursZero pins the env -> value path of both abort knobs.
// The package-level atomics are filled at init, long before a test can touch
// the environment, so the loaders behind them are exercised directly. The bug
// this guards against is utils.LoadEnvInt silently turning a deployed
// AIBRIX_DECODE_ABORT_TIMEOUT=0 back into the 3s default, so aborts keep firing
// on a cluster that has switched them off.
func TestDecodeAbortEnvHonoursZero(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		set  bool
		// expected seconds; -1 means "the compiled default". The retry delay
		// loader returns nanoseconds and is compared after the same conversion.
		expected int
	}{
		{name: "unset", set: false, expected: -1},
		{name: "zero", raw: "0", set: true, expected: 0},
		{name: "positive", raw: "5", set: true, expected: 5},
		{name: "negative", raw: "-3", set: true, expected: -1},
		{name: "unparseable", raw: "abc", set: true, expected: -1},
	}

	setEnv := func(t *testing.T, key, raw string, set bool) {
		t.Helper()
		// t.Setenv first either way, so the variable is restored afterwards
		// even in the "unset" case.
		t.Setenv(key, raw)
		if !set {
			require.NoError(t, os.Unsetenv(key))
		}
	}

	t.Run("AIBRIX_DECODE_ABORT_TIMEOUT", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				setEnv(t, "AIBRIX_DECODE_ABORT_TIMEOUT", tc.raw, tc.set)
				want := tc.expected
				if want < 0 {
					want = defaultDecodeAbortTimeout
				}
				assert.Equal(t, want, loadDecodeAbortTimeoutSeconds())
			})
		}
	})

	t.Run("AIBRIX_DECODE_ABORT_RETRY_DELAY", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				setEnv(t, "AIBRIX_DECODE_ABORT_RETRY_DELAY", tc.raw, tc.set)
				want := tc.expected
				if want < 0 {
					want = defaultDecodeAbortRetryDelay
				}
				assert.Equal(t, time.Duration(want)*time.Second,
					time.Duration(loadDecodeAbortRetryDelayNanos()))
			})
		}
	})
}

// postDecodeAbort must not inherit the (usually already cancelled) request
// context: the abort is exactly what has to survive it.
func TestPostDecodeAbortIgnoresRequestCancellation(t *testing.T) {
	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	addr := strings.TrimPrefix(decodeSrv.URL, "http://")
	require.NoError(t, postDecodeAbort(context.Background(), decodeSrv.Client(), addr, "rid-1", 2*time.Second))
	assert.Equal(t, "rid-1", gjson.Get(recorder.wait(t, time.Second), "rid").String())

	// A non-200 from the decode pod surfaces as an error, never a panic.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	err := postDecodeAbort(context.Background(), bad.Client(), strings.TrimPrefix(bad.URL, "http://"), "rid-2", time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), strconv.Itoa(http.StatusServiceUnavailable))

	assert.Error(t, postDecodeAbort(context.Background(), nil, addr, "rid-3", time.Second),
		"a missing client is an error, not a panic")
}

// ---------------------------------------------------------------------------
// abort retry
// ---------------------------------------------------------------------------

// The first abort can beat the decode request to the decode pod when the
// prefill pod refuses the connection outright. The engine then takes its
// "unknown rid" branch and, with the default single tokenizer worker, returns
// without dispatching anything - while /abort_request still answers 200. The
// gateway therefore repeats the abort.
func TestAbortRetriedWhenDecodeNeverRegistered(t *testing.T) {
	withAbortRetryDelay(t, 50*time.Millisecond)

	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	rid := "req-retry-aaaaaaaabbbbbbbb"
	leg := abortTestLeg(t, "req-retry", rid, strings.TrimPrefix(decodeSrv.URL, "http://"))

	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-retry", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))

	first := recorder.wait(t, 5*time.Second)
	second := recorder.wait(t, 5*time.Second)
	assert.Equal(t, rid, gjson.Get(first, "rid").String())
	assert.Equal(t, first, second, "the retry must repeat the identical abort payload")

	waitAbortDone(t, leg)
	// Exactly two: the retry fires once, it does not loop.
	recorder.none(t, 400*time.Millisecond)
}

func TestAbortNotRetriedWhenRetryDelayIsZero(t *testing.T) {
	withAbortRetryDelay(t, 0)

	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	rid := "req-noretry-aaaaaaaabbbbbbbb"
	leg := abortTestLeg(t, "req-noretry", rid, strings.TrimPrefix(decodeSrv.URL, "http://"))

	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-noretry", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))

	assert.Equal(t, rid, gjson.Get(recorder.wait(t, 5*time.Second), "rid").String())
	waitAbortDone(t, leg)
	recorder.none(t, 400*time.Millisecond)
}

// If the decode pod starts answering between the two attempts, the KV transfer
// landed after all and the second abort must not go out. The pending retry
// must also stop waiting, rather than sleep out a delay it will only skip at
// the end of.
func TestAbortRetrySkippedWhenDecodeStartsStreaming(t *testing.T) {
	// Far longer than this test is willing to wait for the goroutine to end,
	// so a retry that merely skips the POST at the end of the delay fails here
	// instead of passing slowly.
	withAbortRetryDelay(t, 30*time.Second)

	recorder := newAbortRecorder()
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()

	leg := abortTestLeg(t, "req-retry-streaming", "req-retry-streaming-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))

	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-retry-streaming", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))

	recorder.wait(t, 5*time.Second)
	// Response headers arrive from the decode pod while the retry is pending.
	leg.MarkDecodeResponded()
	waitAbortDone(t, leg)
	recorder.none(t, 300*time.Millisecond)
}

// The decode leg can also start answering while the first abort POST is still
// on the wire - the abort is aimed at a pod that is by definition slow to
// answer. The gateway then cancels its own request instead of holding the
// connection open for the full AIBRIX_DECODE_ABORT_TIMEOUT, and records the
// attempt for what it is: skipped, not failed.
func TestAbortCancelledWhenDecodeStartsStreamingMidFlight(t *testing.T) {
	// Both knobs are set far beyond what this test waits for, so nothing below
	// can be explained by a deadline expiring on its own.
	withAbortTimeout(t, 30)
	withAbortRetryDelay(t, 30*time.Second)

	aborts, cleanup := metrics.SetupCounterMetricsForTest(
		metrics.GatewayPDDecodeAbortTotal, []string{"result", "prefill_failure_class"})
	defer cleanup()

	recorder := newAbortRecorder()
	recorder.release = make(chan struct{})
	decodeSrv := recorder.server(t)
	defer decodeSrv.Close()
	defer recorder.releaseHandlers()

	leg := abortTestLeg(t, "req-midflight", "req-midflight-aaaaaaaabbbbbbbb", strings.TrimPrefix(decodeSrv.URL, "http://"))

	require.NotNil(t, OnPrefillLegFailed(decodeSrv.Client(), leg, "req-midflight", "test-model",
		&PrefillHTTPError{StatusCode: http.StatusInternalServerError}))

	// The first abort has reached the decode pod, which is sitting on it.
	recorder.wait(t, 5*time.Second)

	// Response headers arrive: there is nothing left to abort.
	leg.MarkDecodeResponded()

	select {
	case <-recorder.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight abort was not cancelled when the decode leg started answering")
	}

	waitAbortDone(t, leg)
	recorder.none(t, 300*time.Millisecond)

	assert.Zero(t, testutil.ToFloat64(aborts.WithLabelValues(abortResultError, PrefillFailureHTTPStatus)),
		"an abort the gateway cancelled itself is not a failed abort")
	assert.Equal(t, 2.0, testutil.ToFloat64(aborts.WithLabelValues(abortResultSkippedStreaming, PrefillFailureHTTPStatus)),
		"both the cancelled attempt and the retry it pre-empted are recorded as skipped")
}
