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
	// a TTL of 0 in TokenLoadConfig disables the janitor.
	DefaultTokenLoadTTLSeconds = 3600

	// tokenLoadJanitorInterval is the scan period of the TTL janitor.
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
	// TTL is the maximum age of an outstanding charge; 0 disables the janitor.
	TTL time.Duration
}

// DefaultTokenLoadConfig returns the defaults, overridden by the
// AIBRIX_TOKEN_LOAD_KV_WEIGHT, AIBRIX_TOKEN_LOAD_REQUEST_COST and
// AIBRIX_TOKEN_LOAD_TTL_SECONDS environment variables. Each must be positive;
// an unset, empty or invalid value keeps the default.
func DefaultTokenLoadConfig() TokenLoadConfig {
	return TokenLoadConfig{
		KVWeight:    utils.LoadEnvFloat("AIBRIX_TOKEN_LOAD_KV_WEIGHT", DefaultTokenLoadKVWeight),
		RequestCost: utils.LoadEnvFloat("AIBRIX_TOKEN_LOAD_REQUEST_COST", DefaultTokenLoadRequestCost),
		TTL:         time.Duration(utils.LoadEnvInt("AIBRIX_TOKEN_LOAD_TTL_SECONDS", DefaultTokenLoadTTLSeconds)) * time.Second,
	}
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
// Every Release is idempotent per request ID and the counters are clamped at
// zero, so a duplicate release can never drive a pod negative. A charge whose
// release never arrives (the completion path was skipped) is force-released by
// the TTL janitor so it cannot pin load on a pod forever.
//
// All methods are safe for concurrent use. Reads do not allocate.
type TokenLoadTracker struct {
	activeTokens sync.Map // map[string]*atomic.Uint64 (float64 bits), pod name → tokens
	kvTokens     sync.Map // map[string]*atomic.Uint64 (float64 bits), pod name → tokens
	entries      sync.Map // map[string]*tokenLoadEntry, request ID → charge

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
	// tokensReleased flips once when ReleaseTokens runs, so neither a second
	// ReleaseTokens nor the janitor subtracts the active charge again.
	tokensReleased atomic.Bool
}

// NewTokenLoadTracker creates a tracker with DefaultTokenLoadConfig and starts
// its TTL janitor when the TTL is positive.
func NewTokenLoadTracker() *TokenLoadTracker {
	return NewTokenLoadTrackerWithConfig(DefaultTokenLoadConfig())
}

// NewTokenLoadTrackerWithConfig creates a tracker with an explicit config and
// starts its TTL janitor when cfg.TTL is positive.
func NewTokenLoadTrackerWithConfig(cfg TokenLoadConfig) *TokenLoadTracker {
	t := newTokenLoadTracker(cfg, time.Now)
	if cfg.TTL > 0 {
		t.startJanitor(tokenLoadJanitorInterval)
	}
	klog.InfoS("token_load_tracker created",
		"kv_weight", cfg.KVWeight, "request_cost", cfg.RequestCost,
		"ttl_seconds", int(cfg.TTL.Seconds()), "janitor_enabled", cfg.TTL > 0)
	return t
}

// newTokenLoadTracker builds a tracker without a janitor goroutine; tests use
// it with a fake clock and drive sweepExpired directly.
func newTokenLoadTracker(cfg TokenLoadConfig, clock func() time.Time) *TokenLoadTracker {
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

// AcquirePrefill charges cost to both the active and the resident-KV counter
// of pod and records the charge under requestID for later release. A second
// AcquirePrefill for the same requestID replaces the first charge without
// releasing it; callers pair every acquire with a release.
func (t *TokenLoadTracker) AcquirePrefill(requestID, pod string, cost float64) {
	entry := &tokenLoadEntry{pod: pod, cost: cost, acquiredAt: t.now()}
	t.entries.Store(requestID, entry)
	t.addActive(pod, cost)
	t.addKV(pod, cost)
	klog.V(4).InfoS("token_load_acquired", "request_id", requestID, "pod_name", pod, "cost", cost)
}

// ReleaseTokens subtracts requestID's charge from its pod's active counter.
// Call it when the prefill HTTP call returns. The KV charge stays until
// ReleaseKVCache. No-op for an unknown request ID or a repeated call.
func (t *TokenLoadTracker) ReleaseTokens(requestID string) {
	v, ok := t.entries.Load(requestID)
	if !ok {
		return
	}
	entry := v.(*tokenLoadEntry)
	if !entry.tokensReleased.CompareAndSwap(false, true) {
		return
	}
	t.addActive(entry.pod, -entry.cost)
	klog.V(4).InfoS("token_load_tokens_released", "request_id", requestID, "pod_name", entry.pod, "cost", entry.cost)
}

// ReleaseKVCache subtracts requestID's charge from its pod's resident-KV
// counter and forgets the request. Call it when the request completes. It
// does not touch the active counter: a request that completes while its
// prefill call is somehow still outstanding keeps that charge until
// ReleaseTokens or the janitor. No-op for an unknown request ID.
func (t *TokenLoadTracker) ReleaseKVCache(requestID string) {
	v, ok := t.entries.LoadAndDelete(requestID)
	if !ok {
		return
	}
	entry := v.(*tokenLoadEntry)
	t.addKV(entry.pod, -entry.cost)
	klog.V(4).InfoS("token_load_kv_released", "request_id", requestID, "pod_name", entry.pod, "cost", entry.cost)
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
	value := addFloat(&t.activeTokens, pod, delta)
	metrics.SetGaugeMetric(metrics.PDTokenLoadActiveTokens, metrics.GetMetricHelp(metrics.PDTokenLoadActiveTokens),
		value, []string{"pod_name"}, pod)
}

func (t *TokenLoadTracker) addKV(pod string, delta float64) {
	value := addFloat(&t.kvTokens, pod, delta)
	metrics.SetGaugeMetric(metrics.PDTokenLoadKVTokens, metrics.GetMetricHelp(metrics.PDTokenLoadKVTokens),
		value, []string{"pod_name"}, pod)
}

// startJanitor runs sweepExpired every interval until Close is called.
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
			case <-t.stopCh:
				return
			}
		}
	}()
}

// sweepExpired force-releases every charge older than the TTL and returns how
// many it released. No-op when the TTL is not positive.
func (t *TokenLoadTracker) sweepExpired() int {
	if t.cfg.TTL <= 0 {
		return 0
	}
	now := t.now()
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
		// The releases re-check the entry under the map's own atomics, so a
		// concurrent normal release between Range and here is harmless.
		t.ReleaseAll(requestID)
		released++
		return true
	})
	return released
}

// addFloat atomically adds delta to the float64 stored under key, clamps the
// result at zero, and returns the new value. The counter is only allocated
// the first time a pod is seen; later calls take the read path.
func addFloat(m *sync.Map, key string, delta float64) float64 {
	v, ok := m.Load(key)
	if !ok {
		v, _ = m.LoadOrStore(key, &atomic.Uint64{})
	}
	a := v.(*atomic.Uint64)
	for {
		old := a.Load()
		next := math.Float64frombits(old) + delta
		if next < 0 {
			next = 0
		}
		if a.CompareAndSwap(old, math.Float64bits(next)) {
			return next
		}
	}
}

func loadFloat(m *sync.Map, key string) float64 {
	v, ok := m.Load(key)
	if !ok {
		return 0
	}
	return math.Float64frombits(v.(*atomic.Uint64).Load())
}
