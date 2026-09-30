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

// This file implements the decode watchdog of PD fail-fast.
//
// gateway_pd_fail_fast.go covers a prefill leg that dies. It says nothing about
// the other half of the disaggregated request. In SGLang disaggregation the
// prefill leg's HTTP call only returns once the decode pod has taken the KV
// transfer, so a *successful* prefill is itself a statement about the decode
// pod: the request is in its running batch and it must start answering.
//
// When it does not - the decode pod's scheduler dies mid-request or wedges -
// nobody says so. The prefill goroutine finished happily, Envoy is waiting on a
// decode response that will never come, the ext_proc stream is parked in its
// select, and the client hangs until Envoy's route timeout, which is typically
// very long or disabled for streaming LLM routes. The routing context and its
// load accounting stay live the whole time, too.
//
// The watchdog gives that wait a deadline: once the prefill leg succeeded and
// while nothing has come back from the decode pod, the stream fails the request
// after a per-mode timeout. Nothing is armed before the prefill leg succeeds:
// the prefill call has its own timeout, and its failure is fail-fast's.
//
// The budget depends on what the first message from the decode pod means. On a
// streaming request it is the first token, so AIBRIX_DECODE_FIRST_RESPONSE_-
// TIMEOUT measures time-to-first-token and 60s is generous. On a non-streaming
// request the engine sends the response headers only once the whole generation
// is finished, so the same wait is the entire decode; that mode has its own
// AIBRIX_DECODE_RESPONSE_TIMEOUT, off by default.
//
// The watchdog is armed only for a request that carries a gateway-owned rid
// (leg.RID() != ""), i.e. only for SGLang PD. Every other stream selects on
// exactly the channels it did before: a nil timer channel and a nil wakeup
// channel both block forever.

package gateway

import (
	"net/http"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

const (
	// decodeWatchdogPhaseFirstResponse is the "phase" label of
	// GatewayPDDecodeWatchdogTotal and the "phase" field of the
	// pd_decode_watchdog log line.
	decodeWatchdogPhaseFirstResponse = "first_response"

	// decodeWatchdogStatus is the "status" label the gateway's failed-request
	// counter carries for a watchdog kill, so it is separable from an upstream
	// 504 and from a prefill fail-fast in the same dashboards.
	decodeWatchdogStatus = "pd_decode_watchdog"
)

// decodeAbortClient is the client the stream side uses for watchdog aborts.
//
// The routers own an http.Client because they also send the prefill leg; the
// gateway Server has none, and the abort is the only outbound call the stream
// side makes, so one package-level client with the routers' pooling settings
// is enough. Its Timeout is only a backstop: every abort carries its own
// AIBRIX_DECODE_ABORT_TIMEOUT deadline.
var decodeAbortClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// decodeFirstMessageTimeout is the watchdog's budget for this particular
// stream; a non-positive value disables it.
//
// The two modes are not the same measurement, so they cannot share a number:
// for a streaming response the first ext_proc message is the first token, for a
// non-streaming one it is the finished answer.
func (st *processState) decodeFirstMessageTimeout() time.Duration {
	watchdog := st.routerCtx.PDOverrides().Watchdog
	if st.stream {
		return watchdog.FirstResponseTimeout
	}
	return watchdog.ResponseTimeout
}

// decodeWatchdogDeadline returns when the watchdog should fire for this stream
// and in which phase, or the zero time when nothing is armed.
//
// It is recomputed from the leg on every pass of the loop rather than kept as
// state, so every event that changes the answer - the prefill leg succeeding,
// the decode pod's first message, the response completing - re-arms or disarms
// the timer by construction, with no separate bookkeeping to keep in sync.
func (st *processState) decodeWatchdogDeadline() (time.Time, string) {
	leg := st.routerCtx.PDLeg()
	// No leg, no rid: not an SGLang PD request (or not routed yet). Nothing is
	// armed, and such a stream behaves exactly as it did without the watchdog.
	if leg == nil || leg.RID() == "" || st.completed || leg.DecodeResponded() {
		return time.Time{}, ""
	}

	succeededAt := leg.PrefillSucceededAt()
	timeout := st.decodeFirstMessageTimeout()
	if succeededAt.IsZero() || timeout <= 0 {
		return time.Time{}, ""
	}
	return succeededAt.Add(timeout), decodeWatchdogPhaseFirstResponse
}

// armDecodeWatchdog (re)arms the stream's one-shot timer for d and returns the
// channel to select on.
//
// One timer per stream, reused: allocating a fresh one per loop pass would
// leave the old one pending in the runtime's heap until it fired. Reuse means
// Reset, and Reset is only safe on a stopped-and-drained timer, which is what
// stopDecodeWatchdog does; watchdogPending tracks whether an undelivered fire
// might still be sitting in the channel, so the drain never blocks on a value
// the select already took.
func (st *processState) armDecodeWatchdog(d time.Duration) <-chan time.Time {
	if d < 0 {
		d = 0
	}
	if st.watchdog == nil {
		st.watchdog = time.NewTimer(d)
		st.watchdogPending = true
		return st.watchdog.C
	}
	st.stopDecodeWatchdog()
	st.watchdog.Reset(d)
	st.watchdogPending = true
	return st.watchdog.C
}

// stopDecodeWatchdog stops the stream's timer and drains a fire that may
// already be in flight. Idempotent, and safe on a stream that never armed one,
// so Process can defer it unconditionally.
func (st *processState) stopDecodeWatchdog() {
	if st.watchdog == nil {
		return
	}
	if !st.watchdog.Stop() && st.watchdogPending {
		select {
		case <-st.watchdog.C:
		default:
		}
	}
	st.watchdogPending = false
}

// handleDecodeWatchdog runs when the watchdog timer fires: the decode leg of
// this PD request has stopped talking. It aborts the decode leg, fails the
// client and returns the error that ends the ext_proc stream.
//
// Nothing has been written to the client yet, so the gateway answers it the
// same way a prefill fail-fast does - an ImmediateResponse with 504 and the
// usual OpenAI-shaped body, then a gRPC error close. See
// failStreamOnPrefillFailure for why both are sent.
func (s *Server) handleDecodeWatchdog(srv extProcPb.ExternalProcessor_ProcessServer, st *processState, phase string) error {
	leg := st.routerCtx.PDLeg()
	decodeAddr, decodePod := leg.DecodeTarget()
	timeout := st.decodeFirstMessageTimeout()

	klog.ErrorS(nil, "pd_decode_watchdog",
		"request_id", st.requestID,
		"rid", leg.RID(),
		"decode_pod", decodePod,
		"decode_addr", decodeAddr,
		"phase", phase,
		"elapsed", time.Since(leg.PrefillSucceededAt()),
		"timeout", timeout,
		"stream", st.stream)

	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: st.model}, nil,
		metrics.GatewayPDDecodeWatchdogTotal, &metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"phase": phase})

	// Tell the decode pod to drop the request so it stops holding KV pages for
	// a client that is about to be disconnected. Non-blocking, and it only
	// holds the leg, which outlives the routing context released when this
	// stream ends.
	pd.AbortDecodeLegOnWatchdog(decodeAbortClient, leg, st.requestID, st.model, pd.AbortTriggerWatchdogFirstResponse)

	// What the missing message was: a first token on a streaming request, the
	// finished answer on a non-streaming one. The phase label stays
	// first_response either way, and the log line already carries "stream".
	what := "did not start responding within "
	if !st.stream {
		what = "did not finish responding within "
	}
	resp := buildErrorResponse(envoyTypePb.StatusCode_GatewayTimeout,
		"decode pod "+decodePod+" "+what+timeout.String(),
		"", "",
		HeaderErrorPDDecode, "true",
		HeaderRequestID, st.requestID)
	return s.failStreamWithResponse(srv, st, resp, envoyTypePb.StatusCode_GatewayTimeout, decodeWatchdogStatus,
		status.Errorf(codes.DeadlineExceeded, "pd decode leg sent nothing within %s", timeout))
}
