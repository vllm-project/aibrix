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

package engine

import (
	"fmt"
	"math/rand"
	"sync/atomic"
)

// The gateway owns the engine-visible request id ("rid") of a PD request.
//
// SGLang accepts a client-supplied rid in the request body and its
// /abort_request endpoint matches live requests by rid only - the abort
// payload carries no bootstrap_room - so injecting a known rid into both legs
// is the only way the gateway can cancel the decode leg when the prefill leg
// dies. The X-Request-Id header is not a substitute: SGLang reads the rid from
// the JSON body and ignores that header.
//
// Two properties are required of that rid:
//
//  1. Prefix safety. The scheduler aborts every request whose rid has the
//     aborted rid as a prefix, across the waiting queue, the running batch and
//     the decode preallocation/transfer queues. A rid that is a prefix of
//     another live rid would take innocent requests down with it. Every
//     gateway rid therefore ends with a fixed-width 16 hex-digit nonce, so no
//     gateway rid can be a proper prefix of another: two rids can only differ
//     inside that suffix, which is the same length for all of them.
//
//  2. Per-attempt uniqueness. SGLang keys its in-flight request state by rid,
//     and the gateway's RequestID is not guaranteed unique across attempts:
//     when the client sends a traceparent, RequestID is the 32-hex trace id,
//     which a retrying client reuses. The nonce - process-random high half,
//     monotonic counter low half - makes every attempt distinct even when
//     RequestID repeats, while keeping RequestID as the human-readable prefix
//     so engine logs remain greppable by it.
var (
	pdRIDNonceBase = rand.Uint32()
	pdRIDNonceSeq  atomic.Uint32
)

// NewPDRequestID derives the engine-visible rid of one PD attempt from the
// gateway request id. Fixed-width suffix; see the comment above.
func NewPDRequestID(requestID string) string {
	return fmt.Sprintf("%s-%08x%08x", requestID, pdRIDNonceBase, pdRIDNonceSeq.Add(1))
}
