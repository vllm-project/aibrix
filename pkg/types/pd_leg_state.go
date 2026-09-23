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

package types

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
)

// PrefillFailure describes a terminal failure of the prefill leg of a
// disaggregated (PD) request. For engines whose prefill handshake is
// self-coordinating (SGLang) the prefill leg is fire-and-forget, so the failure
// is recorded on the request's PD leg state instead of being returned: the
// gateway goroutine that owns the client stream picks it up from there.
type PrefillFailure struct {
	// Class is a coarse, low-cardinality failure category
	// ("timeout", "transport", "http_status", ...) suitable as a metric label.
	Class string
	// StatusCode is the prefill pod's HTTP status when Class is "http_status",
	// 0 otherwise.
	StatusCode int
	// Message is the error text, for logging and for the client-facing error
	// the gateway synthesises.
	Message string
	// At is when the failure was observed.
	At time.Time
}

// ClassOrEmpty returns the failure class, or "" for a nil failure. It lets a
// caller log the class of a failure it has just recorded without first having
// to decide whether one was recorded at all.
func (f *PrefillFailure) ClassOrEmpty() string {
	if f == nil {
		return ""
	}
	return f.Class
}

// PDLegState is the prefill/decode leg state of one *incarnation* of a request.
//
// It exists as a separate heap object because RoutingContext is pooled
// (requestPool) while the async prefill leg is fire-and-forget: the goroutine
// that posts the prefill request routinely outlives the client stream. The
// prefill HTTP call is made on a context derived from the request context, so a
// client cancel fails the prefill leg at the very instant the stream goroutine
// runs its terminal path and hands the RoutingContext back to the pool.
//
// If that goroutine reported through the pooled RoutingContext, its report
// could land on whichever request had since taken the object out of the pool:
// reset() clears the failure slot, so the stale report would win the CAS on the
// *new* occupant, close the new request's wakeup channel and fail a healthy
// stream - and the decode abort that follows would carry the new request's rid.
// Capturing a *PDLegState before launching the goroutine closes that window by
// construction: reset() installs a brand-new PDLegState, so a goroutine holding
// a retired one can only ever mutate an object that nothing selects on any more.
//
// Every method is nil-receiver-safe: a RoutingContext built as a struct literal
// (the gateway does that for metric emission) has no leg, and callers should
// not have to care.
type PDLegState struct {
	// rid is the engine-visible request id the PD router injects into the JSON
	// body of both disaggregation legs. It is derived from
	// RoutingContext.RequestID and is what the gateway sends to /abort_request
	// to cancel the decode leg. Empty for every non-PD request and for the PD
	// engines that do not accept a client-supplied rid. Atomic because the
	// request path sets it while the abort goroutine and the stream goroutine
	// read it.
	rid atomic.Pointer[string]

	// decodeResponded is set the first time anything comes back from the decode
	// pod for this request (response headers, or the first response body chunk
	// when the filter chain is configured without header callbacks). Once set,
	// the decode leg is live towards the client and a late prefill failure must
	// not abort it: the KV transfer already landed and the pod is generating.
	decodeResponded atomic.Bool

	// prefillFailure records the first terminal failure of the prefill leg.
	// The async prefill leg is fire-and-forget, so this is the only channel
	// through which the request-processing goroutine can learn that the decode
	// leg will never receive its KV cache.
	prefillFailure atomic.Pointer[PrefillFailure]

	// prefillFailed is the wakeup edge for prefillFailure. The goroutine that
	// owns the client's ext_proc stream is parked waiting for Envoy to forward
	// the decode leg's response headers, which never arrive when the prefill
	// leg died; it selects on this channel to learn about the failure without
	// polling. Allocated with the struct and never replaced - a PDLegState is
	// used by exactly one incarnation - so it needs no atomic and no lazy
	// creation. It is closed exactly once, by whichever SetPrefillFailure call
	// wins the prefillFailure CAS.
	prefillFailed chan struct{}

	// decodeTarget is where the decode leg of this request was sent, captured
	// on the request path by the PD router. The prefill goroutine needs it to
	// abort a decode leg whose KV will never arrive, and cannot derive it
	// itself: the routing context it could read it from is pooled.
	decodeTarget atomic.Pointer[pdDecodeTarget]

	// pdOverrides are the PD routing overrides the request's model config
	// profile resolved to. They are resolved on the request path and read from
	// the leg rather than the routing context for the same reason as
	// decodeTarget: the async prefill goroutine, and the decode abort it can
	// start, outlive the client stream and must not read a routing context that
	// may have been recycled.
	pdOverrides atomic.Pointer[PDOverrides]

	// abortMu guards the two fields below. The abort context is built by the
	// goroutine that sends the abort and cancelled by the goroutine that owns
	// the client stream, so the two race; both critical sections are a single
	// field access.
	abortMu     sync.Mutex
	abortCtx    context.Context
	abortCancel context.CancelFunc

	// abortDone is closed once the decode abort of this incarnation is over:
	// either the goroutine that sends it has exited, or the failure path
	// decided not to start one at all. Allocated with the struct, closed at
	// most once, and never replaced - a PDLegState belongs to exactly one
	// incarnation. See AbortDone.
	abortDone     chan struct{}
	abortDoneOnce sync.Once
}

// closedAbortDone is what AbortDone hands a nil leg: there is no abort
// goroutine anywhere near a request that has no PD leg state at all.
var closedAbortDone = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// pdDecodeTarget is the decode pod's abort address and name, kept in one
// immutable object so the two are always read as a consistent pair.
type pdDecodeTarget struct {
	addr    string
	podName string
}

// newPDLegState returns the leg state for a fresh incarnation of a request.
func newPDLegState() *PDLegState {
	return &PDLegState{
		prefillFailed: make(chan struct{}),
		abortDone:     make(chan struct{}),
	}
}

// SetRID records the engine-visible rid. Only the first call has any effect in
// practice: the PD router derives the rid once, before either leg is sent.
func (l *PDLegState) SetRID(rid string) {
	if l == nil || rid == "" {
		return
	}
	l.rid.Store(&rid)
}

// RID returns the engine-visible request id injected into both PD legs, or ""
// when this request has no gateway-owned rid.
func (l *PDLegState) RID() string {
	if l == nil {
		return ""
	}
	if rid := l.rid.Load(); rid != nil {
		return *rid
	}
	return ""
}

// MarkDecodeResponded records that the decode pod has started answering, and
// cancels the abort context with it: a decode leg that is producing tokens has
// received its KV cache, so an /abort_request still in flight - or a retry
// still waiting out its delay - is now pointless work aimed at a healthy
// request.
//
// The flag is stored before the cancel so that an abort context built
// concurrently with this call comes back already cancelled: AbortContext
// re-reads the flag under the same lock.
func (l *PDLegState) MarkDecodeResponded() {
	if l == nil {
		return
	}
	l.decodeResponded.Store(true)
	l.cancelAbort()
}

// AbortContext returns the context that bounds the best-effort abort of this
// request's decode leg.
//
// It is rooted at context.Background(), not at the request context: by the
// time a prefill failure surfaces, the request context is usually cancelled
// already and the abort is precisely the work that has to outlive it. It is
// cancellable all the same, because the abort has a stop condition of its own
// - see MarkDecodeResponded.
//
// The context is built on first use, so the overwhelming majority of requests,
// which never abort anything, do not pay for one.
func (l *PDLegState) AbortContext() context.Context {
	if l == nil {
		return context.Background()
	}
	l.abortMu.Lock()
	defer l.abortMu.Unlock()
	if l.abortCtx == nil {
		l.abortCtx, l.abortCancel = context.WithCancel(context.Background())
		if l.decodeResponded.Load() {
			// The decode leg started answering before there was anything to
			// cancel; the abort must still come back a cancelled one.
			l.abortCancel()
		}
	}
	return l.abortCtx
}

// FinishDecodeAbort ends the decode-abort lifecycle of this incarnation: it
// releases the abort context and makes AbortDone() ready. It is idempotent,
// and every path through the prefill-failure handling has to reach it -
// including the ones that send no abort at all - or a waiter is left hanging.
func (l *PDLegState) FinishDecodeAbort() {
	if l == nil {
		return
	}
	l.cancelAbort()
	l.abortDoneOnce.Do(func() { close(l.abortDone) })
}

// AbortDone returns a channel that is closed once the decode-abort handling of
// this incarnation has finished: the abort goroutine has exited, or the
// prefill-failure path decided not to start one.
//
// The abort is fire-and-forget yet still writes a log line and a metric
// sample, so it needs a join point: a test that drives a prefill failure has
// to be able to wait for it, or the goroutine outlives the test and races
// whatever the test tears down on its way out.
//
// It is *not* a "nothing is running" predicate: the channel of a leg whose
// prefill never failed stays open, because no failure was handled on it.
// Callers wait on it once they know a failure was reported. A nil leg has no
// abort to wait for and gets an already-closed channel.
func (l *PDLegState) AbortDone() <-chan struct{} {
	if l == nil {
		return closedAbortDone
	}
	return l.abortDone
}

// cancelAbort cancels the abort context, if one was ever built.
func (l *PDLegState) cancelAbort() {
	l.abortMu.Lock()
	cancel := l.abortCancel
	l.abortMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// DecodeResponded reports whether anything has come back from the decode pod.
func (l *PDLegState) DecodeResponded() bool {
	if l == nil {
		return false
	}
	return l.decodeResponded.Load()
}

// SetPrefillFailure records the terminal failure of the prefill leg and reports
// whether this call was the one that recorded it. Only the first failure is
// kept, and the winner of the CAS - and only the winner - closes PrefillFailed(),
// so the close happens exactly once however many goroutines race here.
//
// No "is this leg still live?" guard is needed: a retired PDLegState is inert
// by construction. Nothing selects on its channel any more and nothing reads
// its failure, so a stale report is a write into a private object that is about
// to be garbage collected.
func (l *PDLegState) SetPrefillFailure(failure *PrefillFailure) bool {
	if l == nil || failure == nil {
		return false
	}
	if !l.prefillFailure.CompareAndSwap(nil, failure) {
		return false
	}
	if l.prefillFailed != nil {
		close(l.prefillFailed)
	}
	return true
}

// PrefillFailure returns the recorded prefill failure, or nil when the prefill
// leg has not failed (yet).
func (l *PDLegState) PrefillFailure() *PrefillFailure {
	if l == nil {
		return nil
	}
	return l.prefillFailure.Load()
}

// PrefillFailed returns a channel that is closed when the prefill leg of this
// request fails. It never carries a value: the failure itself is read back with
// PrefillFailure(). A nil leg yields a nil channel, which blocks forever in a
// select - the correct "this can never fire" semantics for a non-PD stream.
func (l *PDLegState) PrefillFailed() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.prefillFailed
}

// SetDecodeTarget records the address and pod name of the decode leg, so a
// prefill failure can be aimed at the pod that is waiting for the KV transfer.
func (l *PDLegState) SetDecodeTarget(addr, podName string) {
	if l == nil || addr == "" {
		return
	}
	l.decodeTarget.Store(&pdDecodeTarget{addr: addr, podName: podName})
}

// DecodeTarget returns the decode leg's address and pod name, or ("", "") when
// the request was never routed to a decode pod.
func (l *PDLegState) DecodeTarget() (addr string, podName string) {
	if l == nil {
		return "", ""
	}
	if target := l.decodeTarget.Load(); target != nil {
		return target.addr, target.podName
	}
	return "", ""
}

// SetPDOverrides records the request's PD routing overrides. A nil argument is
// a no-op and the read sites keep the process defaults.
func (l *PDLegState) SetPDOverrides(o *PDOverrides) {
	if l == nil || o == nil {
		return
	}
	l.pdOverrides.Store(o)
}

// PDOverrides returns the request's PD routing overrides, or the process
// defaults when none were recorded. The returned struct is read-only and never
// nil.
func (l *PDLegState) PDOverrides() *PDOverrides {
	if l != nil {
		if o := l.pdOverrides.Load(); o != nil {
			return o
		}
	}
	return DefaultPDOverrides()
}

// PDLeg returns the PD leg state of this incarnation of the request, or nil
// when there is none. Callers that outlive the client stream - the async
// prefill goroutine above all - must capture this pointer before they are
// launched and use it instead of the RoutingContext, which is pooled and may
// by then belong to another request.
func (r *RoutingContext) PDLeg() *PDLegState {
	if r == nil {
		return nil
	}
	return r.pdLeg.Load()
}

// SetPDRequestID records the engine-visible request id ("rid") the PD router
// injected into both disaggregation legs of this request.
func (r *RoutingContext) SetPDRequestID(rid string) {
	r.PDLeg().SetRID(rid)
}

// PDRequestID returns the engine-visible request id ("rid") injected into both
// PD legs, or "" when this request has no gateway-owned rid.
func (r *RoutingContext) PDRequestID() string {
	return r.PDLeg().RID()
}

// MarkDecodeResponded records that the decode pod has started responding.
// Nil-safe: the response paths call it without knowing whether a routing
// context was ever created for the stream.
func (r *RoutingContext) MarkDecodeResponded() {
	r.PDLeg().MarkDecodeResponded()
}

// DecodeResponded reports whether the decode pod has started responding.
func (r *RoutingContext) DecodeResponded() bool {
	return r.PDLeg().DecodeResponded()
}

// SetPrefillFailure records the terminal failure of the prefill leg on the
// current incarnation. Callers that may outlive the client stream must hold the
// *PDLegState from PDLeg() instead, or they risk recording onto the next
// request that takes this pooled object.
func (r *RoutingContext) SetPrefillFailure(failure *PrefillFailure) bool {
	return r.PDLeg().SetPrefillFailure(failure)
}

// PrefillFailed returns the wakeup channel of the current incarnation.
func (r *RoutingContext) PrefillFailed() <-chan struct{} {
	return r.PDLeg().PrefillFailed()
}

// PrefillFailure returns the recorded prefill failure of the current
// incarnation, or nil.
func (r *RoutingContext) PrefillFailure() *PrefillFailure {
	return r.PDLeg().PrefillFailure()
}

// SetDecodeTarget records where the decode leg of this request was sent.
func (r *RoutingContext) SetDecodeTarget(addr, podName string) {
	r.PDLeg().SetDecodeTarget(addr, podName)
}

// DecodeTarget returns where the decode leg of this request was sent.
func (r *RoutingContext) DecodeTarget() (addr string, podName string) {
	return r.PDLeg().DecodeTarget()
}

// SetPDOverrides records the PD routing overrides of this incarnation of the
// request on its PD leg, where the async prefill and abort paths read them.
func (r *RoutingContext) SetPDOverrides(o *PDOverrides) {
	if r == nil {
		return
	}
	r.PDLeg().SetPDOverrides(o)
}

// PDOverrides returns the PD routing overrides of this incarnation of the
// request, or the process defaults when it has none.
func (r *RoutingContext) PDOverrides() *PDOverrides {
	if r == nil {
		return DefaultPDOverrides()
	}
	return r.PDLeg().PDOverrides()
}

// PodAddress returns the host:port the gateway would forward this request to if
// pod were its target, without making pod the target. The PD router needs the
// decode pod's address before SetTargetPod is called, so the abort of a failed
// prefill lands on exactly the HTTP server that is serving the decode leg.
func (r *RoutingContext) PodAddress(pod *v1.Pod) string {
	if r == nil || pod == nil || pod.Status.PodIP == "" {
		return ""
	}
	return r.targetAddress(pod)
}
