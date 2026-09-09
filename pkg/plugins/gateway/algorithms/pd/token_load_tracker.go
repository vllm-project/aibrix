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

package pd

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	// DefaultTokenLoadKVWeight is the default weight of resident KV tokens in
	// the token-load priority. A prefill pod keeps a request's KV blocks until
	// the decoder has pulled them, so resident KV still costs the pod something
	// after the prefill call itself has returned, but less than active compute.
	DefaultTokenLoadKVWeight = 0.3

	// DefaultTokenLoadRequestCost is the default fixed per-request cost in
	// tokens. It models scheduling and KV-transfer setup work that does not
	// scale with prompt length and keeps very short prompts from looking free.
	DefaultTokenLoadRequestCost = 3500

	// DefaultTokenLoadTTLSeconds bounds how long a charge may stay outstanding
	// before the janitor force-releases it. It must exceed the longest
	// legitimate request; an hour leaves a wide margin while still capping the
	// damage a leaked entry can do. The environment variable must be positive;
	// a TTL of 0 in TokenLoadConfig disables the sweep of stale charges.
	DefaultTokenLoadTTLSeconds = 3600

	// DefaultTokenLoadSessionTTLSeconds bounds how long the last prompt size
	// of a session is remembered for the session-delta cost estimate. It only
	// needs to outlive the gap between two turns of one conversation.
	DefaultTokenLoadSessionTTLSeconds = 1800

	// DefaultTokenLoadMaxSessions bounds how many (model, session) baselines
	// the tracker remembers at once. The session header is client-supplied,
	// so without a bound a stream of distinct IDs could grow the table by
	// QPS × SessionTTL entries before the janitor sweeps them. Once the
	// table is full, new sessions are not recorded and their requests are
	// charged by the prefix-match or whole-prompt rule instead.
	DefaultTokenLoadMaxSessions = 100000

	// maxTokenLoadSessionIDLen is the longest session ID the tracker records,
	// the same bound session-affinity routing applies to the caller-owned
	// session key. Longer IDs are treated as absent.
	maxTokenLoadSessionIDLen = 256

	// tokenLoadJanitorInterval is the scan period of the janitor, which
	// force-releases charges older than the TTL and prunes idle pods. It also
	// bounds how long a pod must be idle before it is pruned.
	tokenLoadJanitorInterval = 60 * time.Second

	// bytesPerTokenEstimate is the prompt-size heuristic used when the router
	// has no token count for the request: one token per four bytes of request
	// body, the same estimate the vLLM PD proxy examples use.
	bytesPerTokenEstimate = 4
)

// TokenLoadConfig holds the tunables of a TokenLoadTracker.
type TokenLoadConfig struct {
	// KVWeight is the weight of resident KV tokens in GetPriority.
	KVWeight float64
	// RequestCost is the fixed per-request cost added to every charge, in tokens.
	RequestCost float64
	// TTL is the maximum age of an outstanding charge; 0 disables the sweep
	// of stale charges.
	TTL time.Duration
	// SessionTTL is how long a session's last prompt size is remembered for
	// the session-delta cost estimate; 0 disables session tracking.
	SessionTTL time.Duration
	// MaxSessions bounds the number of sessions remembered at once; 0 selects
	// DefaultTokenLoadMaxSessions.
	MaxSessions int
}

// DefaultTokenLoadConfig returns the defaults, overridden by the
// AIBRIX_TOKEN_LOAD_KV_WEIGHT, AIBRIX_TOKEN_LOAD_REQUEST_COST,
// AIBRIX_TOKEN_LOAD_TTL_SECONDS, AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS and
// AIBRIX_TOKEN_LOAD_MAX_SESSIONS environment variables. Each must be
// positive, except that a session TTL of 0 turns session tracking off; an
// unset, empty or invalid value keeps the default.
func DefaultTokenLoadConfig() TokenLoadConfig {
	return TokenLoadConfig{
		KVWeight:    utils.LoadEnvFloat("AIBRIX_TOKEN_LOAD_KV_WEIGHT", DefaultTokenLoadKVWeight),
		RequestCost: utils.LoadEnvFloat("AIBRIX_TOKEN_LOAD_REQUEST_COST", DefaultTokenLoadRequestCost),
		TTL:         time.Duration(utils.LoadEnvInt("AIBRIX_TOKEN_LOAD_TTL_SECONDS", DefaultTokenLoadTTLSeconds)) * time.Second,
		SessionTTL:  loadSessionTTL(),
		MaxSessions: utils.LoadEnvInt("AIBRIX_TOKEN_LOAD_MAX_SESSIONS", DefaultTokenLoadMaxSessions),
	}
}

// loadSessionTTL reads AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS. Unlike the
// other tunables, 0 is a valid setting here: it disables the session-delta
// rule for deployments whose prefill pods cannot reach a conversation's
// earlier KV cache. Anything else goes through utils.LoadEnvInt.
func loadSessionTTL() time.Duration {
	const key = "AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS"
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v == 0 {
		klog.Infof("set %s: 0, session tracking disabled", key)
		return 0
	}
	return time.Duration(utils.LoadEnvInt(key, DefaultTokenLoadSessionTTLSeconds)) * time.Second
}

// TokenLoadTracker keeps a token-weighted ledger of the prefill load the
// router has assigned to each prefill pod. It is the state behind the
// token_load prefill score policy.
//
// Two counters are kept per pod:
//
//   - active tokens: prompts the pod is computing right now;
//   - kv tokens: prompts whose KV cache is still resident on the pod because
//     the decoder has not finished pulling it.
//
// The lifecycle of a prefill request is
//
//  1. AcquirePrefill(requestID, pod, cost): both counters += cost. The router
//     calls this under the same lock as the pod selection, so concurrent
//     selections see each other's charges.
//  2. ReleaseTokens(requestID): active -= cost, when the prefill HTTP call
//     returns.
//  3. ReleaseKVCache(requestID): kv -= cost, when the whole request completes.
//
// The two releases may arrive in either order; each subtracts its part once,
// and the request is forgotten as soon as both parts are released. Releases
// are idempotent per request ID and the counters are clamped at zero, so a
// duplicate release can never drive a pod negative. A charge whose release
// never arrives (the completion path was skipped) is force-released by the
// TTL janitor so it cannot pin load on a pod forever. The janitor also drops
// the counters and gauge series of pods that saw no traffic for a whole
// sweep interval, so pod churn does not grow the ledger without bound.
//
// All methods are safe for concurrent use. Reads do not allocate.
type TokenLoadTracker struct {
	activeTokens sync.Map // map[string]*podCounter, pod name → tokens
	kvTokens     sync.Map // map[string]*podCounter, pod name → tokens
	entries      sync.Map // map[string]*tokenLoadEntry, request ID → charge
	// sessions remembers the last prompt size per (model, session) so a
	// multi-turn continuation is charged only for what the engine computes.
	// sessionCount is its size, kept so admission can stop at MaxSessions
	// without walking the map; sessionsFull records that the cap was hit,
	// so the warning is logged once.
	sessions     sync.Map // map[string]*tokenLoadSession, sessionKey → last prompt
	sessionCount atomic.Int64
	sessionsFull atomic.Bool

	// countersMu serialises the janitor's pruning of idle pods (write lock)
	// with counter updates (read lock), so a counter and its gauge series are
	// never dropped between a writer's update and its gauge refresh. Reads of
	// the counters take no lock.
	countersMu sync.RWMutex

	cfg TokenLoadConfig
	// stopCh is closed by Close to stop the janitor; janitorDone is closed by
	// the janitor when it has stopped.
	stopCh      chan struct{}
	janitorDone chan struct{}
	closeOnce   sync.Once
	// clock is injectable so unit tests can age entries without sleeping.
	clock func() time.Time
}

// tokenLoadEntry records one AcquirePrefill so the releases subtract exactly
// what was charged.
type tokenLoadEntry struct {
	pod        string
	cost       float64
	acquiredAt time.Time
	// tokensReleased and kvReleased each flip once, when the matching release
	// runs, so neither a repeated release nor the janitor subtracts a part of
	// the charge twice. The entry leaves the ledger once both are set.
	tokensReleased atomic.Bool
	kvReleased     atomic.Bool
}

// released reports whether both parts of the charge have been released.
func (e *tokenLoadEntry) released() bool {
	return e.tokensReleased.Load() && e.kvReleased.Load()
}

// podCounter is one per-pod token counter: the bits of a float64 value and a
// flag that records any write since the janitor last looked, so the janitor
// can tell a pod that is merely between requests from one that is gone.
type podCounter struct {
	bits    atomic.Uint64
	touched atomic.Bool
}

func (c *podCounter) load() float64 { return math.Float64frombits(c.bits.Load()) }

// tokenLoadGaugeLabels is the label set of the per-pod gauges.
var tokenLoadGaugeLabels = []string{"pod_name"}

// tokenLoadSession is the last prompt seen for one (model, session).
type tokenLoadSession struct {
	mu           sync.Mutex
	promptTokens int
	lastSeen     time.Time
	// deleted is set, under mu, when the janitor removes the session from
	// the map, so a writer that loaded the pointer just before the removal
	// re-inserts instead of refreshing an orphan.
	deleted bool
}

// NewTokenLoadTracker creates a tracker with DefaultTokenLoadConfig and starts
// its TTL janitor when either TTL is positive.
func NewTokenLoadTracker() *TokenLoadTracker {
	return NewTokenLoadTrackerWithConfig(DefaultTokenLoadConfig())
}

// NewTokenLoadTrackerWithConfig creates a tracker with an explicit config and
// starts its TTL janitor when cfg.TTL or cfg.SessionTTL is positive.
func NewTokenLoadTrackerWithConfig(cfg TokenLoadConfig) *TokenLoadTracker {
	t := newTokenLoadTracker(cfg, time.Now)
	janitor := cfg.TTL > 0 || cfg.SessionTTL > 0
	if janitor {
		t.startJanitor(tokenLoadJanitorInterval)
	}
	klog.InfoS("token_load_tracker created",
		"kv_weight", cfg.KVWeight, "request_cost", cfg.RequestCost,
		"ttl_seconds", int(cfg.TTL.Seconds()), "session_ttl_seconds", int(cfg.SessionTTL.Seconds()),
		"max_sessions", t.cfg.MaxSessions, "janitor_enabled", janitor)
	return t
}

// newTokenLoadTracker builds a tracker without a janitor goroutine; tests use
// it with a fake clock and drive sweepExpired directly.
func newTokenLoadTracker(cfg TokenLoadConfig, clock func() time.Time) *TokenLoadTracker {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultTokenLoadMaxSessions
	}
	return &TokenLoadTracker{cfg: cfg, clock: clock, stopCh: make(chan struct{})}
}

// Close stops the janitor goroutine, if one was started, and returns once it
// has exited. Charges and counters stay readable. Safe to call more than once.
func (t *TokenLoadTracker) Close() {
	t.closeOnce.Do(func() { close(t.stopCh) })
	if t.janitorDone != nil {
		<-t.janitorDone
	}
}

// Config returns the tracker's tunables.
func (t *TokenLoadTracker) Config() TokenLoadConfig { return t.cfg }

// PrefillCost returns the load charged for a prefill of promptTokens tokens:
// the fixed per-request cost plus the prompt length.
func (t *TokenLoadTracker) PrefillCost(promptTokens int) float64 {
	if promptTokens < 0 {
		promptTokens = 0
	}
	return t.cfg.RequestCost + float64(promptTokens)
}

// EstimatePromptTokens estimates the prompt length of a request from its body
// size when no token count is available.
func EstimatePromptTokens(reqBody []byte) int {
	return len(reqBody) / bytesPerTokenEstimate
}

// Sources of the NewTokens estimate, for logs.
const (
	NewTokensSourceSession     = "session_delta"
	NewTokensSourcePrefixMatch = "prefix_match"
	NewTokensSourcePrompt      = "prompt"
)

// NewTokens estimates how many of a request's promptTokens the selected pod
// has to compute, the new_tokens term of the prefill cost. It returns the
// estimate and which rule produced it:
//
//  1. When sessionID is set and the (model, session) was seen before with a
//     shorter prompt, the growth since that prompt. Each turn of a
//     conversation resends the whole history, but an engine that still holds
//     the conversation's KV cache only computes the new turn. This rule
//     therefore assumes the earlier turns are reachable from the selected
//     pod (sticky routing, a shared or tiered KV store, or the same pod's
//     prefix cache); callers whose deployment cannot offer that should not
//     send a session ID. The prompt size is recorded for the next turn, so
//     call this once per charged request. A session ID longer than 256
//     bytes is ignored, and once MaxSessions sessions are live a new one is
//     not recorded, so its requests fall through to the rules below.
//  2. Otherwise, when matchPct (0-100) is non-negative, the part of the
//     prompt the pod's prefix cache does not cover: promptTokens × (1 −
//     matchPct/100). Pass a negative matchPct when no match information is
//     available.
//  3. Otherwise the whole prompt.
//
// The result is never negative or larger than promptTokens.
func (t *TokenLoadTracker) NewTokens(model, sessionID string, promptTokens, matchPct int) (int, string) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if sessionID != "" && len(sessionID) <= maxTokenLoadSessionIDLen && t.cfg.SessionTTL > 0 {
		if last, seen := t.recordSessionPrompt(model, sessionID, promptTokens); seen && promptTokens > last {
			return promptTokens - last, NewTokensSourceSession
		}
	}
	if matchPct >= 0 {
		if matchPct > 100 {
			matchPct = 100
		}
		return promptTokens * (100 - matchPct) / 100, NewTokensSourcePrefixMatch
	}
	return promptTokens, NewTokensSourcePrompt
}

// recordSessionPrompt stores promptTokens as the last prompt of (model,
// session) and returns the previous value and whether there was one. A new
// session is admitted only while fewer than MaxSessions are live; otherwise
// nothing is recorded and the caller charges by the other rules.
func (t *TokenLoadTracker) recordSessionPrompt(model, sessionID string, promptTokens int) (int, bool) {
	now := t.now()
	key := sessionKey(model, sessionID)
	for {
		v, loaded := t.sessions.Load(key)
		if !loaded {
			if t.sessionCount.Add(1) > int64(t.cfg.MaxSessions) {
				t.sessionCount.Add(-1)
				if t.sessionsFull.CompareAndSwap(false, true) {
					klog.Warningf("token_load_tracker session table is full (max_sessions=%d): new sessions are charged without a session delta until the janitor sweeps idle ones", t.cfg.MaxSessions)
				}
				return 0, false
			}
			if _, raced := t.sessions.LoadOrStore(key, &tokenLoadSession{promptTokens: promptTokens, lastSeen: now}); raced {
				t.sessionCount.Add(-1) // another writer admitted it first; read theirs
				continue
			}
			return 0, false
		}
		sess := v.(*tokenLoadSession)
		sess.mu.Lock()
		if sess.deleted {
			sess.mu.Unlock() // swept between Load and Lock; start over
			continue
		}
		last := sess.promptTokens
		sess.promptTokens = promptTokens
		sess.lastSeen = now
		sess.mu.Unlock()
		return last, true
	}
}

// sessionKey qualifies a client-supplied session ID with the model: the same
// ID may be reused against different models, and the delta only makes sense
// against the same one.
func sessionKey(model, sessionID string) string {
	return model + "\x00" + sessionID
}

// AcquirePrefill charges cost to both the active and the resident-KV counter
// of pod and records the charge under requestID for later release. Request
// IDs are unique per request, so a second AcquirePrefill for the same
// requestID is a caller bug; it is tolerated by releasing whatever the
// earlier charge still holds before the new one replaces it, with a warning.
func (t *TokenLoadTracker) AcquirePrefill(requestID, pod string, cost float64) {
	entry := &tokenLoadEntry{pod: pod, cost: cost, acquiredAt: t.now()}
	if prev, loaded := t.entries.Swap(requestID, entry); loaded {
		old := prev.(*tokenLoadEntry)
		klog.Warningf("token_load_tracker re-acquire for request_id=%s: releasing earlier charge pod_name=%s cost=%g before charging pod_name=%s cost=%g",
			requestID, old.pod, old.cost, pod, cost)
		t.releaseTokens(requestID, old)
		t.releaseKV(requestID, old)
	}
	t.addActive(pod, cost)
	t.addKV(pod, cost)
	klog.V(4).InfoS("token_load_acquired", "request_id", requestID, "pod_name", pod, "cost", cost)
}

// ReleaseTokens subtracts requestID's charge from its pod's active counter.
// Call it when the prefill HTTP call returns. The KV charge stays until
// ReleaseKVCache. No-op for an unknown request ID or a repeated call.
func (t *TokenLoadTracker) ReleaseTokens(requestID string) {
	if v, ok := t.entries.Load(requestID); ok {
		t.releaseTokens(requestID, v.(*tokenLoadEntry))
	}
}

// ReleaseKVCache subtracts requestID's charge from its pod's resident-KV
// counter. Call it when the request completes. It does not touch the active
// counter: a request that completes while its prefill call is somehow still
// outstanding keeps that charge, and stays in the ledger, until ReleaseTokens
// or the janitor. No-op for an unknown request ID or a repeated call.
func (t *TokenLoadTracker) ReleaseKVCache(requestID string) {
	if v, ok := t.entries.Load(requestID); ok {
		t.releaseKV(requestID, v.(*tokenLoadEntry))
	}
}

// releaseTokens releases the active part of entry, once.
func (t *TokenLoadTracker) releaseTokens(requestID string, entry *tokenLoadEntry) {
	if !entry.tokensReleased.CompareAndSwap(false, true) {
		return
	}
	t.addActive(entry.pod, -entry.cost)
	klog.V(4).InfoS("token_load_tokens_released", "request_id", requestID, "pod_name", entry.pod, "cost", entry.cost)
	t.forgetIfReleased(requestID, entry)
}

// releaseKV releases the resident-KV part of entry, once.
func (t *TokenLoadTracker) releaseKV(requestID string, entry *tokenLoadEntry) {
	if !entry.kvReleased.CompareAndSwap(false, true) {
		return
	}
	t.addKV(entry.pod, -entry.cost)
	klog.V(4).InfoS("token_load_kv_released", "request_id", requestID, "pod_name", entry.pod, "cost", entry.cost)
	t.forgetIfReleased(requestID, entry)
}

// forgetIfReleased drops entry from the ledger once both of its parts are
// released. Each release sets its own flag before checking both, so whichever
// of two concurrent releases finishes last sees both flags and deletes. The
// delete is keyed on the entry pointer, so it never removes a newer charge
// that replaced this one under the same request ID.
func (t *TokenLoadTracker) forgetIfReleased(requestID string, entry *tokenLoadEntry) {
	if entry.released() {
		t.entries.CompareAndDelete(requestID, entry)
	}
}

// ReleaseAll releases whatever requestID still holds on both counters and
// forgets the request. Use it on terminal paths where nothing of the request
// can remain on the pod (prefill failure, request completion).
func (t *TokenLoadTracker) ReleaseAll(requestID string) {
	t.ReleaseTokens(requestID)
	t.ReleaseKVCache(requestID)
}

// GetLoad returns pod's current active and resident-KV token counters.
// Unknown pods report 0, 0.
func (t *TokenLoadTracker) GetLoad(pod string) (activeTokens, kvTokens float64) {
	return loadFloat(&t.activeTokens, pod), loadFloat(&t.kvTokens, pod)
}

// GetPriority returns pod's token-load priority, lower is better:
//
//	active_tokens + kv_weight * kv_tokens
func (t *TokenLoadTracker) GetPriority(pod string) float64 {
	active, kv := t.GetLoad(pod)
	return active + t.cfg.KVWeight*kv
}

func (t *TokenLoadTracker) now() time.Time {
	if t.clock == nil {
		return time.Now()
	}
	return t.clock()
}

func (t *TokenLoadTracker) addActive(pod string, delta float64) {
	t.addCounter(&t.activeTokens, metrics.PDTokenLoadActiveTokens, pod, delta)
}

func (t *TokenLoadTracker) addKV(pod string, delta float64) {
	t.addCounter(&t.kvTokens, metrics.PDTokenLoadKVTokens, pod, delta)
}

// addCounter adds delta to pod's counter in m and publishes the result as the
// gauge metricName. The shared lock only excludes the janitor's pruning;
// writers still run concurrently with each other.
func (t *TokenLoadTracker) addCounter(m *sync.Map, metricName, pod string, delta float64) {
	t.countersMu.RLock()
	defer t.countersMu.RUnlock()
	value := addFloat(m, pod, delta)
	metrics.SetGaugeMetric(metricName, metrics.GetMetricHelp(metricName), value, tokenLoadGaugeLabels, pod)
}

// startJanitor runs sweepExpired and pruneIdle every interval until Close is
// called.
func (t *TokenLoadTracker) startJanitor(interval time.Duration) {
	t.janitorDone = make(chan struct{})
	go func() {
		defer close(t.janitorDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.sweepExpired()
				t.pruneIdle()
			case <-t.stopCh:
				return
			}
		}
	}()
}

// sweepExpired force-releases every charge older than TTL and forgets every
// session idle for longer than SessionTTL. It returns how many charges it
// released. Each part is a no-op when its TTL is not positive.
func (t *TokenLoadTracker) sweepExpired() int {
	now := t.now()
	if t.cfg.SessionTTL > 0 {
		t.sessions.Range(func(key, val any) bool {
			sess := val.(*tokenLoadSession)
			// The expiry check, the removal and the tombstone happen under
			// the session lock, so a concurrent refresh either lands before
			// the check and keeps the session, or sees the tombstone and
			// re-inserts; it can never be lost.
			sess.mu.Lock()
			if now.Sub(sess.lastSeen) > t.cfg.SessionTTL {
				sess.deleted = true
				t.sessions.Delete(key)
				t.sessionCount.Add(-1)
			}
			sess.mu.Unlock()
			return true
		})
	}
	if t.cfg.TTL <= 0 {
		return 0
	}
	released := 0
	t.entries.Range(func(key, val any) bool {
		entry := val.(*tokenLoadEntry)
		age := now.Sub(entry.acquiredAt)
		if age <= t.cfg.TTL {
			return true
		}
		requestID := key.(string)
		klog.Warningf("token_load_tracker force-releasing stale charge: request_id=%s pod_name=%s cost=%g age_seconds=%.0f ttl_seconds=%.0f",
			requestID, entry.pod, entry.cost, age.Seconds(), t.cfg.TTL.Seconds())
		// Release this entry, not whatever is under requestID now: the flags
		// make a concurrent normal release harmless, and a re-acquire that
		// replaced the entry in the meantime must keep its own charge.
		t.releaseTokens(requestID, entry)
		t.releaseKV(requestID, entry)
		released++
		return true
	})
	return released
}

// pruneIdle drops the counters and gauge series of every pod that has been
// idle since the previous call, meaning both counters are zero and neither
// was written in between, and returns how many pods it dropped. Pods come
// and go under autoscaling and rollouts; without pruning each one would keep
// two counters and two gauge series on the gateway forever. A pruned pod is
// re-created, from zero, by its next charge.
func (t *TokenLoadTracker) pruneIdle() int {
	t.countersMu.Lock()
	defer t.countersMu.Unlock()

	pods := map[string]struct{}{}
	collect := func(key, _ any) bool {
		pods[key.(string)] = struct{}{}
		return true
	}
	t.activeTokens.Range(collect)
	t.kvTokens.Range(collect)

	pruned := 0
	for pod := range pods {
		// Clear both flags before deciding, so a pod that is active on one
		// counter is re-examined from scratch next time.
		active := idleCounter(&t.activeTokens, pod)
		kv := idleCounter(&t.kvTokens, pod)
		if !active || !kv {
			continue
		}
		t.activeTokens.Delete(pod)
		t.kvTokens.Delete(pod)
		metrics.DeleteGaugeMetric(metrics.PDTokenLoadActiveTokens, tokenLoadGaugeLabels, pod)
		metrics.DeleteGaugeMetric(metrics.PDTokenLoadKVTokens, tokenLoadGaugeLabels, pod)
		pruned++
		klog.V(4).InfoS("token_load_pod_pruned", "pod_name", pod)
	}
	return pruned
}

// idleCounter reports whether pod's counter in m is zero and was not written
// since the last call, and clears the written flag. A missing counter is idle.
func idleCounter(m *sync.Map, pod string) bool {
	v, ok := m.Load(pod)
	if !ok {
		return true
	}
	c := v.(*podCounter)
	touched := c.touched.Swap(false)
	return !touched && c.load() == 0
}

// addFloat atomically adds delta to the float64 stored under key, clamps the
// result at zero, and returns the new value. The counter is only allocated
// the first time a pod is seen; later calls take the read path.
func addFloat(m *sync.Map, key string, delta float64) float64 {
	v, ok := m.Load(key)
	if !ok {
		v, _ = m.LoadOrStore(key, &podCounter{})
	}
	c := v.(*podCounter)
	for {
		old := c.bits.Load()
		next := math.Float64frombits(old) + delta
		if next < 0 {
			next = 0
		}
		if c.bits.CompareAndSwap(old, math.Float64bits(next)) {
			c.touched.Store(true)
			return next
		}
	}
}

func loadFloat(m *sync.Map, key string) float64 {
	v, ok := m.Load(key)
	if !ok {
		return 0
	}
	return v.(*podCounter).load()
}
