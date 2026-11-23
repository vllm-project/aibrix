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

package blacklist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// EnvPodBlacklistEnabled is the master switch for the whole pod blacklist:
	// probe sweep, load-outlier sweep, flaky re-add, request failures and quarantine. Unset,
	// empty, false or 0 leaves every path a no-op so a healthy fleet matches
	// pre-blacklist routing.
	EnvPodBlacklistEnabled = "AIBRIX_POD_BLACKLIST_ENABLED"

	// EnvPodBlacklistMaxExcludedPercent caps how much of the pods a request considers the
	// blacklist may exclude: when more than this share of them is excluded, the request is
	// routed across all of them as if none were. Excluding most of a model's pods leaves the
	// few that remain to carry all of its traffic, and a verdict wrong for many pods at once
	// (the gateway's own network fault, say) is more likely than most of a fleet failing
	// together. 100 turns the cap off; every pod excluded is still routed across.
	EnvPodBlacklistMaxExcludedPercent = "AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT"
	defaultMaxExcludedPercent         = 50

	// EnvPodBlacklistRequestFailureThreshold is how many upstream failures in a row (see
	// IsPodFailureStatus) block the pod that served them. Any other response from the pod, or a
	// gap of requestFailureStreakGap between two failures, starts the count over. The source is
	// opt-in: the default 0 leaves it off, and so leaves the probe sweep, the load-outlier sweep
	// and the flaky re-add as the only sources, until the attribution policy has been measured on
	// real traffic (see #3390).
	EnvPodBlacklistRequestFailureThreshold = "AIBRIX_POD_BLACKLIST_REQUEST_FAILURE_THRESHOLD"
	defaultRequestFailureThreshold         = 0

	podBlacklistBaseDuration = 1 * time.Minute
	podBlacklistMaxDuration  = 10 * time.Minute
	// An entry is pruned once it has been untouched this long, counted from the
	// later of its last update and its openUntil (sweepStale), so a live backoff
	// or one-hour block is never pruned however quiet the pod is.
	podBlacklistEntryTTL      = 30 * time.Minute
	podBlacklistSweepInterval = 5 * time.Minute
	podHealthCheckPath        = "/health"
	podModelsCheckPath        = "/v1/models"
	// flakyReAddOpenDuration is how long a pod is proactively excluded from
	// routing after OnPodFlakyReAdd fires -- i.e. it reappeared in the cache
	// after being genuinely absent for a bit (see flakyReAddMinObservedGap in
	// pkg/cache/informers.go), with no check that it's actually healthy
	// again. Deliberately much shorter than podBlacklistBaseDuration: this
	// isn't a confirmed failure, just a reappearance, so the pod shouldn't sit
	// out of rotation for long.
	flakyReAddOpenDuration = 3 * time.Second
	// podProbeSweepInterval is how often the background sweep probes every
	// routable pod (see startPodProbeSweep).
	podProbeSweepInterval = 300 * time.Second
	// podProbeFailureIterations is how many consecutive sweeps a pod must fail
	// before the sweep blacklists it. A failed pod is simply probed again on
	// the next sweeps, so one blip (a probe timing out on a busy pod) does not
	// exclude it. Set to 1 to blacklist on the first failed sweep.
	podProbeFailureIterations = 3
	// podProbeRecoveryIterations is how many consecutive healthy sweeps lift a
	// block the sweep (or a flaky re-add) put on a pod before its deadline: the
	// probe is paid every sweep anyway, and an hour is a long time to keep out a
	// pod that has been answering correctly. More than one, so a pod that flaps
	// does not leave on a single lucky probe.
	podProbeRecoveryIterations = 2
	// podProbeDeadBlacklistDuration is how long the sweep blacklists a pod it
	// found dead: GET /health failing, or GET /v1/models answering 2xx with a
	// body that is not a JSON model list (see ProbeDead). It is deliberately
	// far longer than the degraded-pod backoff, so a stale registration is not
	// handed real traffic every few minutes. Every further
	// podProbeFailureIterations dead sweeps renew the hour, so a pod that stays
	// dead stays out.
	podProbeDeadBlacklistDuration = time.Hour
	// podRequestRecoverySuccesses is how many responses that are not failures, in a row, end a
	// pod's probation after a request-failure block: until then one more failure blocks it again
	// at once, with the next backoff, instead of needing the full threshold. Only responses
	// count, never a healthy probe sweep: a pod that passes /health and fails inference is the
	// case the request source exists for.
	podRequestRecoverySuccesses = 10
	// requestFailureStreakGap is the longest pause between two failures that still count as in
	// a row. A failure that arrives later than this after the previous one starts a new count,
	// so failures minutes apart on a pod with little traffic never add up to a block.
	requestFailureStreakGap = 1 * time.Minute
	// requestTripDecay is how long after a request block ends its trip count is kept. Past it the
	// pod is off probation and the backoff starts over, whether or not enough responses have come
	// to end the probation: at low traffic podRequestRecoverySuccesses can take a long time, and a
	// pod must not stay one stray failure from the 10-minute cap for as long as that takes.
	requestTripDecay = 2 * podBlacklistMaxDuration
	// podProbeConcurrency bounds the probes in flight during one sweep.
	podProbeConcurrency = 16
	// podProbeTimeout is how long the sweep waits for each request of a probe.
	// Nothing waits on the sweep, and a pod that is merely slow to answer must
	// not be judged dead. GET /health on the fleet's sglang pods takes
	// 1.4-1.7 s, which the sweep's earlier 1 s timeout turned into a
	// blacklisting for an hour.
	podProbeTimeout = 5 * time.Second
)

// Enabled is the process-wide master switch for the pod blacklist. It is loaded
// once from AIBRIX_POD_BLACKLIST_ENABLED (default false). When false, every
// exported method is a no-op and the background sweeps never start, so routing
// matches the pre-blacklist behaviour. Tests that exercise the feature set it
// to true (see TestMain).
var Enabled = utils.LoadEnvBool(EnvPodBlacklistEnabled, false)

// maxExcludedPercent is the share of the pods a request considers that the blacklist may
// exclude, loaded once from AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT (default 50, at most 100).
var maxExcludedPercent = min(utils.LoadEnvNonNegativeInt(EnvPodBlacklistMaxExcludedPercent, defaultMaxExcludedPercent), 100)

// requestFailureThreshold is the consecutive failures that block a pod, loaded once from
// AIBRIX_POD_BLACKLIST_REQUEST_FAILURE_THRESHOLD (default 0, which turns the source off).
// PodBlacklist.SetRequestFailureThreshold overrides it for one blacklist.
var requestFailureThreshold = utils.LoadEnvNonNegativeInt(EnvPodBlacklistRequestFailureThreshold, defaultRequestFailureThreshold)

// MaxExcludedPercent is the cap ExclusionOverCap applies, for the log line that reports it.
func MaxExcludedPercent() int { return maxExcludedPercent }

// SetMaxExcludedPercentForTest sets the cap and returns a function that puts the previous one
// back, for tests in other packages. It writes a process global: the test must not be parallel.
func SetMaxExcludedPercentForTest(percent int) (restore func()) {
	prev := maxExcludedPercent
	maxExcludedPercent = percent
	return func() { maxExcludedPercent = prev }
}

// ExclusionOverCap reports whether keeping kept of total pods, so excluding the rest, excludes
// more of them than AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT allows. The gateway then routes
// across all total pods instead. It never reports it for a share at or under the cap, so with
// the default 50, one pod of two or two of four may be excluded and two of three may not.
func ExclusionOverCap(kept, total int) bool {
	if total <= 0 {
		return false
	}
	return (total-kept)*100 > maxExcludedPercent*total
}

type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
)

// podFailureState is the exclusion state of one pod. The pod is excluded while
// its breaker is open and openUntil has not passed, while requestBlockedUntil has
// not passed, or while loadExcluded is set. Once a deadline passes the pod is
// admitted again with no probe request: the next background sweep (see
// RecordProbeResult) or the next failing response (see RecordResponse) is what
// decides whether it is excluded again.
type podFailureState struct {
	// mu is a read-write lock: Allow, which routing calls for every candidate, only reads, so
	// one request never waits for another to finish with the entry. The lock's own counter is
	// still shared, so many requests asking about the same pod at once still cost on the order
	// of 100 ns each. Everything that changes the state takes the write lock.
	mu sync.RWMutex
	// uid is the UID of the pod the state was observed on. A pod recreated under the same
	// name has a new UID: readers treat the entry as absent for it, and the first write for
	// it starts the entry over (adoptLocked). Until then the old pod's exclusion still counts
	// toward excluded and the excluded-pods gauge.
	uid   string
	state circuitState

	// tripCount counts consecutive open cycles and drives the backoff. A healthy
	// sweep after the block has lapsed starts it over.
	tripCount int
	openUntil time.Time
	// openReason is a human-readable explanation set whenever state transitions
	// to circuitOpen, surfaced via BlacklistReason. Stale once state leaves
	// circuitOpen, but BlacklistReason only ever reads it while state is still
	// circuitOpen, so it's never cleared on recovery.
	openReason string
	// Independent of the open/closed breaker; the next load-outlier sweep is the recovery check.
	loadExcluded bool
	// probeFailures counts consecutive background-sweep probes this pod has
	// failed while its breaker was closed; probeDeadSweeps counts consecutive
	// sweeps that found it dead (ProbeDead), in any breaker state. A passing
	// probe starts both over, a non-dead failure restarts probeDeadSweeps, and
	// probeFailures also restarts whenever the breaker is not closed. Any
	// failing probe starts probeHealthySweeps over.
	probeFailures   int
	probeDeadSweeps int
	// probeHealthySweeps counts consecutive healthy sweeps while the pod is
	// still blocked; podProbeRecoveryIterations of them lift the block.
	probeHealthySweeps int
	// quarantinedUntil is when the one-hour block the sweep gives a dead pod
	// ends (zero for any other kind of exclusion). IsQuarantined reads it.
	quarantinedUntil time.Time

	// The request-failure source (RecordResponse) is kept apart from the breaker above on
	// purpose: it has its own deadline and its own backoff, so a healthy probe sweep neither
	// lifts the block nor resets the escalation. A pod that passes /health and fails inference
	// is exactly what this source is for, and the sweep would call it healthy.
	//
	// requestBlockedUntil is when the block ends (zero when there is none). requestFailures
	// counts the failures in a row since the last response that was not one. requestTrips
	// counts blocks in a row and drives the backoff; while it is above zero the pod is on
	// probation (one failure blocks it again), until requestSuccesses reaches
	// podRequestRecoverySuccesses.
	//
	// lastRequestFailure is when the latest failure was counted: a streak older than
	// requestFailureStreakGap is over. requestTripEnd is when the latest block ended, or is set
	// to end: a trip count older than requestTripDecay is dropped. Only responses and blocks
	// write either, so neither is kept alive by the sweeps that refresh lastUpdated on a pod
	// whose request state has long been idle (see expireRequestLocked).
	requestBlockedUntil time.Time
	requestBlockReason  string
	requestFailures     int
	requestTrips        int
	requestSuccesses    int
	lastRequestFailure  time.Time
	requestTripEnd      time.Time

	// lastUpdated is when the entry was last changed by a sweep or an event. Reading it
	// through Allow does not count: an entry that routing keeps looking at but nothing
	// changes must still age out (see sweepStale).
	lastUpdated time.Time

	// counted is whether the entry is included in PodBlacklist.excluded, and removed is
	// set once sweepStale has deleted it; see syncCountLocked.
	counted bool
	removed bool
}

// blocksLocked reports whether state keeps its pod out of routing at now: it is
// load-excluded, its breaker is open with a block that has yet to end, or the request
// path has blocked it and that block has yet to end. Allow and IsBlacklisted both answer
// from it, so routing has one definition of "excluded". Caller must hold state.mu, for
// reading or writing.
func (state *podFailureState) blocksLocked(now time.Time) bool {
	return state.loadExcluded ||
		(state.state == circuitOpen && now.Before(state.openUntil)) ||
		now.Before(state.requestBlockedUntil)
}

// excludingLocked reports whether state can exclude its pod: its breaker is open or a
// request-failure block is set (even if the block has ended and nothing has closed it
// yet) or it is load-excluded. Caller must hold state.mu.
//
// It is what PodBlacklist.excluded counts, and it has no clock: the count only changes
// when an entry is written, and nothing writes one when its block ends. It is therefore a
// superset of blocksLocked at any instant, and must stay one: FilterBlacklisted skips
// every lookup while the count is zero, so a pod that blocksLocked excludes but this does
// not would be routed to. The other direction is the cost: an entry whose block has ended
// is still counted, and keeps every request off the fast path, until a writer closes it
// with lapseLocked (a probe sweep result, a flaky re-add) or sweepStale does.
func (state *podFailureState) excludingLocked() bool {
	return !state.removed && (state.loadExcluded || state.state == circuitOpen || !state.requestBlockedUntil.IsZero())
}

// adoptLocked starts state over for pod when state was observed on another pod of the same
// name, so a recreated pod inherits no block, probe or request-failure counts, or backoff
// from the one it replaced. It is not a recovery: the old pod did not get better. Every
// writer given the pod calls it before it writes, and ends with syncCountLocked, which
// takes a cleared exclusion off the count. A successful response for an entry with no
// request state writes nothing, so it does not adopt the entry.
//
// A UID mismatch is resolved last writer wins: a write for the old pod that lands after
// the new pod's (such as a response to a request routed to it before it was replaced)
// takes the entry back, and the new pod starts over again at its next write.
//
// mu, counted, removed and lastUpdated are kept. Caller must hold state.mu for writing.
func (state *podFailureState) adoptLocked(pod *v1.Pod) {
	if state.uid == string(pod.UID) {
		return
	}
	state.uid = string(pod.UID)
	state.state = circuitClosed
	state.tripCount = 0
	state.openUntil = time.Time{}
	state.openReason = ""
	state.loadExcluded = false
	state.probeFailures = 0
	state.probeDeadSweeps = 0
	state.probeHealthySweeps = 0
	state.quarantinedUntil = time.Time{}
	state.requestBlockedUntil = time.Time{}
	state.requestBlockReason = ""
	state.requestFailures = 0
	state.requestTrips = 0
	state.requestSuccesses = 0
	state.lastRequestFailure = time.Time{}
	state.requestTripEnd = time.Time{}
}

// lapseLocked closes an open breaker, and a request-failure block, whose block has
// ended, so the writers that branch on the state see a pod that is already admitted as
// closed. The trip counts are kept: a pod that is blocked again still escalates its
// backoff. Caller must hold state.mu.
func (state *podFailureState) lapseLocked(now time.Time) {
	if state.state == circuitOpen && !now.Before(state.openUntil) {
		state.state = circuitClosed
	}
	if !state.requestBlockedUntil.IsZero() && !now.Before(state.requestBlockedUntil) {
		state.requestBlockedUntil = time.Time{}
	}
}

// expireRequestLocked drops the request-failure state that has gone stale at now: a streak
// whose latest failure is more than requestFailureStreakGap old, and a trip count whose latest
// block ended more than requestTripDecay ago. It runs lazily, when a response arrives, and goes
// by the timestamps only responses write, never by lastUpdated, which the load sweep refreshes
// for every pod that has an entry. Caller must hold state.mu for writing.
func (state *podFailureState) expireRequestLocked(now time.Time) {
	if state.requestFailures > 0 && now.Sub(state.lastRequestFailure) > requestFailureStreakGap {
		state.requestFailures = 0
	}
	if state.requestTrips > 0 && now.Sub(state.requestTripEnd) > requestTripDecay {
		state.requestTrips, state.requestSuccesses = 0, 0
	}
}

// PodBlacklist is a per-pod exclusion list keyed by namespace/name. Each entry
// belongs to the pod UID it was observed on (see podFailureState.uid).
// Zero value is ready to use. Pods created by a StormService and pods that
// carry a ModelClaim binding are never tracked (see ignoredPod).
type PodBlacklist struct {
	pods sync.Map // "namespace/name" -> *podFailureState
	// tracked is how many entries pods holds.
	tracked atomic.Int64
	// excluded is how many of those entries can exclude their pod (excludingLocked).
	// FilterBlacklisted reads it to skip the per-pod lookups while it is zero, so one entry
	// that excludes nothing, such as a pod that failed a probe once, does not put every
	// request on the slow path. It is kept exact by syncCountLocked, which every change to
	// an entry's state ends with.
	excluded atomic.Int64

	// requestFailureThresholdOverride, when set, replaces the process-wide threshold for this
	// blacklist (see SetRequestFailureThreshold). Unset by default.
	requestFailureThresholdOverride atomic.Pointer[int]

	// Probe hooks. Nil uses the real HTTP probes (httpHealthCheckWithin,
	// httpListenerCheckWithin, fetchModelListWithin); tests set them so that
	// a probe never makes a network call.
	HealthCheck   func(addr string) bool
	IdentityCheck func(addr, model string) bool
}

// SetRequestFailureThreshold sets how many upstream failures in a row block a pod on this
// blacklist, in place of AIBRIX_POD_BLACKLIST_REQUEST_FAILURE_THRESHOLD, which every blacklist
// reads otherwise. 0 or less turns the request-failure source off. It touches no process global, so
// tests that set it may run in parallel.
func (b *PodBlacklist) SetRequestFailureThreshold(threshold int) {
	b.requestFailureThresholdOverride.Store(&threshold)
}

// requestThreshold is the threshold in force: the override, or else the process-wide value.
func (b *PodBlacklist) requestThreshold() int {
	if p := b.requestFailureThresholdOverride.Load(); p != nil {
		return *p
	}
	return requestFailureThreshold
}

// PodSource is the part of the cache the background sweeps read. cache.Cache
// satisfies it.
type PodSource interface {
	ListModels() []string
	ListPodsByModel(modelName string) (types.PodList, error)
}

// PodKey is the namespace/name key a pod is tracked under.
func PodKey(pod *v1.Pod) string {
	return utils.GeneratePodKey(pod.Namespace, pod.Name)
}

// syncCountLocked brings b.excluded in line with state, which has just changed: it adds
// the entry when it has become able to exclude its pod and takes it off when it no longer
// can, or has been deleted by sweepStale. A writer that still holds the pointer of an
// entry sweepStale has deleted finds removed set and counts nothing. Caller must hold
// state.mu for writing, and must call it before releasing it.
func (b *PodBlacklist) syncCountLocked(state *podFailureState) {
	want := state.excludingLocked()
	if want == state.counted {
		return
	}
	state.counted = want
	if want {
		b.excluded.Add(1)
	} else {
		b.excluded.Add(-1)
	}
}

// entry returns the breaker entry stored under key, creating it on first use. Every
// creation goes through here so tracked stays equal to the number of entries.
func (b *PodBlacklist) entry(key string) any {
	if v, ok := b.pods.Load(key); ok {
		return v
	}
	v, loaded := b.pods.LoadOrStore(key, &podFailureState{})
	if !loaded {
		b.tracked.Add(1)
	}
	return v
}

// checkPodHealthyWithin reports whether the pod at addr answers its health
// check, waiting up to timeout for the real HTTP probe; a test hook ignores
// the timeout.
func (b *PodBlacklist) checkPodHealthyWithin(addr, engine string, timeout time.Duration) bool {
	if b.HealthCheck != nil {
		return b.HealthCheck(addr)
	}
	if !engineServesHealthRoute(engine) {
		return httpListenerCheckWithin(addr, timeout)
	}
	return httpHealthCheckWithin(addr, timeout)
}

// engineServesHealthRoute reports whether engine is known to answer GET /health
// with a 2xx while it is up, so that a 2xx can be required of it: vLLM and SGLang,
// the engines whose model list is known too (engineListsModels). Any other engine,
// and a pod with no engine label, is only required to answer at all
// (httpListenerCheckWithin). xLLM answers 404 on /health while serving, xDiT has no
// such route, and a pod whose engine is not known must not be judged dead, and
// blocked for an hour, for a route it may not have. Like the identity check, an
// engine joins the list once its /health has been verified.
func engineServesHealthRoute(engine string) bool {
	return engineListsModels(engine)
}

// ProbeResult is the verdict of the status-independent pod check (see probe).
type ProbeResult int

const (
	// ProbeHealthy: GET /health is 2xx and, where the identity check applies,
	// GET /v1/models is a JSON list naming the model.
	ProbeHealthy ProbeResult = iota
	// ProbeDegraded: the pod answers but does not look like the engine for the
	// model, e.g. GET /v1/models is not 2xx or is a valid list without the model.
	// That can be transient, so the sweep only gives it the ordinary backoff.
	ProbeDegraded
	// ProbeDead: GET /health failed, or GET /v1/models answered 2xx with an
	// empty or non-JSON body, which is another process on the address. The
	// sweep blacklists such a pod for podProbeDeadBlacklistDuration.
	ProbeDead
)

// probe runs the status-independent check of a pod and returns the verdict and,
// when it is not healthy, what failed: GET /health must be 2xx and, for an
// engine that lists its models (engineListsModels) with a known model, GET
// /v1/models must return a JSON list naming it. It is the definition of
// "healthy" the background sweep uses. timeout is how long each of its HTTP
// requests may take.
func (b *PodBlacklist) probe(addr, model, engine string, timeout time.Duration) (ProbeResult, string) {
	if !b.checkPodHealthyWithin(addr, engine, timeout) {
		return ProbeDead, "GET " + podHealthCheckPath + " failed"
	}
	if model == "" || !engineListsModels(engine) {
		return ProbeHealthy, ""
	}
	notListed := "GET " + podModelsCheckPath + " did not return a JSON list naming " + model
	if b.IdentityCheck != nil {
		// A test hook answers only yes or no, so it cannot say the body was not JSON.
		if b.IdentityCheck(addr, model) {
			return ProbeHealthy, ""
		}
		return ProbeDegraded, notListed
	}
	switch fetchModelListWithin(addr, model, timeout) {
	case modelListServed:
		return ProbeHealthy, ""
	case modelListUnparseable:
		return ProbeDead, "GET " + podModelsCheckPath + " answered 2xx with an empty or non-JSON body"
	default:
		return ProbeDegraded, notListed
	}
}

// engineListsModels reports whether engine is known to answer GET /v1/models
// with an OpenAI-style model list, so that list can identify the engine.
// Unknown or unlabeled engines are left to the /health check alone rather than
// risk blacklisting a pod whose /v1/models has another shape.
//
// GET /health only proves that some process answers on the address. Under host
// networking a port is recycled, so a stale registration can point at an
// unrelated listener that answers /health 200 while serving nothing. For an
// engine that lists its models the model list is the identity check.
func engineListsModels(engine string) bool {
	switch strings.ToLower(engine) {
	case metrics.EngineNameVLLM, metrics.EngineNameSGLang:
		return true
	}
	return false
}

// podHealthHTTPClient has no timeout of its own: each probe sets its deadline
// on the request context, which also bounds reading the body. It has a transport
// of its own, with no proxy: http.DefaultTransport would honour HTTP_PROXY and
// HTTPS_PROXY, and a proxy that cannot reach the pod network answers every probe
// with an error that looks exactly like a dead pod, which the sweep would turn
// into an hour-long block on every pod. A probe is rare per pod, so it keeps no
// connection alive either.
var podHealthHTTPClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func httpHealthCheckWithin(addr string, timeout time.Duration) bool {
	return httpGetHealthWithin(addr, timeout, func(status int) bool { return status >= 200 && status < 300 })
}

// httpListenerCheckWithin is httpHealthCheckWithin for an engine with no
// /health route: any HTTP response, whatever its status, shows a server is
// listening. Like a 2xx /health it cannot tell the engine from another process
// that took over a recycled address; that engine has no identity check either.
func httpListenerCheckWithin(addr string, timeout time.Duration) bool {
	return httpGetHealthWithin(addr, timeout, func(int) bool { return true })
}

func httpGetHealthWithin(addr string, timeout time.Duration, healthy func(status int) bool) bool {
	if addr == "" {
		return true // fail open: no address to probe
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+podHealthCheckPath, nil)
	if err != nil {
		return true
	}
	resp, err := podHealthHTTPClient.Do(req)
	if err != nil {
		return false // unreachable
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return healthy(resp.StatusCode)
}

// modelListState is what GET /v1/models says about a model.
type modelListState int

const (
	// modelListServed: a JSON list naming the model. Also the answer when there
	// is nothing to probe (empty address or model), which fails open.
	modelListServed modelListState = iota
	// modelListNotServed: transport error, non-2xx, or a JSON list without the model.
	modelListNotServed
	// modelListUnparseable: 2xx, but the body is empty or not the JSON list.
	modelListUnparseable
)

// isMalformedModelList reports whether err from decoding a 2xx /v1/models body means the
// body that arrived is not the JSON model list: empty, or not that JSON. An error from
// reading it, or one that came with the request's wait running out, is not.
func isMalformedModelList(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	return errors.Is(err, io.EOF) || errors.As(err, &syntax) || errors.As(err, &typed)
}

// fetchModelListWithin reports what GET addr/v1/models says about model, waiting
// up to timeout for the whole response. An empty address or model fails open.
func fetchModelListWithin(addr, model string, timeout time.Duration) modelListState {
	if addr == "" || model == "" {
		return modelListServed
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+podModelsCheckPath, nil)
	if err != nil {
		return modelListServed
	}
	resp, err := podHealthHTTPClient.Do(req)
	if err != nil {
		return modelListNotServed
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return modelListNotServed
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		if !isMalformedModelList(ctx, err) {
			// The answer did not finish arriving (the wait ran out, the connection broke):
			// that says nothing about what is listening, so it is not "another process".
			return modelListNotServed
		}
		return modelListUnparseable
	}
	for _, m := range body.Data {
		if m.ID == model {
			return modelListServed
		}
	}
	return modelListNotServed
}

// podTarget is the ip:port and model requests to pod use, for log lines. An
// empty addr or model is derived from the pod, like the sweep derives its
// probe target.
func podTarget(pod *v1.Pod, addr, model string) (string, string) {
	if model == "" {
		model = pod.Labels[constants.ModelLabelName]
	}
	if addr == "" {
		addr = types.PodTargetAddress(pod, model, "")
	}
	return addr, model
}

// podDesc names pod in a log line: its key plus the ip:port and model it is
// reached on, so a line can be matched to a curl command.
func podDesc(pod *v1.Pod, addr, model string) string {
	addr, model = podTarget(pod, addr, model)
	return fmt.Sprintf("%s (addr=%s, model=%s)", PodKey(pod), addr, model)
}

// PodLogFields is podDesc for structured log lines: pod key, ip:port and model as
// separate key/value pairs, so callers outside this package name a pod the way the
// blacklist's own lines do. A nil pod yields no fields, like the other PodBlacklist
// APIs that accept nil.
func PodLogFields(pod *v1.Pod) []interface{} {
	if pod == nil {
		return nil
	}
	addr, model := podTarget(pod, "", "")
	return []interface{}{"pod", PodKey(pod), "addr", addr, "model", model}
}

func backoffDuration(tripCount int) time.Duration {
	if tripCount < 1 {
		tripCount = 1
	}
	const maxShift = 6 // 2^6 == 64x base already exceeds the cap below
	shift := min(tripCount-1, maxShift)
	d := podBlacklistBaseDuration * time.Duration(int64(1)<<uint(shift))
	if d > podBlacklistMaxDuration {
		d = podBlacklistMaxDuration
	}
	return d
}

// Allow reports whether pod may be a routing candidate: not load-excluded, not
// open with a block that has yet to end, and not blocked by request failures with a
// block that has yet to end. A pod whose block has ended is a candidate again, with
// no probe request; the background sweep (or, for a request block, its probation)
// decides whether it is excluded afresh. An entry observed on another pod UID does
// not apply. It only reads: it takes the entry's read lock and does
// not count as a touch, so it does not keep an entry from aging out.
func (b *PodBlacklist) Allow(pod *v1.Pod) bool {
	if !Enabled || pod == nil {
		return true
	}
	key := PodKey(pod)
	v, ok := b.pods.Load(key)
	if !ok {
		return true
	}
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; treating as allowed", v, podDesc(pod, "", ""))
		return true
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.uid != string(pod.UID) || !state.blocksLocked(time.Now())
}

// IsBlacklisted is a read-only exclusion check (open breaker, request-failure
// block or load-excluded). It and Allow answer from the same deadline-aware check
// (blocksLocked): only a pod that is load-excluded, or open or request-blocked
// before its deadline, is reported, and a pod whose block has ended is not.
func (b *PodBlacklist) IsBlacklisted(pod *v1.Pod) bool {
	if !Enabled || pod == nil {
		return false
	}
	key := PodKey(pod)
	v, ok := b.pods.Load(key)
	if !ok {
		return false
	}
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; treating as not blacklisted", v, podDesc(pod, "", ""))
		return false
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.uid == string(pod.UID) && state.blocksLocked(time.Now())
}

// IsQuarantined reports whether pod is inside the one-hour block the probe
// sweep gives a pod it found dead (see RecordProbeResult). Such a pod is not
// only kept out of routing: the cache also stops scraping it and emitting its
// series, since there is nothing to see until the hour ends. An ordinary
// backoff, a load exclusion or a flaky re-add exclusion is not quarantine.
// It satisfies cache.PodQuarantineChecker.
func (b *PodBlacklist) IsQuarantined(pod *v1.Pod) bool {
	if !Enabled || pod == nil {
		return false
	}
	v, ok := b.pods.Load(PodKey(pod))
	if !ok {
		return false
	}
	state, ok := v.(*podFailureState)
	if !ok {
		return false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.uid == string(pod.UID) && state.state == circuitOpen && time.Now().Before(state.quarantinedUntil)
}

// BlacklistReason returns a short human-readable explanation for why pod is
// currently excluded from routing, or "" if it isn't excluded at all. Mirrors
// the exclusion checks in IsBlacklisted; a pod can be excluded for both
// reasons at once (e.g. flagged as a load outlier while also mid-backoff from
// failed background probes), in which case both are reported.
func (b *PodBlacklist) BlacklistReason(pod *v1.Pod) string {
	if pod == nil {
		return ""
	}
	key := PodKey(pod)
	v, ok := b.pods.Load(key)
	if !ok {
		return ""
	}
	state, ok := v.(*podFailureState)
	if !ok {
		return ""
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.uid != string(pod.UID) {
		return ""
	}

	var reasons []string
	if state.loadExcluded {
		reasons = append(reasons, "load outlier: running+waiting request count is a statistical outlier among its replicas")
	}
	if state.state == circuitOpen && time.Now().Before(state.openUntil) {
		if state.openReason != "" {
			reasons = append(reasons, state.openReason)
		} else {
			reasons = append(reasons, "circuit breaker open")
		}
	}
	if time.Now().Before(state.requestBlockedUntil) {
		reasons = append(reasons, state.requestBlockReason)
	}
	return strings.Join(reasons, "; ")
}

// BlacklistReasons returns one "<pod name>: <reason>" entry per pod in pods
// that is currently excluded, in the same relative order -- for logging why
// pods were excluded. A pod with no active exclusion (e.g. its breaker
// recovered since the caller last looked) is simply omitted.
func (b *PodBlacklist) BlacklistReasons(pods []*v1.Pod) []string {
	reasons := make([]string, 0, len(pods))
	for _, pod := range pods {
		reason := b.BlacklistReason(pod)
		if reason == "" {
			continue
		}
		reasons = append(reasons, pod.Name+": "+reason)
	}
	return reasons
}

// SetLoadExcluded toggles load-outlier exclusion. Separate from the open/closed
// breaker: the next sweep is the recovery check. Clearing a never-excluded
// pod does not create a map entry.
//
// Returns whether this call actually changed the exclusion state. The caller
// (sweepLoadOutliers) uses this to attach richer, cache-derived context to the
// un-blacklist transition specifically -- SetLoadExcluded itself has no cache
// reference and stays a plain, dependency-free breaker so it's cheap to construct
// in tests.
func (b *PodBlacklist) SetLoadExcluded(pod *v1.Pod, excluded bool) (changed bool) {
	if !Enabled || pod == nil || ignoredPod(pod) {
		return false
	}
	key := PodKey(pod)

	if !excluded {
		v, ok := b.pods.Load(key)
		if !ok {
			return false
		}
		state, ok := v.(*podFailureState)
		if !ok {
			return false
		}
		state.mu.Lock()
		state.adoptLocked(pod)
		changed = state.loadExcluded
		state.loadExcluded = false
		state.lastUpdated = time.Now()
		b.syncCountLocked(state)
		state.mu.Unlock()
		if changed {
			emitRecovery(pod, recoverLoadNormal)
		}
		return changed
	}

	v := b.entry(key)
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return false
	}
	state.mu.Lock()
	state.adoptLocked(pod)
	changed = !state.loadExcluded
	state.loadExcluded = true
	state.lastUpdated = time.Now()
	b.syncCountLocked(state)
	state.mu.Unlock()
	if changed {
		emitExclusion(pod, excludeLoadOutlier)
		klog.Warningf("pod %s blacklisted: running+waiting request count is a statistical outlier among its replicas", podDesc(pod, "", ""))
	}
	return changed
}

// OnPodFlakyReAdd implements cache.PodLifecycleListener. It proactively opens the
// pod's breaker for flakyReAddOpenDuration without confirming anything: a pod that
// reappeared after being absent is kept out of routing briefly, then admitted again.
// tripCount is deliberately left untouched: this isn't a confirmed failure, so it
// neither escalates nor resets the backoff of an unrelated earlier block.
//
// The breaker is otherwise unaffected by the informer's delete/re-add cycle (it's
// keyed by pod name, independent of the cache's *Pod object identity, and the
// re-added pod usually has the same UID), so a pod already open here means it was
// excluded for an unrelated reason (e.g. a probe sweep blocked it when this flap
// happened) -- this only extends openUntil if the new deadline is later. A pod
// with a new UID (recreated on the same IP, e.g. with host networking) starts
// over (adoptLocked).
func (b *PodBlacklist) OnPodFlakyReAdd(pod *v1.Pod) {
	if !Enabled || pod == nil || ignoredPod(pod) {
		return
	}
	key := PodKey(pod)
	v := b.entry(key)
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return
	}

	state.mu.Lock()
	state.adoptLocked(pod)
	now := time.Now()
	state.lastUpdated = now
	state.lapseLocked(now)
	newOpenUntil := now.Add(flakyReAddOpenDuration)
	changed, newlyExcluded := false, false
	switch state.state {
	case circuitClosed:
		state.state = circuitOpen
		state.openUntil = newOpenUntil
		state.openReason = "flaky delete/re-add reappearance"
		changed, newlyExcluded = true, true
	case circuitOpen:
		if newOpenUntil.After(state.openUntil) {
			state.openUntil = newOpenUntil
			changed = true
		}
	}
	b.syncCountLocked(state)
	state.mu.Unlock()

	// Logged only on an actual state change -- not on every call (e.g. circuitOpen
	// where the existing window already outlasts this one) -- both to avoid noise
	// and because PodLifecycleListener's contract (called synchronously from the
	// informer event handler) means this I/O must stay off the hot path as much
	// as possible even though c.mu itself is already released by the time this
	// listener runs (see addPod/updatePod in pkg/cache/informers.go).
	if newlyExcluded {
		emitExclusion(pod, excludeFlakyReAdd)
	}
	if changed {
		addr, model := podTarget(pod, "", "")
		klog.InfoS("pod briefly excluded after a flaky delete/re-add",
			"pod", key, "addr", addr, "model", model, "duration", flakyReAddOpenDuration)
	}
}

// IsPodFailureStatus reports whether an HTTP status from the upstream, as the gateway sees it
// in the response headers, counts against the pod that served it: 502, 503 or 504. Those are the
// statuses that say the pod, or something between the gateway and the engine, could not serve
// the request: an engine that is shedding or has lost its backend, a sidecar or proxy in front of
// it that cannot reach it, a request that timed out.
//
// Nothing else counts:
//   - A bare 500 is the status engines return for a request-induced exception (a guided-decoding
//     or grammar bug, a multimodal decode failure, a tool-call parser error). One tenant's
//     deterministic 500s, retried onto the same pod by prefix-cache routing, would otherwise
//     eject pods that serve everyone else fine. An engine that answers every request with 500
//     while /health passes is therefore not found by this source.
//   - A 4xx is the request's fault, not the pod's: a pod that answers 400 is alive.
//   - A 429 is load, which every replica of the model shares, and ejecting for it would push the
//     traffic onto the pods that are least able to take it.
//   - A 501 says the engine does not implement the route, a configuration matter for every request.
//   - How a stream ends does not count either (see RecordResponse).
//
// Only statuses that reach the gateway can count, and the ones Envoy generates itself do not on
// the pinned Envoy: it skips ext_proc for its own local replies (a connection refused or reset
// before any response, a timeout), so those 503s and 504s never arrive here. See the README.
func IsPodFailureStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// RecordResponse applies the status of an upstream response received from pod, the pod a
// request was routed to. It reports whether this response blocked the pod.
//
// It does nothing unless the master switch is on and AIBRIX_POD_BLACKLIST_REQUEST_FAILURE_THRESHOLD
// is above 0 (it is 0 by default).
//
// A response with a failing status (IsPodFailureStatus) adds to the pod's count of failures in
// a row; any other response starts that count over, and so does a pause of more than
// requestFailureStreakGap between two failures. When the count reaches the threshold the pod is
// blocked for the backoff (backoffDuration: 1 minute, doubling per block, at most 10 minutes).
// The block ends on its deadline, with no probe request, and the pod is then on probation: one
// more failure blocks it again, with the next backoff, until podRequestRecoverySuccesses
// responses in a row that are not failures end it, or requestTripDecay passes without a block. A
// healthy probe sweep neither lifts a request block nor shortens the backoff.
//
//   - A response that arrives while the pod is already excluded, by any source, is ignored. Those
//     are requests that were in flight when the block began, and counting them would stack
//     blocks for one incident, or, for the successes, end a probation that has not started.
//   - Only a failure creates an entry. A success looks for one and returns when there is none,
//     so a healthy fleet allocates nothing and FilterBlacklisted stays on its no-lookup path.
//   - Nothing here looks at how a stream ends. A client that gives up, an ext_proc stream
//     closed with EOF or cancelled, and a request that never got headers are not recorded: the
//     gateway cannot tell them from an upstream failure, as Envoy half-closes and resets the
//     stream together when a downstream client leaves.
//   - A pod the blacklist ignores (see ignoredPod) is never recorded.
//
// The block is soft like every other: routing falls back to the pods it excluded when too few
// are left (see ExclusionOverCap). Nothing is probed on the request path.
func (b *PodBlacklist) RecordResponse(pod *v1.Pod, status int) (blocked bool) {
	threshold := b.requestThreshold()
	if !Enabled || threshold <= 0 || pod == nil {
		return false
	}
	failed := IsPodFailureStatus(status)
	if !failed && b.tracked.Load() == 0 {
		// Nothing is tracked, so there is no count to start over, and a success must not make
		// an entry. This is a healthy fleet: no key is built, nothing is allocated.
		return false
	}
	key := PodKey(pod)

	var v any
	if failed {
		if ignoredPod(pod) {
			return false
		}
		v = b.entry(key)
	} else {
		var ok bool
		if v, ok = b.pods.Load(key); !ok {
			return false
		}
	}
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return false
	}

	if !failed {
		// Most entries have nothing to start over; look before taking the write lock.
		state.mu.RLock()
		idle := state.requestFailures == 0 && state.requestTrips == 0
		state.mu.RUnlock()
		if idle {
			return false
		}
	}

	var failures, trips int
	var duration time.Duration
	var probation, ignored bool
	state.mu.Lock()
	func() {
		defer state.mu.Unlock()
		defer b.syncCountLocked(state) // deferred after the unlock, so it runs before it
		state.adoptLocked(pod)
		now := time.Now()
		state.lapseLocked(now)
		if state.blocksLocked(now) {
			ignored = true
			return
		}
		state.expireRequestLocked(now)
		state.lastUpdated = now
		if !failed {
			state.requestFailures = 0
			if state.requestTrips > 0 {
				state.requestSuccesses++
				if state.requestSuccesses >= podRequestRecoverySuccesses {
					state.requestTrips, state.requestSuccesses = 0, 0
				}
			}
			return
		}
		state.requestFailures++
		state.lastRequestFailure = now
		state.requestSuccesses = 0
		limit := threshold
		if state.requestTrips > 0 {
			limit = 1 // probation
		}
		if state.requestFailures < limit {
			return
		}
		probation = state.requestTrips > 0
		failures = state.requestFailures
		state.requestTrips++
		trips = state.requestTrips
		duration = backoffDuration(trips)
		state.requestBlockedUntil = now.Add(duration)
		state.requestTripEnd = state.requestBlockedUntil
		state.requestFailures = 0
		state.requestBlockReason = fmt.Sprintf("%d upstream failures in a row, last status %d; blocked for %s (request trip #%d)", failures, status, duration, trips)
		blocked = true
	}()

	if failed {
		outcome := failureCounted
		if ignored {
			outcome = failureIgnored
		}
		emitRequestFailure(pod, status, outcome)
	}
	if blocked {
		reason := excludeRequestFailures
		if probation {
			reason = excludeRequestFailuresProbation
		}
		emitExclusion(pod, reason)
		klog.Warningf("pod %s blacklisted for %s: %d upstream failures in a row, last status %d (request trip #%d)",
			podDesc(pod, "", ""), duration, failures, status, trips)
	}
	return blocked
}

// recordHealthySweep applies a healthy sweep to state, the entry stored for pod. It starts
// the failure counts over and, once a block has ended, the backoff too. Healthy sweeps in
// a row lift a block that is still running (podProbeRecoveryIterations of them), which
// ends a quarantine as well.
func (b *PodBlacklist) recordHealthySweep(pod *v1.Pod, key string, state *podFailureState) {
	state.mu.Lock()
	state.adoptLocked(pod)
	now := time.Now()
	state.probeFailures = 0
	state.probeDeadSweeps = 0
	state.lapseLocked(now)
	healthySweeps := 0
	lifted := false
	if state.state == circuitClosed {
		state.tripCount = 0
		state.probeHealthySweeps = 0
	} else {
		state.probeHealthySweeps++
		healthySweeps = state.probeHealthySweeps
		if state.probeHealthySweeps >= podProbeRecoveryIterations {
			state.state = circuitClosed
			state.openUntil = now
			state.quarantinedUntil = time.Time{}
			state.tripCount = 0
			state.probeHealthySweeps = 0
			state.lastUpdated = now
			lifted = true
		}
	}
	b.syncCountLocked(state)
	state.mu.Unlock()
	if lifted {
		emitRecovery(pod, recoverProbeHealthy)
		addr, model := podTarget(pod, "", "")
		klog.InfoS("pod un-blacklisted: background probes found it healthy",
			"pod", key, "addr", addr, "model", model, "healthySweeps", healthySweeps)
	}
}

// RecordProbeResult applies one background-sweep probe outcome and reports
// whether it blacklisted the pod (or renewed its blacklisting). failure says
// what failed and goes into the blacklist reason.
//
//   - A pod found dead (ProbeDead) podProbeFailureIterations sweeps in a row is
//     blacklisted for podProbeDeadBlacklistDuration, whatever state its breaker
//     is in: closed, or already open from an earlier sweep or a flaky re-add. An
//     open pod's deadline is only ever extended, never shortened. While it stays
//     dead each further run of dead sweeps renews the hour. When the hour ends
//     the pod is admitted again, and one that is still dead is blacklisted anew
//     after another podProbeFailureIterations dead sweeps.
//   - A pod that is merely degraded (ProbeDegraded) podProbeFailureIterations
//     sweeps in a row, while its breaker is closed (an open one whose block has
//     ended counts as closed), is tripped with the ordinary backoff.
//   - A healthy probe starts both counts over, and once a block has ended it
//     starts the backoff over too. A block that is still running is lifted by
//     podProbeRecoveryIterations healthy sweeps in a row, so a pod that is
//     answering correctly is not kept out for the rest of an hour.
func (b *PodBlacklist) RecordProbeResult(pod *v1.Pod, result ProbeResult, failure string) (blacklisted bool) {
	if !Enabled || pod == nil || ignoredPod(pod) {
		return false
	}
	key := PodKey(pod)
	if result == ProbeHealthy {
		if v, ok := b.pods.Load(key); ok {
			if state, ok := v.(*podFailureState); ok {
				b.recordHealthySweep(pod, key, state)
			}
		}
		return false
	}

	v := b.entry(key)
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return false
	}
	exclusion := "" // set when this verdict newly excludes the pod; counted once the lock is released
	defer func() {
		if exclusion != "" {
			emitExclusion(pod, exclusion)
		}
	}()
	state.mu.Lock()
	defer state.mu.Unlock()
	defer b.syncCountLocked(state) // deferred after the unlock, so it runs before it
	state.adoptLocked(pod)
	now := time.Now()
	state.lastUpdated = now
	state.lapseLocked(now)
	state.probeHealthySweeps = 0
	wasExcluded := state.state == circuitOpen && now.Before(state.openUntil)

	if result == ProbeDead {
		state.probeDeadSweeps++
	} else {
		state.probeDeadSweeps = 0
	}
	if state.state == circuitClosed {
		state.probeFailures++
	} else {
		state.probeFailures = 0
	}

	if state.probeDeadSweeps >= podProbeFailureIterations {
		until := now.Add(podProbeDeadBlacklistDuration)
		if state.state == circuitClosed {
			state.tripCount++
		}
		if state.state != circuitOpen || until.After(state.openUntil) {
			state.state = circuitOpen
			state.openUntil = until
		}
		if until.After(state.quarantinedUntil) {
			state.quarantinedUntil = until
		}
		state.probeFailures = 0
		state.probeDeadSweeps = 0
		if !wasExcluded {
			exclusion = excludeProbeDead
		}
		state.openReason = fmt.Sprintf("background probe found it dead %d sweeps in a row (%s); blacklisted for %s", podProbeFailureIterations, failure, podProbeDeadBlacklistDuration)
		klog.Warningf("pod %s blacklisted for %s: background probe found it dead %d sweeps in a row (%s)", podDesc(pod, "", ""), podProbeDeadBlacklistDuration, podProbeFailureIterations, failure)
		return true
	}

	if state.state != circuitClosed {
		// Already excluded, so only a dead verdict can matter. Log it as the count
		// builds: an excluded pod would otherwise say nothing until the sweep that
		// blacklists it for an hour, and cannot be told from one the sweep skips.
		if result == ProbeDead {
			addr, model := podTarget(pod, "", "")
			klog.InfoS("excluded pod failed a background probe as dead; it will be re-checked on the next sweep",
				"pod", key, "addr", addr, "model", model, "deadSweeps", state.probeDeadSweeps, "blacklistAfter", podProbeFailureIterations, "reason", failure)
		}
		return false
	}
	if state.probeFailures < podProbeFailureIterations {
		addr, model := podTarget(pod, "", "")
		klog.InfoS("pod failed a background probe; it will be re-checked on the next sweep",
			"pod", key, "addr", addr, "model", model, "failure", state.probeFailures, "blacklistAfter", podProbeFailureIterations, "reason", failure)
		return false
	}
	state.tripCount++
	state.state = circuitOpen
	state.openUntil = now.Add(backoffDuration(state.tripCount))
	state.probeFailures = 0
	exclusion = excludeProbeDegraded
	state.openReason = fmt.Sprintf("background probe failed %d sweeps in a row (%s, trip #%d)", podProbeFailureIterations, failure, state.tripCount)
	klog.Warningf("pod %s blacklisted for %s: background probe failed %d sweeps in a row (%s) (trip #%d)", podDesc(pod, "", ""), backoffDuration(state.tripCount), podProbeFailureIterations, failure, state.tripCount)
	return true
}

// HasExcluded reports whether any tracked entry can currently exclude its pod
// (load-excluded, open breaker, or request-failure block). It reads the same
// atomic counter FilterBlacklisted uses to skip per-pod lookups on a healthy
// fleet: false guarantees no pod is excluded, while true means callers should
// consult FilterBlacklisted or Allow.
func (b *PodBlacklist) HasExcluded() bool {
	return Enabled && b.excluded.Load() > 0
}

// FilterBlacklisted returns the pods Allow admits, in order. While no entry can exclude
// its pod it returns pods itself, so the common case costs no lookup and no allocation;
// and when it does look, it allocates only if it finds a pod to drop. Callers must not
// modify the result in place.
func (b *PodBlacklist) FilterBlacklisted(pods []*v1.Pod) []*v1.Pod {
	if !b.HasExcluded() {
		return pods
	}
	var out []*v1.Pod // allocated at the first pod to drop
	for i, pod := range pods {
		if b.Allow(pod) {
			if out != nil {
				out = append(out, pod)
			}
			continue
		}
		if out == nil {
			out = make([]*v1.Pod, i, len(pods))
			copy(out, pods[:i])
		}
	}
	if out == nil {
		return pods
	}
	return out
}

// StartSweep starts the goroutine that prunes breaker entries that have not
// been touched for podBlacklistEntryTTL.
func (b *PodBlacklist) StartSweep(stopCh <-chan struct{}) {
	if !Enabled {
		return
	}
	ticker := time.NewTicker(podBlacklistSweepInterval)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				klog.Errorf("PodBlacklist: sweep goroutine panic recovered: %v", r)
			}
		}()
		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							klog.Errorf("PodBlacklist: panic recovered during sweep: %v", r)
						}
					}()
					b.sweepStale(time.Now())
					b.publishExcludedPods()
				}()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

func (b *PodBlacklist) sweepStale(now time.Time) {
	b.pods.Range(func(key, value any) bool {
		state, ok := value.(*podFailureState)
		if !ok {
			return true
		}
		state.mu.Lock()
		// Healthy probes do not refresh lastUpdated, so a pod that recovers
		// while blacklisted would otherwise lose its entry, and the rest of
		// its block, 30 minutes into an hour.
		last := state.lastUpdated
		if state.openUntil.After(last) {
			last = state.openUntil
		}
		// requestTripEnd is the end of the latest request block (never earlier than
		// requestBlockedUntil), kept after the block lapses, so the probation it leaves behind
		// outlives its block by the full TTL.
		if state.requestTripEnd.After(last) {
			last = state.requestTripEnd
		}
		// The delete happens under state.mu: a writer that refreshes the entry takes the
		// same lock, so it either lands before this check and keeps the entry alive, or
		// after the delete. CompareAndDelete removes only the state that was just
		// checked, never a fresh one created under the same key in the meantime. A
		// writer that had already loaded the pointer and is still waiting for the lock
		// updates a state that is no longer stored; an entry that has been idle for the
		// whole TTL holds nothing worth keeping, and the next event recreates it.
		if now.Sub(last) > podBlacklistEntryTTL && b.pods.CompareAndDelete(key, value) {
			b.tracked.Add(-1)
			state.removed = true
			b.syncCountLocked(state)
		} else {
			// A block that has ended still counts toward excluded, which keeps every request
			// off FilterBlacklisted's no-lookup path. The sweeps close it when they next
			// report on the pod, but a pod that has left the fleet is no longer probed and
			// would otherwise stay counted until the entry's own TTL runs out. Closing it
			// here is what the next writer would do; no reader's answer changes, since Allow
			// and IsBlacklisted already look at the deadline and quarantinedUntil never
			// outlasts openUntil.
			state.lapseLocked(now)
			b.syncCountLocked(state)
		}
		state.mu.Unlock()
		return true
	})
}
