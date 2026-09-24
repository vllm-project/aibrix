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

// This file implements the decode-side half of PD fail-fast.
//
// In SGLang disaggregation the prefill leg is fire-and-forget: the gateway
// posts it to the prefill pod from a goroutine while Envoy forwards the main
// request to the decode pod. The prefill pod's HTTP call only returns after
// the bootstrap handshake with the decode pod and the KV transfer, so when it
// fails the decode pod is usually already parked waiting for KV that will
// never arrive. SGLang has no prefill->decode failure notification, so the
// decode request sits there until its bootstrap timeout expires (300s by
// default), holding its pre-allocated KV pages the whole time.
//
// The gateway can cut that short because it knows both ends: it injects its
// own rid into both legs (see engine.NewPDRequestID) and, when the prefill leg
// fails, posts {"rid": <rid>} to the decode pod's /abort_request. The abort is
// strictly best-effort: it runs in its own goroutine with its own short
// timeout and its result gates nothing.
//
// That abort races the decode leg's own arrival. When the prefill leg fails
// instantly - a dead prefill pod refuses the connection before Envoy has even
// finished forwarding the decode leg - the abort can reach the decode pod
// before the decode request is registered there. SGLang's tokenizer manager
// then hits its "rid not known" branch and, with the single-tokenizer default,
// returns without dispatching anything to the scheduler; /abort_request
// answers 200 either way, so the gateway cannot distinguish a matched abort
// from a dropped one. The decode request would then hang for the full
// bootstrap timeout - exactly the case fail-fast exists for.
//
// The abort is therefore sent twice: once immediately, and once more after
// AIBRIX_DECODE_ABORT_RETRY_DELAY, unless the decode leg has started answering
// in between. Aborting an rid the engine has already aborted, or never knew,
// is a no-op on both sides, so the retry needs no result-dependent logic and
// no de-duplication.
//
// "Unless the decode leg has started answering" holds at every point of that
// sequence, not just at the two decision points: the abort runs on the leg's
// own context (types.PDLegState.AbortContext), which the stream goroutine
// cancels the moment the decode pod produces anything, so a POST in flight is
// cut short and a retry still waiting out its delay stops waiting.

package pd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	// defaultDecodeAbortTimeout bounds the abort POST. The decode pod this
	// abort is aimed at is, by construction, a pod that is stuck or unhealthy,
	// so the call must give up quickly rather than pile goroutines onto it.
	defaultDecodeAbortTimeout = 3

	// defaultDecodeAbortRetryDelay spaces the two abort attempts. It has to
	// outlast the window between "the gateway sent the decode leg" and "the
	// decode pod registered it", which is a local dispatch on the pod, so a
	// couple of seconds is generous.
	defaultDecodeAbortRetryDelay = 2

	// sglangAbortRequestPath is SGLang's native abort endpoint. Its payload is
	// AbortReq{rid, abort_all}; the scheduler matches live requests by rid
	// prefix, which is why the rid the gateway sends must never be a prefix of
	// another live request's rid (see engine.NewPDRequestID).
	sglangAbortRequestPath = "/abort_request"
)

// decodeAbortTimeoutFor is the per-attempt deadline of this request's abort
// POST; a non-positive value disables decode aborts. The value is the request's
// resolved PD overrides, which carry the AIBRIX_DECODE_ABORT_TIMEOUT default
// when its profile sets none. The leg, not the routing context, is the source:
// an abort outlives the request that started it.
func decodeAbortTimeoutFor(leg *types.PDLegState) time.Duration {
	return leg.PDOverrides().Abort.Timeout
}

// loadDecodeAbortTimeoutSeconds reads AIBRIX_DECODE_ABORT_TIMEOUT, the process
// default for the deadline of the /abort_request call the gateway posts to the
// decode pod after a prefill failure (see types.PDOverrides for the knob).
//
// utils.LoadEnvNonNegativeInt, not utils.LoadEnvInt: the latter treats every
// value <= 0 as invalid and falls back to its default, which would make the
// documented "0 disables decode aborts" unreachable from the environment.
func loadDecodeAbortTimeoutSeconds() int {
	return utils.LoadEnvNonNegativeInt("AIBRIX_DECODE_ABORT_TIMEOUT", defaultDecodeAbortTimeout)
}

// decodeAbortRetryDelayFor is the wait between the two abort attempts of this
// request; a non-positive value means a single attempt. See
// decodeAbortTimeoutFor for why the leg is the source.
func decodeAbortRetryDelayFor(leg *types.PDLegState) time.Duration {
	return leg.PDOverrides().Abort.RetryDelay
}

// loadDecodeAbortRetryDelayNanos reads AIBRIX_DECODE_ABORT_RETRY_DELAY, the
// process default for the wait before repeating the abort once, to cover the
// case where the first attempt arrived at the decode pod before the decode
// request itself (see the race described at the top of this file). Same reason
// as above for utils.LoadEnvNonNegativeInt: 0 is the documented "send a single
// attempt" setting, not an invalid value.
func loadDecodeAbortRetryDelayNanos() int64 {
	seconds := utils.LoadEnvNonNegativeInt("AIBRIX_DECODE_ABORT_RETRY_DELAY", defaultDecodeAbortRetryDelay)
	return int64(time.Duration(seconds) * time.Second)
}

// Prefill failure classes. Low cardinality: they are used as a log field and
// as a metric label, and the gateway maps them to a client-facing error.
const (
	// PrefillFailureRequestSetup: the HTTP request could not even be built
	// (bad URL, nil body). The prefill pod was never contacted.
	PrefillFailureRequestSetup = "request_setup"
	// PrefillFailureTimeout: AIBRIX_PREFILL_REQUEST_TIMEOUT elapsed, or the
	// transport reported a timeout. The prefill may be in any state; the
	// gateway can no longer observe it.
	PrefillFailureTimeout = "timeout"
	// PrefillFailureCanceled: the request context was cancelled, i.e. the
	// client went away or the gateway is shutting down.
	PrefillFailureCanceled = "canceled"
	// PrefillFailureTransport: connection refused/reset, DNS, EOF - the
	// prefill pod did not answer.
	PrefillFailureTransport = "transport"
	// PrefillFailureHTTPStatus: the prefill pod answered with a non-200. This
	// is the OOM / crashed-scheduler / bootstrap-timeout case.
	PrefillFailureHTTPStatus = "http_status"
	// PrefillFailureBadResponse: HTTP 200 with a body the gateway cannot
	// parse. The KV transfer itself completed, so this one does NOT abort the
	// decode leg.
	PrefillFailureBadResponse = "bad_response"
)

// Abort outcomes, used as the abort_result log field and metric label.
const (
	abortResultOK               = "ok"
	abortResultError            = "error"
	abortResultSkippedStreaming = "skipped_streaming"
	abortResultSkippedDisabled  = "skipped_disabled"
	abortResultSkippedNoRID     = "skipped_no_rid"
	abortResultSkippedNoTarget  = "skipped_no_target"
)

// PrefillHTTPError is returned by the prefill executor when the prefill pod
// answers with a non-200, so callers can classify the failure and read the
// status code back out instead of parsing an error string.
type PrefillHTTPError struct {
	StatusCode int
	Body       string
}

func (e *PrefillHTTPError) Error() string {
	return fmt.Sprintf("http prefill request failed with status %d: %s", e.StatusCode, e.Body)
}

// PrefillBodyError is returned when the prefill pod answered 200 but the body
// could not be parsed. Kept distinct from a transport failure because the KV
// transfer did complete: the decode leg must not be aborted.
type PrefillBodyError struct {
	Err error
}

func (e *PrefillBodyError) Error() string { return e.Err.Error() }
func (e *PrefillBodyError) Unwrap() error { return e.Err }

// PrefillSetupError marks a failure to build the prefill HTTP request.
type PrefillSetupError struct {
	Err error
}

func (e *PrefillSetupError) Error() string { return e.Err.Error() }
func (e *PrefillSetupError) Unwrap() error { return e.Err }

// classifyPrefillFailure maps an error returned by the prefill executor to a
// failure class and, for PrefillFailureHTTPStatus, the prefill pod's status
// code. The ordering matters: a timeout surfaces as a *url.Error wrapping
// context.DeadlineExceeded, so context errors are checked before the generic
// transport fallback.
func classifyPrefillFailure(err error) (string, int) {
	if err == nil {
		return "", 0
	}

	var httpErr *PrefillHTTPError
	if errors.As(err, &httpErr) {
		return PrefillFailureHTTPStatus, httpErr.StatusCode
	}

	var bodyErr *PrefillBodyError
	if errors.As(err, &bodyErr) {
		return PrefillFailureBadResponse, 0
	}

	var setupErr *PrefillSetupError
	if errors.As(err, &setupErr) {
		return PrefillFailureRequestSetup, 0
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return PrefillFailureTimeout, 0
	}
	if errors.Is(err, context.Canceled) {
		return PrefillFailureCanceled, 0
	}

	// net.Error covers i/o timeouts that never reached the context deadline
	// (dial timeout, response-header timeout on the shared client).
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return PrefillFailureTimeout, 0
	}

	return PrefillFailureTransport, 0
}

// PrefillFailureIsTerminal reports whether a prefill failure of this class
// means the client request itself can never be served, and not merely that the
// gateway lost sight of an otherwise healthy prefill.
//
// It is the same predicate that decides whether to abort the decode leg, and
// deliberately so: the decode leg is aborted exactly when the KV cache will
// never arrive, which is exactly when the client must be told the request
// failed instead of being left to wait out the bootstrap timeout.
//
// Everything except PrefillFailureBadResponse qualifies: a 200 with an
// unparseable body still completed the KV transfer, and aborting then would
// kill a healthy request.
func PrefillFailureIsTerminal(class string) bool {
	return class != "" && class != PrefillFailureBadResponse
}

// RecordPrefillFailure classifies err and records it on the PD leg state as the
// terminal failure of the prefill leg. The first failure wins; the returned
// value always describes the error passed in, so callers can log the class of
// the failure they just observed.
//
// It takes the leg rather than the RoutingContext on purpose: the async prefill
// goroutine can outlive the client stream, and the RoutingContext is pooled, so
// recording through the context risks recording onto an unrelated request (see
// types.PDLegState). Request-path callers pass routingCtx.PDLeg().
//
// This is the state the gateway consumes: it is what turns "the prefill
// goroutine logged an error somewhere" into something the goroutine owning the
// client stream can act on.
func RecordPrefillFailure(leg *types.PDLegState, err error) *types.PrefillFailure {
	if err == nil {
		return nil
	}
	class, statusCode := classifyPrefillFailure(err)
	failure := &types.PrefillFailure{
		Class:      class,
		StatusCode: statusCode,
		Message:    err.Error(),
		At:         time.Now(),
	}
	leg.SetPrefillFailure(failure)
	return failure
}

// decodeAbortLog is the shared shape of the pd_decode_abort log line and of the
// gateway_pd_decode_abort_total sample that goes with it.
type decodeAbortLog struct {
	rid        string
	requestID  string
	model      string
	decodePod  string
	decodeAddr string
	// class is the low-cardinality cause of this abort: the prefill failure
	// class ("transport", "http_status", ...). It is what the
	// prefill_failure_class metric label carries.
	class string
	// statusCode is the prefill pod's HTTP status for an http_status failure,
	// 0 otherwise.
	statusCode int
}

// emit writes one pd_decode_abort line and the counter sample that mirrors it.
// attempt is 1 or 2 for a dispatched abort, 0 for the decisions taken before
// any POST was made.
func (l decodeAbortLog) emit(attempt int, result string, latency time.Duration, abortErr error) {
	fields := []interface{}{
		"rid", l.rid,
		"request_id", l.requestID,
		"decode_pod", l.decodePod,
		"decode_addr", l.decodeAddr,
		"prefill_failure_class", l.class,
		"prefill_status_code", l.statusCode,
		"abort_attempt", attempt,
		"abort_result", result,
		"abort_latency", latency,
	}
	if abortErr != nil {
		klog.ErrorS(abortErr, "pd_decode_abort", fields...)
	} else {
		klog.InfoS("pd_decode_abort", fields...)
	}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: l.model}, nil,
		metrics.GatewayPDDecodeAbortTotal, &metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"result": result, "prefill_failure_class": l.class})
}

// OnPrefillLegFailed records a terminal prefill failure on the PD leg state
// and, unless the decode leg is already answering the client, fires a
// best-effort /abort_request at the decode pod.
//
// Everything it needs is passed in by value or as the per-incarnation
// *types.PDLegState, never as the RoutingContext: its caller is the async
// prefill goroutine, which can still be running long after the client stream
// released the (pooled) routing context. Reading or writing that object here
// would mean acting on whatever request has since been handed it - closing the
// wrong wakeup channel and aborting the wrong rid.
//
// It never blocks: the abort POST runs in its own goroutine, on the leg's
// abort context (the request context is typically already cancelled or about
// to be) with its own AIBRIX_DECODE_ABORT_TIMEOUT budget per attempt. Nothing
// downstream waits for, or branches on, the abort result - but the goroutine
// is joinable through leg.AbortDone(), because it still emits a log line and a
// metric sample after everything else about the request is over.
//
// The recorded types.PrefillFailure is what the gateway consumes to tell the
// client that the request died; it is set here, on the same path that already
// logs prefill_request_failed, so every prefill failure has it regardless of
// whether an abort was sent.
func OnPrefillLegFailed(client *http.Client, leg *types.PDLegState, requestID, model string, err error) *types.PrefillFailure {
	// Whatever is decided below, the leg's abort lifecycle ends here unless it
	// is handed to the goroutine: AbortDone() has to become ready even on the
	// paths that send no abort at all, or a waiter never wakes up.
	abortLaunched := false
	defer func() {
		if !abortLaunched {
			leg.FinishDecodeAbort()
		}
	}()

	failure := RecordPrefillFailure(leg, err)
	if failure == nil {
		return nil
	}
	class, statusCode := failure.Class, failure.StatusCode

	if !PrefillFailureIsTerminal(class) {
		return failure
	}

	// The rid and the decode target belong to the leg, so they are stable even
	// if the routing context has since been recycled for another request.
	rid := leg.RID()
	decodeAddr, decodePodName := leg.DecodeTarget()

	logAbort := decodeAbortLog{
		rid:        rid,
		requestID:  requestID,
		model:      model,
		decodePod:  decodePodName,
		decodeAddr: decodeAddr,
		class:      class,
		statusCode: statusCode,
	}.emit

	switch {
	case rid == "":
		// Non-SGLang PD engines do not carry a gateway-owned rid, so there is
		// nothing the abort endpoint could match.
		logAbort(0, abortResultSkippedNoRID, 0, nil)
		return failure
	case decodeAbortTimeoutFor(leg) <= 0:
		logAbort(0, abortResultSkippedDisabled, 0, nil)
		return failure
	case decodeAddr == "":
		logAbort(0, abortResultSkippedNoTarget, 0, nil)
		return failure
	case leg.DecodeResponded():
		// The decode pod is already streaming to the client: the KV transfer
		// landed and the prefill leg failed afterwards (or its HTTP response
		// was merely lost). Aborting now would kill a healthy generation.
		logAbort(0, abortResultSkippedStreaming, 0, nil)
		return failure
	}

	timeout := decodeAbortTimeoutFor(leg)
	retryDelay := decodeAbortRetryDelayFor(leg)
	// Taken before the goroutine starts, like everything else it needs: the
	// leg is per-incarnation and stays valid, the routing context does not.
	abortCtx := leg.AbortContext()
	abortLaunched = true
	go func() {
		// The goroutine owns the rest of the lifecycle: releasing the abort
		// context and making AbortDone() ready.
		defer leg.FinishDecodeAbort()

		// attemptAbort sends one abort. Its outcome deliberately does not feed
		// the decision to retry: a 200 from /abort_request does not mean the
		// engine matched the rid (see the race at the top of this file), so
		// the second attempt is scheduled on timing alone.
		attemptAbort := func(attempt int) {
			start := time.Now()
			// Re-check on every attempt: the decode leg may have started
			// answering the client since the last one, at which point the KV
			// transfer has landed and aborting would kill a live request.
			if leg.DecodeResponded() {
				logAbort(attempt, abortResultSkippedStreaming, 0, nil)
				return
			}
			if abortErr := postDecodeAbort(abortCtx, client, decodeAddr, rid, timeout); abortErr != nil {
				// A POST cut short by the abort context is not a failed
				// abort. The only thing that cancels that context is the
				// decode leg starting to answer, so this is the same
				// "nothing left to abort" outcome as the check above, just
				// observed while the request was already on the wire.
				if errors.Is(abortErr, context.Canceled) && abortCtx.Err() != nil {
					logAbort(attempt, abortResultSkippedStreaming, time.Since(start), nil)
					return
				}
				logAbort(attempt, abortResultError, time.Since(start), abortErr)
				return
			}
			logAbort(attempt, abortResultOK, time.Since(start), nil)
		}

		attemptAbort(1)
		if retryDelay <= 0 {
			return
		}
		// Repeating an abort the engine already applied, or never matched, is
		// harmless on both sides; this second attempt exists purely to catch
		// the decode request that had not been registered yet on the first.
		//
		// The wait is interruptible: a decode leg that starts answering in the
		// meantime must not leave a goroutine sleeping out the delay, holding
		// the retired leg state, only to skip the attempt at the end of it.
		// attemptAbort still runs, so the skip is recorded either way.
		retryTimer := time.NewTimer(retryDelay)
		select {
		case <-abortCtx.Done():
			retryTimer.Stop()
		case <-retryTimer.C:
		}
		attemptAbort(2)
	}()

	return failure
}

// postDecodeAbort posts {"rid": rid} to the decode pod's /abort_request.
//
// ctx is the caller's abort context, not the request context: the request
// context that carried the prefill leg is usually already cancelled by the
// time a prefill failure is observed, and the abort must still go out. timeout
// is the per-attempt deadline layered on top of it.
func postDecodeAbort(ctx context.Context, client *http.Client, decodeAddr, rid string, timeout time.Duration) error {
	if client == nil {
		return errors.New("no http client available for decode abort")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// AbortReq only needs the rid: abort_all defaults to false, and an empty
	// rid is refused by the engine (it would prefix-match every live request),
	// which is why callers must not reach here with rid == "".
	payload, err := sonic.Marshal(map[string]string{"rid": rid})
	if err != nil {
		return fmt.Errorf("failed to marshal abort payload: %w", err)
	}

	url := fmt.Sprintf("http://%s%s", decodeAddr, sglangAbortRequestPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create abort request: %w", err)
	}
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute abort request: %w", err)
	}
	defer func() {
		// Drain so the connection can be reused; the body is empty on success.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("abort request rejected with status %d", resp.StatusCode)
	}
	return nil
}
