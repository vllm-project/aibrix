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

// This file holds the pd-package half of the decode watchdog: its knobs and
// the abort it sends. The watchdog itself runs in the gateway's stream loop
// (gateway_decode_watchdog.go), which owns the client stream but not the abort
// protocol.

package pd

import (
	"context"
	"net/http"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// defaultDecodeFirstResponseTimeout bounds, for a streaming request, the
	// wait for the decode pod's first token - its first response body chunk -
	// after the prefill leg succeeded.
	// Generous: it has to cover a decode pod that is merely queued behind a
	// long batch, not one that is dead.
	defaultDecodeFirstResponseTimeout = 60

	// defaultDecodeResponseTimeout is 0, i.e. the watchdog is off for
	// non-streaming requests. There the first body chunk from the decode pod is
	// the finished answer, so the budget has to cover a whole generation, whose
	// length is set by the caller's max_tokens and the pod's throughput. Any
	// default would kill somebody's legitimate long answer.
	defaultDecodeResponseTimeout = 0

	// defaultDecodeStreamIdleTimeout bounds the gap between two body chunks of
	// a streaming response once the first one arrived. A healthy stream sends a
	// chunk per token or per few tokens, so a gap of minutes means the pod is
	// stuck.
	defaultDecodeStreamIdleTimeout = 120
)

// Abort triggers that are not prefill failures. They travel in the same slot
// as a prefill failure class - the prefill_failure_class log field and metric
// label of gateway_pd_decode_abort_total - because a counter's label set is
// fixed at registration and that counter predates the watchdog.
const (
	// AbortTriggerWatchdogFirstResponse: the decode pod sent no response body
	// after the prefill leg succeeded, whether or not it sent headers
	// (AIBRIX_DECODE_FIRST_RESPONSE_TIMEOUT, or AIBRIX_DECODE_RESPONSE_TIMEOUT
	// for a non-streaming request).
	AbortTriggerWatchdogFirstResponse = "watchdog_first_response"

	// AbortTriggerWatchdogStreamIdle: the decode pod started streaming tokens
	// and then went silent for AIBRIX_DECODE_STREAM_IDLE_TIMEOUT.
	AbortTriggerWatchdogStreamIdle = "watchdog_stream_idle"
)

// loadDecodeWatchdogTimeouts reads the process defaults of the decode watchdog
// timeouts (see types.PDWatchdogOverrides).
//
// utils.LoadEnvNonNegativeInt, not utils.LoadEnvInt: the latter treats every
// value <= 0 as invalid and falls back to its default, which would make the
// documented "0 disables this phase" unreachable from the environment - and an
// operator facing a misbehaving watchdog must be able to switch it off without
// a new build.
func loadDecodeWatchdogTimeouts() types.PDWatchdogOverrides {
	seconds := func(key string, def int) time.Duration {
		return time.Duration(utils.LoadEnvNonNegativeInt(key, def)) * time.Second
	}
	return types.PDWatchdogOverrides{
		FirstResponseTimeout: seconds("AIBRIX_DECODE_FIRST_RESPONSE_TIMEOUT", defaultDecodeFirstResponseTimeout),
		ResponseTimeout:      seconds("AIBRIX_DECODE_RESPONSE_TIMEOUT", defaultDecodeResponseTimeout),
		StreamIdleTimeout:    seconds("AIBRIX_DECODE_STREAM_IDLE_TIMEOUT", defaultDecodeStreamIdleTimeout),
	}
}

// AbortDecodeLegOnWatchdog sends a single best-effort /abort_request for the
// leg's rid to its decode pod, logged and counted like an abort fired by a
// prefill failure, with trigger in place of the failure class.
//
// It is the decode watchdog's abort: the watchdog has decided the decode pod
// stopped answering and is failing the client, and this tells the pod to drop
// the request instead of generating for a client that is gone. It never blocks:
// the POST runs in its own goroutine, bounded by the request's
// AIBRIX_DECODE_ABORT_TIMEOUT, and nothing branches on its outcome. The
// goroutine is joinable through leg.AbortDone().
//
// One attempt, unlike OnPrefillLegFailed's two. That retry covers an abort
// overtaking its own decode request on the pod, which cannot happen here: the
// watchdog only fires after the decode request has been on the pod for at
// least the configured timeout (and, for a stream-idle kill, after the pod has
// already answered).
//
// The prefill-failure abort and this one never share a leg: the watchdog is
// armed by the prefill leg succeeding, the other by it failing, so the leg's
// abort lifecycle (AbortContext, AbortDone) belongs to whichever ran.
func AbortDecodeLegOnWatchdog(client *http.Client, leg *types.PDLegState, requestID, model, trigger string) {
	abortLaunched := false
	defer func() {
		if !abortLaunched {
			leg.FinishDecodeAbort()
		}
	}()

	rid := leg.RID()
	decodeAddr, decodePodName := leg.DecodeTarget()
	logAbort := decodeAbortLog{
		rid:        rid,
		requestID:  requestID,
		model:      model,
		decodePod:  decodePodName,
		decodeAddr: decodeAddr,
		class:      trigger,
	}.emit

	timeout := decodeAbortTimeoutFor(leg)
	switch {
	case rid == "":
		logAbort(0, abortResultSkippedNoRID, 0, nil)
		return
	case timeout <= 0:
		logAbort(0, abortResultSkippedDisabled, 0, nil)
		return
	case decodeAddr == "":
		logAbort(0, abortResultSkippedNoTarget, 0, nil)
		return
	}

	// Not leg.AbortContext(): that context exists to stop a prefill-failure
	// abort once the decode pod starts answering, and a watchdog abort is
	// sent regardless of whether it has. Nothing cancels this one but its own
	// timeout.
	abortLaunched = true
	go func() {
		defer leg.FinishDecodeAbort()
		start := time.Now()
		if err := postDecodeAbort(context.Background(), client, decodeAddr, rid, timeout); err != nil {
			logAbort(1, abortResultError, time.Since(start), err)
			return
		}
		logAbort(1, abortResultOK, time.Since(start), nil)
	}()
}
