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

// This file implements the client-facing half of PD fail-fast.
//
// The decode-side half (algorithms/pd/abort.go) stops the decode pod from
// sitting on pre-allocated KV pages after the prefill leg died. It does nothing
// for the client: the ext_proc stream that owns the request is parked in
// srv.Recv() waiting for Envoy to forward the decode leg's response headers,
// and those only arrive once the decode pod gives up on its bootstrap - minutes
// later.
//
// Here the wait becomes a select: the message from Envoy, the shutdown signal,
// or RoutingContext.PrefillFailed(), the edge that the prefill goroutine closes
// when it records its failure. When that third case wins, the stream answers
// the client immediately instead of waiting for a decode response that will
// never be worth having.

package gateway

import (
	"strconv"

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
	// prefillFailFastStageBefore / ...After are the "stage" label of
	// GatewayPDPrefillFailureTotal and the "stage" field of the
	// pd_prefill_fail_fast log line: they say whether the decode pod had
	// started answering the client when the prefill failure surfaced, which is
	// what decides whether the gateway can still replace the response.
	prefillFailFastStageBefore = "before_response"
	prefillFailFastStageAfter  = "after_response"

	// prefillFailFastStatus is the "status" label the gateway's generic request
	// counters carry for a fail-fast, so it is separable from an upstream 503
	// in the same dashboards.
	prefillFailFastStatus = "pd_prefill_failure"

	// maxPrefillFailureMessage bounds how much of the prefill error text is
	// echoed to the client. The message is the Go error from the prefill call,
	// which already only names the prefill target the routing context chose.
	maxPrefillFailureMessage = 256
)

// handlePrefillFailFast runs when RoutingContext.PrefillFailed() fires. It
// decides what the client is told and returns the error that ends the stream,
// or nil to keep serving it.
//
// The caller has already disarmed the wakeup, so returning nil parks the loop
// back on Envoy's messages instead of spinning on the closed channel.
func (s *Server) handlePrefillFailFast(srv extProcPb.ExternalProcessor_ProcessServer, st *processState) error {
	failure := st.routerCtx.PrefillFailure()
	if failure == nil {
		// Woken without a failure behind the edge. Not reachable through the
		// per-incarnation leg state, but the loop must not act on nothing:
		// go back to waiting on Envoy.
		return nil
	}

	if !pd.PrefillFailureIsTerminal(failure.Class) {
		// bad_response: the prefill pod answered 200 and the KV transfer
		// completed, the gateway just could not parse the reply. The decode leg
		// is healthy and is not aborted either (see pd.OnPrefillLegFailed), so
		// the client must not be failed.
		klog.V(4).InfoS("pd_prefill_fail_fast_ignored", "request_id", st.requestID,
			"rid", st.routerCtx.PDRequestID(), "class", failure.Class)
		return nil
	}

	stage := prefillFailFastStageBefore
	if st.routerCtx.DecodeResponded() {
		stage = prefillFailFastStageAfter
	}
	statusCode := prefillFailureStatusCode(failure)

	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: st.model}, nil,
		metrics.GatewayPDPrefillFailureTotal, &metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"class": failure.Class, "stage": stage})

	klog.ErrorS(nil, "pd_prefill_fail_fast",
		"request_id", st.requestID,
		"rid", st.routerCtx.PDRequestID(),
		"class", failure.Class,
		"status_code", int(statusCode),
		"stage", stage,
		"stream", st.stream,
		"prefill_error", truncatePrefillMessage(failure.Message))

	if stage == prefillFailFastStageAfter {
		if st.routerCtx.Engine == pd.EngineTRTLLM {
			// TRT generation-first can send SSE response headers before KV
			// arrives. Headers are not proof that decode is making progress.
			// We cannot replace a response already on the wire, but closing
			// ext_proc fails/resets the upstream stream (failure_mode_allow
			// must remain false). TRT cancels its promise on disconnect.
			// Conservatively reset even if some tokens have already arrived:
			// a terminal CTX failure must not leave an unbounded GEN waiter.
			s.emitPrefillFailFastCounters(st, statusCode)
			s.finishRequestCount(st)
			return status.Errorf(codes.Aborted, "TRT prefill leg failed after decode headers (%s): %s",
				failure.Class, truncatePrefillMessage(failure.Message))
		}
		// The decode pod is already writing to the client, so the response is
		// past the point where ext_proc can replace it: an ImmediateResponse
		// now would reset a stream the client is already reading, and the
		// injection alternative (rewriting the next chunk into an SSE error
		// event plus data: [DONE], which response_body_mode: STREAMED would
		// technically allow) would have to fabricate a terminator for a
		// generation that is still running and would desynchronise the token
		// accounting HandleResponseBody keeps for the same chunks.
		//
		// It is also the case that matters least: once the decode leg is
		// responding, the KV transfer landed and the generation is live, so the
		// late prefill error is almost always the gateway losing sight of an
		// otherwise finished prefill. Record it and let the stream run.
		klog.ErrorS(nil, "pd_prefill_failure_after_decode_response",
			"request_id", st.requestID, "rid", st.routerCtx.PDRequestID(),
			"class", failure.Class, "stream", st.stream)
		return nil
	}

	return s.failStreamOnPrefillFailure(srv, st, failure, statusCode)
}

// failStreamOnPrefillFailure answers the client for a prefill failure observed
// before the decode leg produced anything, and ends the ext_proc stream.
//
// Why both an ImmediateResponse and a gRPC error:
//
// The ImmediateResponse carries everything worth carrying - the mapped HTTP
// status, the gateway's usual OpenAI-shaped error body, the x-error-pd-prefill
// header - and ext_proc documents it as "create a locally generated response,
// send it downstream, stop processing additional filters, and ignore any
// additional messages received from the remote server for this request"
// (external_processor.proto, ImmediateResponse), with no requirement that it
// answer a specific message.
//
// What it *is* subject to is the rule one message up: "For every
// ProcessingRequest received by the server with the async_mode field set to
// false, the server must send back exactly one ProcessingResponse message"
// (external_processor.proto, ProcessingResponse). The gateway already answered
// the RequestBody message with the routed headers, so at this instant Envoy is
// not waiting on it, and an ext_proc filter that does not special-case
// immediate_response ahead of its per-message state machine is entitled to
// treat this as a spurious response.
//
// So the stream is also closed with a gRPC error, which needs no goodwill from
// the filter: the Envoy configurations shipped in this repo leave
// failure_mode_allow at its false default, and with that a failed ext_proc
// stream fails the request with a locally generated 5xx and resets the upstream
// decode connection. The two together degrade cleanly: the client gets the
// precise error when the ImmediateResponse is honoured, and a generic 5xx when
// it is not - never the multi-minute hang, which is the whole point.
//
// Ordering is safe because gRPC preserves it on a stream: Envoy sees the
// message before the status, and a filter that acted on the ImmediateResponse
// has already marked itself complete and ignores the close.
func (s *Server) failStreamOnPrefillFailure(srv extProcPb.ExternalProcessor_ProcessServer,
	st *processState, failure *types.PrefillFailure, statusCode envoyTypePb.StatusCode) error {
	message := truncatePrefillMessage(failure.Message)
	resp := buildErrorResponse(statusCode,
		"prefill request failed ("+failure.Class+"): "+message,
		"", "",
		HeaderErrorPDPrefill, "true",
		// Same correlation header the other gateway-generated error responses
		// carry, so this response can be tied to the gateway and engine logs.
		HeaderRequestID, st.requestID)

	// sendProcessingResponse owns the send-failure accounting, so a client that
	// is already gone still unwinds through exactly one path.
	if err := s.sendProcessingResponse(srv, st, resp); err != nil {
		return err
	}

	s.emitPrefillFailFastCounters(st, statusCode)

	// Same terminal bookkeeping as every other early return in the loop:
	// finishRequestCount is idempotent (Process's deferred fallback runs it
	// too) and must run while routerCtx is still owned by this processState.
	s.finishRequestCount(st)

	return status.Errorf(codes.Aborted, "pd prefill leg failed (%s): %s", failure.Class, message)
}

// emitPrefillFailFastCounters mirrors the counter handleProcessingRequest emits
// for any other ImmediateResponse, so a fail-fast shows up as a failed request
// and never as gateway_request_success.
func (s *Server) emitPrefillFailFastCounters(st *processState, statusCode envoyTypePb.StatusCode) {
	if st.model == "" {
		return
	}
	s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model,
		prefillFailFastStatus, strconv.Itoa(int(statusCode)), st.routerCtx)
}

// prefillFailureStatusCode maps a prefill failure class onto the status the
// client sees.
//
// http_status passes the prefill pod's own code through when it is a 4xx/5xx
// Envoy knows how to emit - a 400 from a malformed prompt or a 507 from an OOM
// scheduler is the client's answer, not the gateway's - and degrades to 502 for
// anything else, since a non-error status that still counted as a failure means
// the gateway and the pod disagree about the exchange. Everything else is the
// gateway failing to reach or to finish with the prefill pod, which is 503.
func prefillFailureStatusCode(failure *types.PrefillFailure) envoyTypePb.StatusCode {
	if failure == nil {
		return envoyTypePb.StatusCode_ServiceUnavailable
	}
	if failure.Class == pd.PrefillFailureHTTPStatus {
		if failure.StatusCode >= 400 && failure.StatusCode <= 599 {
			// Envoy validates HttpStatus.code as defined_only, so only pass
			// through codes the enum actually has a name for.
			if _, ok := envoyTypePb.StatusCode_name[int32(failure.StatusCode)]; ok {
				return envoyTypePb.StatusCode(failure.StatusCode)
			}
		}
		return envoyTypePb.StatusCode_BadGateway
	}
	return envoyTypePb.StatusCode_ServiceUnavailable
}

// truncatePrefillMessage bounds an upstream error string before it is copied
// into a client-facing body or a log field.
func truncatePrefillMessage(s string) string {
	if len(s) <= maxPrefillFailureMessage {
		return s
	}
	return s[:maxPrefillFailureMessage] + "..."
}
