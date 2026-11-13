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
	// failure breaker, probe sweep, load-outlier sweep, flaky re-add and
	// quarantine. Unset, empty, false or 0 leaves every path a no-op so a
	// healthy fleet matches pre-blacklist routing.
	EnvPodBlacklistEnabled = "AIBRIX_POD_BLACKLIST_ENABLED"

	// FailureThreshold failures within podBlacklistFailureWindow
	// trips the breaker (subject to confirmHealthy). The window does not
	// reset on an interleaved success — see isFailureStatus / RecordResult.
	FailureThreshold          = 3
	podBlacklistFailureWindow = 30 * time.Second
	podBlacklistBaseDuration  = 1 * time.Minute
	podBlacklistMaxDuration   = 10 * time.Minute
	// Claimed half-open probe expires if RecordResult never arrives, so a hung
	// probe cannot exclude the pod forever.
	podBlacklistProbeTimeout = 30 * time.Second
	// An entry is pruned once it has been untouched this long, counted from the
	// later of its last update and its openUntil (sweepStale), so a live backoff
	// or one-hour block is never pruned however quiet the pod is.
	podBlacklistEntryTTL      = 30 * time.Minute
	podBlacklistSweepInterval = 5 * time.Minute
	podHealthCheckPath        = "/health"
	podModelsCheckPath        = "/v1/models"
	podHealthCheckTimeout     = 3 * time.Second
	// flakyReAddOpenDuration is how long a pod is proactively excluded from
	// routing after OnPodFlakyReAdd fires -- i.e. it reappeared in the cache
	// after being genuinely absent for a bit (see flakyReAddMinObservedGap in
	// pkg/cache/informers.go), with no confirmation it's actually healthy
	// again. Deliberately much shorter than podBlacklistBaseDuration: this
	// isn't a confirmed failure, just an unconfirmed reappearance, so the pod
	// shouldn't sit out of rotation for long once the first real request
	// after it (the half-open probe) confirms it's fine.
	flakyReAddOpenDuration = 3 * time.Second
	// podProbeSweepInterval is how often the background sweep probes every
	// routable pod (see startPodProbeSweep).
	podProbeSweepInterval = 300 * time.Second
	// podProbeFailureIterations is how many consecutive sweeps a pod must fail
	// before the sweep blacklists it. A failed pod is simply probed again on
	// the next sweeps, so one blip (a probe timing out on a busy pod) does not
	// exclude it. Set to 1 to blacklist on the first failed sweep.
	podProbeFailureIterations = 3
	// podProbeDeadBlacklistDuration is how long the sweep blacklists a pod it
	// found dead: GET /health failing, or GET /v1/models answering 2xx with a
	// body that is not a JSON model list (see ProbeDead). It is deliberately
	// far longer than the request-driven backoff, so a stale registration is
	// not handed a real half-open request every few minutes. Every further
	// podProbeFailureIterations dead sweeps renew the hour, so a pod that stays
	// dead stays out.
	podProbeDeadBlacklistDuration = time.Hour
	// podProbeConcurrency bounds the probes in flight during one sweep.
	podProbeConcurrency = 16
	// podProbeTimeout is how long the sweep waits for each request of a probe.
	// It is longer than podHealthCheckTimeout, which a tripping request waits
	// out synchronously: nothing waits on the sweep, and a pod that is merely
	// slow to answer must not be judged dead. GET /health on the fleet's sglang
	// pods takes 1.4-1.7 s, which the sweep's earlier 1 s timeout turned into a
	// blacklisting for an hour.
	podProbeTimeout = 5 * time.Second
)

// Enabled is the process-wide master switch for the pod blacklist. It is loaded
// once from AIBRIX_POD_BLACKLIST_ENABLED (default false). When false, every
// exported method is a no-op and the background sweeps never start, so routing
// matches the pre-blacklist behaviour. Tests that exercise the feature set it
// to true (see TestMain).
var Enabled = utils.LoadEnvBool(EnvPodBlacklistEnabled, false)

// StatusStreamClosed is passed to RecordResult when Envoy's ext_proc
// stream closed before any ResponseHeaders arrived and the client was still
// connected (see handleRecvError).
const StatusStreamClosed = -1

// StatusIncompleteStream is passed to RecordResult when Envoy's ext_proc
// stream closed after the ResponseHeaders arrived fine (so HandleResponseHeaders
// already recorded a 200 for this request) but before the response completed,
// with the client still connected: the pod accepted the request and then
// dropped the connection mid-response. This is a distinct signal from
// StatusStreamClosed (which means no response was seen at all) and from the
// 200 already recorded at header time, so it needs its own RecordResult call
// rather than being folded into either (see recordUncompletedRecv).
const StatusIncompleteStream = -2

// isFailureStatus is true for any outcome that is not a 2xx: real upstream
// statuses (400, 404, 429, 5xx, ...) and the negative sentinels above alike.
// Unlike an HTTP status code, which reflects one request's outcome,
// isFailureStatus feeds the failure-rate window (see podBlacklistFailureWindow),
// so it deliberately does not carve out any status as "not worth counting" -
// a pod that is failing a lot, however it's failing, is what the window is
// for.
func isFailureStatus(statusCode int) bool {
	return statusCode < 200 || statusCode >= 300
}

func failureDesc(statusCode int) string {
	switch statusCode {
	case StatusStreamClosed:
		return "stream-closed-without-response"
	case StatusIncompleteStream:
		return "stream-cut-off-mid-generation"
	default:
		return fmt.Sprintf("HTTP %d", statusCode)
	}
}

type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

type podFailureState struct {
	mu    sync.Mutex
	state circuitState

	// failureTimestamps holds one entry per failure seen while closed, in
	// increasing time order. RecordResult prunes entries older than
	// podBlacklistFailureWindow on every call; a success is simply not
	// appended -- it does NOT clear the window (that's a deliberate policy
	// choice: a pod flaky enough to fail FailureThreshold times in 30s should trip even if
	// a few requests in between happened to succeed).
	failureTimestamps []time.Time
	tripCount         int // consecutive open cycles; drives backoff; reset on a successful probe
	openUntil         time.Time
	probeDeadline     time.Time
	// openReason is a human-readable explanation set whenever state transitions
	// to circuitOpen, surfaced via BlacklistReason. Stale once state leaves
	// circuitOpen, but BlacklistReason only ever reads it while state is still
	// circuitOpen, so it's never cleared on recovery.
	openReason string
	// Independent of the failure breaker; the next load-outlier sweep is the recovery check.
	loadExcluded bool
	// probeFailures counts consecutive background-sweep probes this pod has
	// failed while its breaker was closed; probeDeadSweeps counts consecutive
	// sweeps that found it dead (ProbeDead), in any breaker state. A passing
	// probe starts both over, a non-dead failure restarts probeDeadSweeps, and
	// probeFailures also restarts whenever the breaker is not closed.
	probeFailures   int
	probeDeadSweeps int
	// quarantinedUntil is when the one-hour block the sweep gives a dead pod
	// ends (zero for any other kind of exclusion). IsQuarantined reads it.
	quarantinedUntil time.Time
	lastUpdated      time.Time
}

// recordFailureLocked appends now to state.failureTimestamps and prunes
// entries older than podBlacklistFailureWindow, returning the resulting
// count. Caller must hold state.mu.
func recordFailureLocked(state *podFailureState, now time.Time) int {
	cutoff := now.Add(-podBlacklistFailureWindow)
	n := 0
	for _, t := range state.failureTimestamps {
		if t.After(cutoff) {
			state.failureTimestamps[n] = t
			n++
		}
	}
	state.failureTimestamps = append(state.failureTimestamps[:n], now)
	return len(state.failureTimestamps)
}

// PodBlacklist is a per-pod circuit breaker keyed by namespace/name.
// Zero value is ready to use.
type PodBlacklist struct {
	pods sync.Map // "namespace/name" -> *podFailureState
	// tracked is how many entries pods holds. FilterBlacklisted reads it to skip the
	// per-pod lookups while nothing is tracked, which is the case for a healthy fleet.
	tracked atomic.Int64

	// Probe hooks. Nil uses the real HTTP probes (httpHealthCheck,
	// httpEngineServesModel, httpEngineIsModelServer); tests set them so that
	// confirmation never makes a network call.
	HealthCheck   func(addr string) bool
	ModelCheck    func(addr, model string) bool
	IdentityCheck func(addr, model string) bool
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

func (b *PodBlacklist) checkPodHealthy(addr, engine string) bool {
	return b.checkPodHealthyWithin(addr, engine, podHealthCheckTimeout)
}

// checkPodHealthyWithin is checkPodHealthy with the wait for the real HTTP
// probe set by the caller; a test hook ignores it.
func (b *PodBlacklist) checkPodHealthyWithin(addr, engine string, timeout time.Duration) bool {
	if b.HealthCheck != nil {
		return b.HealthCheck(addr)
	}
	if !engineServesHealthRoute(engine) {
		return httpListenerCheckWithin(addr, timeout)
	}
	return httpHealthCheckWithin(addr, timeout)
}

// engineServesHealthRoute reports whether engine answers GET /health with a 2xx
// while it is up. xLLM does not: it answers 404 on /health on every port, so a
// 2xx requirement would judge every xLLM pod dead. For such an engine the probe
// settles for an HTTP server answering at all (httpListenerCheckWithin).
func engineServesHealthRoute(engine string) bool {
	return !strings.EqualFold(engine, metrics.EngineNameXLLM)
}

func (b *PodBlacklist) checkEngineIdentity(addr, model string) bool {
	check := b.IdentityCheck
	if check == nil {
		check = httpEngineIsModelServer
	}
	return check(addr, model)
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
// "healthy" shared by confirmHealthy and the background sweep. timeout is how
// long each of its HTTP requests may take.
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
// Unknown or unlabeled engines are left on the pre-identity confirmation path
// rather than risk blacklisting a pod whose /v1/models has another shape.
func engineListsModels(engine string) bool {
	switch strings.ToLower(engine) {
	case metrics.EngineNameVLLM, metrics.EngineNameSGLang:
		return true
	}
	return false
}

// isRouteRejectStatus is true for statuses a listener answers with when it
// does not serve the requested route or model. GET /health cannot disprove
// them: any process on the address can answer /health.
func isRouteRejectStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
}

// confirmHealthy is true for a false alarm (stay closed), false to trip.
//
// GET /health only proves that some process answers on addr. Under host
// networking a port is recycled, so a stale registration can point at an
// unrelated listener that answers /health 200 while failing every inference
// request. For an engine that lists its models (engineListsModels) the model
// list is the identity check: addr must serve one and it must name model.
//
//   - Route-reject statuses (404, 405, 501): only the identity check decides.
//   - Any other tracked status: /health AND the identity check. /health still
//     catches an engine core that died behind a live API server, which
//     /v1/models does not.
//
// The identity check is deliberately not applied when there is no model name
// to look for or the engine does not list models. There, 404 with a model is
// confirmed via the lenient model list (the routing cache is the same
// snapshot that selected this pod, so it is no use here), 405 trips on count
// alone, and everything else uses /health.
func (b *PodBlacklist) confirmHealthy(addr string, statusCode int, model, engine string) bool {
	if model != "" && engineListsModels(engine) {
		if isRouteRejectStatus(statusCode) {
			return b.checkEngineIdentity(addr, model)
		}
		result, _ := b.probe(addr, model, engine, podHealthCheckTimeout)
		return result == ProbeHealthy
	}
	if statusCode == http.StatusMethodNotAllowed {
		return false
	}
	if statusCode == http.StatusNotFound && model != "" {
		check := b.ModelCheck
		if check == nil {
			check = httpEngineServesModel
		}
		return check(addr, model)
	}
	return b.checkPodHealthy(addr, engine)
}

// podHealthHTTPClient has no timeout of its own: each probe sets its deadline
// on the request context, which also bounds reading the body.
var podHealthHTTPClient = &http.Client{}

func httpHealthCheck(addr string) bool {
	return httpHealthCheckWithin(addr, podHealthCheckTimeout)
}

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

// httpEngineServesModel reports whether GET addr/v1/models lists model.
// Empty addr/model or unparseable 2xx: fail open. Transport error or non-2xx: not served.
func httpEngineServesModel(addr, model string) bool {
	return engineServesModel(addr, model, true)
}

// httpEngineIsModelServer is httpEngineServesModel for an engine known to
// answer with an OpenAI-style list (engineListsModels): an empty or
// unparseable 2xx body is another listener on a recycled address, not this
// engine, so it counts as not served instead of failing open.
func httpEngineIsModelServer(addr, model string) bool {
	return engineServesModel(addr, model, false)
}

func engineServesModel(addr, model string, failOpenOnUnparseable bool) bool {
	switch fetchModelList(addr, model) {
	case modelListServed:
		return true
	case modelListUnparseable:
		return failOpenOnUnparseable
	default:
		return false
	}
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

func fetchModelList(addr, model string) modelListState {
	return fetchModelListWithin(addr, model, podHealthCheckTimeout)
}

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
		return modelListUnparseable
	}
	for _, m := range body.Data {
		if m.ID == model {
			return modelListServed
		}
	}
	return modelListNotServed
}

// podTarget is the ip:port and model requests to pod use, for log lines. The
// request path passes the address it routed to and the model it asked for; an
// empty addr or model (background paths have no request) is derived from the
// pod, like the sweep derives its probe target.
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

// RecordResult updates the breaker for one completed request.
// addr is the routed TargetAddress(); model and the pod's engine label decide
// how a trip is confirmed (see confirmHealthy).
// Confirmation runs without state.mu so Allow is not blocked for up to
// podHealthCheckTimeout; state is re-checked afterwards so a concurrent
// resolution is not overwritten.
func (b *PodBlacklist) RecordResult(pod *v1.Pod, addr string, statusCode int, model ...string) {
	if !Enabled || pod == nil {
		return
	}
	key := PodKey(pod)
	v := b.entry(key)
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return
	}
	modelName := ""
	if len(model) > 0 {
		modelName = model[0]
	}

	failed := isFailureStatus(statusCode)
	now := time.Now()

	state.mu.Lock()
	state.lastUpdated = now
	priorState := state.state
	needsConfirmation := false

	switch priorState {
	case circuitHalfOpen:
		if !failed {
			state.state = circuitClosed
			state.tripCount = 0
			state.failureTimestamps = nil
			state.mu.Unlock()
			klog.Infof("pod %s passed its half-open probe and is un-blacklisted", podDesc(pod, addr, modelName))
			return
		}
		needsConfirmation = true

	case circuitOpen: // already excluded

	default: // circuitClosed
		if !failed {
			// A success does not clear the window - see failureTimestamps' doc.
			state.mu.Unlock()
			return
		}
		count := recordFailureLocked(state, now)
		needsConfirmation = count >= FailureThreshold
	}
	state.mu.Unlock()

	if !needsConfirmation {
		return
	}

	healthy := b.confirmHealthy(addr, statusCode, modelName, pod.Labels[constants.ModelLabelEngine])

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.state != priorState {
		return
	}

	if priorState == circuitHalfOpen {
		if healthy {
			klog.Infof("pod %s failed its half-open probe with a failure status, but its confirmation check reports healthy; treating as a false alarm and closing the breaker", podDesc(pod, addr, modelName))
			state.state = circuitClosed
			state.tripCount = 0
			state.failureTimestamps = nil
			return
		}
		state.tripCount++
		state.state = circuitOpen
		state.openUntil = time.Now().Add(backoffDuration(state.tripCount))
		state.failureTimestamps = nil
		state.openReason = fmt.Sprintf("failed half-open probe, confirmed unhealthy (last: %s, trip #%d)", failureDesc(statusCode), state.tripCount)
		klog.Warningf("pod %s failed its half-open probe and its confirmation check reports it unhealthy; re-blacklisted for %s (trip #%d)", podDesc(pod, addr, modelName), backoffDuration(state.tripCount), state.tripCount)
		return
	}

	if len(state.failureTimestamps) < FailureThreshold {
		return
	}
	if healthy {
		klog.Infof("pod %s hit %d failures within %s (last: %s), but its confirmation check reports healthy; treating as a false alarm and not blacklisting", podDesc(pod, addr, modelName), FailureThreshold, podBlacklistFailureWindow, failureDesc(statusCode))
		state.failureTimestamps = nil
		return
	}
	state.tripCount++
	state.state = circuitOpen
	state.openUntil = time.Now().Add(backoffDuration(state.tripCount))
	state.failureTimestamps = nil
	state.openReason = fmt.Sprintf("%d failures within %s, confirmed unhealthy (last: %s, trip #%d)", FailureThreshold, podBlacklistFailureWindow, failureDesc(statusCode), state.tripCount)
	klog.Warningf("pod %s blacklisted for %s after %d failures within %s (last: %s), confirmed unhealthy (trip #%d)", podDesc(pod, addr, modelName), backoffDuration(state.tripCount), FailureThreshold, podBlacklistFailureWindow, failureDesc(statusCode), state.tripCount)
}

// Allow reports whether pod may be a routing candidate. It does not change the
// breaker: an open pod whose backoff has elapsed, or a half-open pod whose
// probe deadline has lapsed, is a candidate, but the half-open probe is only
// claimed by ClaimProbe for the pod the router actually picks. Claiming it here
// would spend it on every candidate the router then passes over, and keep the
// pod out of routing until probeDeadline.
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

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	state.lastUpdated = now

	if state.loadExcluded {
		return false
	}

	switch state.state {
	case circuitOpen:
		return !now.Before(state.openUntil)
	case circuitHalfOpen:
		return now.After(state.probeDeadline)
	default: // circuitClosed
		return true
	}
}

// ClaimProbe makes the request just routed to pod the half-open probe, and
// reports whether it did. Call it once per request, for the pod the router
// picked and only once the request is certain to be sent to it: the request's
// RecordResult is then what closes or re-opens the breaker.
//
// An open pod whose backoff has elapsed becomes half-open with a probe
// deadline podBlacklistProbeTimeout away; a half-open pod whose deadline has
// lapsed (the probe it was waiting for never reported) gets a fresh deadline.
// A closed pod, an open pod still inside its backoff (routing fell back to it
// because every pod was excluded) and a half-open pod whose probe is still in
// flight are left alone.
//
// Between Allow and this call a concurrent request can also pick the pod, so a
// burst can send a few requests to it before the claim lands. Allow excludes
// the pod again from the claim on, and whichever of those requests reports
// first settles the breaker.
func (b *PodBlacklist) ClaimProbe(pod *v1.Pod) (claimed bool) {
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

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	state.lastUpdated = now

	switch state.state {
	case circuitOpen:
		if now.Before(state.openUntil) {
			return false
		}
		state.state = circuitHalfOpen
	case circuitHalfOpen:
		if !now.After(state.probeDeadline) {
			return false
		}
	default: // circuitClosed
		return false
	}
	state.probeDeadline = now.Add(podBlacklistProbeTimeout)
	return true
}

// IsBlacklisted is a read-only exclusion check (open breaker or load-excluded).
// Unlike Allow it does not treat an open pod whose backoff has elapsed as a
// candidate: only an open pod before its deadline, or a load-excluded one,
// is reported.
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

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.loadExcluded {
		return true
	}
	return state.state == circuitOpen && time.Now().Before(state.openUntil)
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
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.state == circuitOpen && time.Now().Before(state.quarantinedUntil)
}

// BlacklistReason returns a short human-readable explanation for why pod is
// currently excluded from routing, or "" if it isn't excluded at all. Mirrors
// the exclusion checks in IsBlacklisted; a pod can be excluded for both
// reasons at once (e.g. flagged as a load outlier while also mid-backoff from
// an earlier failure streak), in which case both are reported.
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

	state.mu.Lock()
	defer state.mu.Unlock()

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

// SetLoadExcluded toggles load-outlier exclusion. Separate from the failure
// breaker: the next sweep is the recovery check. Clearing a never-excluded
// pod does not create a map entry.
//
// Returns whether this call actually changed the exclusion state. The caller
// (sweepLoadOutliers) uses this to attach richer, cache-derived context to the
// un-blacklist transition specifically -- SetLoadExcluded itself has no cache
// reference and stays a plain, dependency-free breaker so it's cheap to construct
// in tests.
func (b *PodBlacklist) SetLoadExcluded(pod *v1.Pod, excluded bool) (changed bool) {
	if !Enabled || pod == nil {
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
		changed = state.loadExcluded
		state.loadExcluded = false
		state.lastUpdated = time.Now()
		state.mu.Unlock()
		return changed
	}

	v := b.entry(key)
	state, ok := v.(*podFailureState)
	if !ok {
		klog.Errorf("PodBlacklist: unexpected type %T stored for pod %s; skipping", v, podDesc(pod, "", ""))
		return false
	}
	state.mu.Lock()
	changed = !state.loadExcluded
	state.loadExcluded = true
	state.lastUpdated = time.Now()
	state.mu.Unlock()
	if changed {
		klog.Warningf("pod %s blacklisted: running+waiting request count is a statistical outlier among its replicas", podDesc(pod, "", ""))
	}
	return changed
}

// OnPodFlakyReAdd implements cache.PodLifecycleListener. It proactively opens the
// pod's circuit breaker for flakyReAddOpenDuration, reusing the exact same
// open -> half-open -> (closed | re-open with backoff) machinery RecordResult
// drives for a confirmed failure streak -- just entered from a different trigger.
// tripCount is deliberately left untouched: this isn't a confirmed failure, so if
// the probe that follows does fail, backoff starts from trip #1 rather than
// wherever an unrelated pre-existing streak left off.
//
// The breaker is otherwise unaffected by the informer's delete/re-add cycle (it's
// keyed by pod name, independent of the cache's *Pod object identity), so a pod
// already open or half-open here means it was excluded for an unrelated reason
// (e.g. it was mid-recovery from a real failure streak when this flap happened) --
// this only extends openUntil if the new deadline is later, and never disturbs an
// in-progress half-open probe.
func (b *PodBlacklist) OnPodFlakyReAdd(pod *v1.Pod) {
	if !Enabled || pod == nil {
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
	now := time.Now()
	state.lastUpdated = now
	newOpenUntil := now.Add(flakyReAddOpenDuration)
	changed := false
	switch state.state {
	case circuitClosed:
		state.state = circuitOpen
		state.openUntil = newOpenUntil
		state.openReason = "flaky delete/re-add reappearance, pending health confirmation"
		changed = true
	case circuitOpen:
		if newOpenUntil.After(state.openUntil) {
			state.openUntil = newOpenUntil
			changed = true
		}
	case circuitHalfOpen:
		// A probe is already in flight (or about to be); let it resolve on its
		// own rather than resetting state mid-probe.
	}
	state.mu.Unlock()

	// Logged only on an actual state change -- not on every call (e.g. the
	// circuitHalfOpen no-op above, or circuitOpen where the existing window
	// already outlasts this one) -- both to avoid noise and because
	// PodLifecycleListener's contract (called synchronously from the informer
	// event handler) means this I/O must stay off the hot path as much as
	// possible even though c.mu itself is already released by the time this
	// listener runs (see addPod/updatePod in pkg/cache/informers.go).
	if changed {
		addr, model := podTarget(pod, "", "")
		klog.InfoS("pod proactively excluded pending health confirmation after a flaky delete/re-add",
			"pod", key, "addr", addr, "model", model, "duration", flakyReAddOpenDuration)
	}
}

// RecordProbeResult applies one background-sweep probe outcome and reports
// whether it blacklisted the pod (or renewed its blacklisting). failure says
// what failed and goes into the blacklist reason.
//
//   - A pod found dead (ProbeDead) podProbeFailureIterations sweeps in a row is
//     blacklisted for podProbeDeadBlacklistDuration, whatever state its breaker
//     is in: closed, or already open or half-open from failing requests. An
//     open pod's deadline is only ever extended, never shortened. While it stays
//     dead each further run of dead sweeps renews the hour. When the hour ends
//     the normal half-open path takes over: the next request routed to the pod
//     is its probe.
//   - A pod that is merely degraded (ProbeDegraded) podProbeFailureIterations
//     sweeps in a row, while its breaker is closed, is tripped with the
//     ordinary backoff.
//   - A healthy probe starts both counts over. It never lifts a blacklisting:
//     that is left to the deadline.
func (b *PodBlacklist) RecordProbeResult(pod *v1.Pod, result ProbeResult, failure string) (blacklisted bool) {
	if !Enabled || pod == nil {
		return false
	}
	key := PodKey(pod)
	if result == ProbeHealthy {
		if v, ok := b.pods.Load(key); ok {
			if state, ok := v.(*podFailureState); ok {
				state.mu.Lock()
				state.probeFailures = 0
				state.probeDeadSweeps = 0
				state.mu.Unlock()
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
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	state.lastUpdated = now

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
		state.failureTimestamps = nil
		state.probeFailures = 0
		state.probeDeadSweeps = 0
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
	state.failureTimestamps = nil
	state.probeFailures = 0
	state.openReason = fmt.Sprintf("background probe failed %d sweeps in a row (%s, trip #%d)", podProbeFailureIterations, failure, state.tripCount)
	klog.Warningf("pod %s blacklisted for %s: background probe failed %d sweeps in a row (%s) (trip #%d)", podDesc(pod, "", ""), backoffDuration(state.tripCount), podProbeFailureIterations, failure, state.tripCount)
	return true
}

// FailureCount is how many failures pod currently has recorded in its failure
// window, for diagnostics and tests.
func (b *PodBlacklist) FailureCount(pod *v1.Pod) int {
	if pod == nil {
		return 0
	}
	v, ok := b.pods.Load(PodKey(pod))
	if !ok {
		return 0
	}
	state, ok := v.(*podFailureState)
	if !ok {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.failureTimestamps)
}

// FilterBlacklisted returns the pods Allow admits, in order. While no pod is tracked at
// all it returns pods itself, so the common case costs no lookup and no allocation;
// callers must not modify the result in place.
func (b *PodBlacklist) FilterBlacklisted(pods []*v1.Pod) []*v1.Pod {
	if !Enabled || b.tracked.Load() == 0 {
		return pods
	}
	out := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if b.Allow(pod) {
			out = append(out, pod)
		}
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
		// The delete happens under state.mu: a writer that refreshes the entry takes the
		// same lock, so it either lands before this check and keeps the entry alive, or
		// after the delete. CompareAndDelete removes only the state that was just
		// checked, never a fresh one created under the same key in the meantime. A
		// writer that had already loaded the pointer and is still waiting for the lock
		// updates a state that is no longer stored; an entry that has been idle for the
		// whole TTL holds nothing worth keeping, and the next event recreates it.
		if now.Sub(last) > podBlacklistEntryTTL && b.pods.CompareAndDelete(key, value) {
			b.tracked.Add(-1)
		}
		state.mu.Unlock()
		return true
	})
}
