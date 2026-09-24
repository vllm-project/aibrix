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
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMain installs the process defaults a request without overrides falls
// back to: the environment-derived PD knobs, exactly what the routing algorithm
// package installs at process startup.
func TestMain(m *testing.M) {
	defaults := EnvOverrides()
	types.SetDefaultPDOverrides(&defaults)
	os.Exit(m.Run())
}

// This file covers the PD read sites of the per-request overrides: each one
// reads the request's resolved PD overrides, parked on its PD leg by the PD
// router, and falls back to the process default table when the request carries
// none.

// probePDDefaults installs a known process default table for one test, so
// "kept the process default" is asserted against fixed values instead of
// whatever the environment of the test run configures.
func probePDDefaults(t *testing.T) {
	t.Helper()
	restore := types.DefaultPDOverrides()
	probe := *restore
	probe.Abort = types.PDAbortOverrides{Timeout: 3 * time.Second, RetryDelay: 2 * time.Second}
	probe.DecodeLB = types.PDDecodeLBOverrides{WeightRunning: 1.0, WeightThroughput: 1.0}
	probe.HybridCacheLoadFactor = 0.5
	probe.MinMatchPct = 0
	probe.TokenLoad = types.PDTokenLoadOverrides{KVWeight: 0.5, RequestCost: 100, TTL: time.Hour, SessionTTL: time.Hour}
	types.SetDefaultPDOverrides(&probe)
	t.Cleanup(func() { types.SetDefaultPDOverrides(restore) })
}

// knobsContext returns a request carrying the given resolved PD overrides, the
// way the PD router parks them on the request path. A nil overrides argument
// models a request with none, whose reads fall back to the process defaults.
func knobsContext(t *testing.T, overrides *types.PDOverrides) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "message", "req-knobs", "user")
	t.Cleanup(ctx.Delete)
	ctx.SetPDOverrides(overrides)
	return ctx
}

func TestDecodeAbortKnobsFromProfile(t *testing.T) {
	probePDDefaults(t)

	// Without overrides the request keeps the process default, and a nil leg
	// is safe: the abort may run for a request that never had one.
	assert.Equal(t, 3*time.Second, decodeAbortTimeoutFor(knobsContext(t, nil).PDLeg()))
	assert.Equal(t, 2*time.Second, decodeAbortRetryDelayFor(knobsContext(t, nil).PDLeg()))
	assert.Equal(t, 3*time.Second, decodeAbortTimeoutFor(nil))
	assert.Equal(t, 2*time.Second, decodeAbortRetryDelayFor(nil))

	overrides := *types.DefaultPDOverrides()
	overrides.Abort = types.PDAbortOverrides{Timeout: 0, RetryDelay: 7 * time.Second}
	leg := knobsContext(t, &overrides).PDLeg()
	assert.Equal(t, time.Duration(0), decodeAbortTimeoutFor(leg), "0 disables decode aborts for the request")
	assert.Equal(t, 7*time.Second, decodeAbortRetryDelayFor(leg), "0 would mean a single attempt; 7 is passed through")
}

func TestLoadBalancingDecodeWeightsFromProfile(t *testing.T) {
	probePDDefaults(t)

	policy := LoadBalancingDecodePolicy{}
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "decode-1"}}
	input := DecodePodInput{
		RunningReqs:     5,
		Throughput:      10,
		FreeGPUPercent:  50,
		MaxRequestCount: 10,
		MaxThroughput:   100,
		MaxFreeGPUUsage: 100,
	}

	wantDefault := (1.0*0.5 + 1.0*0.9) / 0.5
	assert.InDelta(t, wantDefault, policy.ScoreDecodePod(knobsContext(t, nil), pod, input), 1e-9, "process default weights")

	overrides := *types.DefaultPDOverrides()
	overrides.DecodeLB = types.PDDecodeLBOverrides{WeightRunning: 3.0, WeightThroughput: 1.0}
	wantProfile := (3.0*0.5 + 1.0*0.9) / 0.5
	assert.InDelta(t, wantProfile, policy.ScoreDecodePod(knobsContext(t, &overrides), pod, input), 1e-9, "profile weights")
}

func TestTokenLoadScorersUseProfileKnobs(t *testing.T) {
	probePDDefaults(t)

	tracker, _ := newTestTokenLoadTracker(t, TokenLoadConfig{KVWeight: 0.5})
	tracker.AcquirePrefill("charge", "prefill-1", 1000) // active 1000, resident KV 1000
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-1"}}

	t.Run("token_load kv weight", func(t *testing.T) {
		scorer, err := NewTokenLoadPrefillPolicy(tracker).Prepare(knobsContext(t, nil), nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1000+0.5*1000, scorer.ScorePod(pod, 0, 0), "process default weight")

		overrides := *types.DefaultPDOverrides()
		overrides.TokenLoad.KVWeight = 2.0
		scorer, err = NewTokenLoadPrefillPolicy(tracker).Prepare(knobsContext(t, &overrides), nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1000+2.0*1000, scorer.ScorePod(pod, 0, 0), "profile weight")
	})

	t.Run("hybrid_cache_load factor, min match and kv weight", func(t *testing.T) {
		policy := NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(),
			prefixcacheindexer.NewPrefixHashTable(), tracker)
		overrides := *types.DefaultPDOverrides()
		overrides.HybridCacheLoadFactor = 1.0
		overrides.MinMatchPct = 0.0
		overrides.TokenLoad.KVWeight = 2.0
		scorer, err := policy.Prepare(knobsContext(t, &overrides), nil, map[string]struct{}{"prefill-1": {}})
		require.NoError(t, err)
		hybrid, ok := scorer.(*hybridCacheLoadScorer)
		require.True(t, ok)
		assert.Equal(t, 1.0, hybrid.cfg.Factor)
		assert.Equal(t, 0.0, hybrid.cfg.MinMatchPct, "zero disables the minimum-match clamp")
		assert.Equal(t, 2.0, hybrid.kvWeight)
		assert.Equal(t, 1000+2.0*1000, hybrid.ScorePod(pod, 0, 0), "the profile weights reach the score")
	})

	t.Run("prefix_cache min match", func(t *testing.T) {
		policy := NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable())
		scorer, err := policy.Prepare(knobsContext(t, nil), nil, nil)
		require.NoError(t, err)
		prefix, ok := scorer.(*prefixCacheScorer)
		require.True(t, ok)
		assert.Equal(t, 0.0, prefix.minMatchPct, "the process default keeps every match")

		overrides := *types.DefaultPDOverrides()
		overrides.MinMatchPct = 30
		scorer, err = policy.Prepare(knobsContext(t, &overrides), nil, nil)
		require.NoError(t, err)
		prefix, ok = scorer.(*prefixCacheScorer)
		require.True(t, ok)
		assert.Equal(t, 30.0, prefix.minMatchPct, "the profile raises the threshold for its request")
	})
}

func TestTokenLoadTrackerProfileOverrides(t *testing.T) {
	t.Run("request cost", func(t *testing.T) {
		tracker, _ := newTestTokenLoadTracker(t, TokenLoadConfig{RequestCost: 100})
		assert.Equal(t, 250.0, tracker.PrefillCost(150), "env cost")
		assert.Equal(t, 350.0, tracker.PrefillCostWithRequestCost(150, 200), "profile cost")
	})

	t.Run("kv weight", func(t *testing.T) {
		tracker, _ := newTestTokenLoadTracker(t, TokenLoadConfig{KVWeight: 0.5})
		tracker.AcquirePrefill("req", "pod-a", 100)
		assert.Equal(t, 150.0, tracker.GetPriority("pod-a"), "env weight")
		assert.Equal(t, 300.0, tracker.GetPriorityWithKVWeight("pod-a", 2.0), "profile weight")
	})

	t.Run("session limits", func(t *testing.T) {
		tracker, _ := newTestTokenLoadTracker(t, TokenLoadConfig{SessionTTL: time.Hour, MaxSessions: 2})

		first, source := tracker.NewTokens("m", "s1", 100, -1)
		assert.Equal(t, 100, first)
		assert.Equal(t, NewTokensSourcePrompt, source)

		second, source := tracker.NewTokens("m", "s1", 150, -1)
		assert.Equal(t, 50, second, "the default path charges the session delta")
		assert.Equal(t, NewTokensSourceSession, source)

		whole, source := tracker.NewTokensWithSessionLimits("m", "s1", 200, -1, 0, 2)
		assert.Equal(t, 200, whole, "sessionTTL 0 disables the delta rule for the request")
		assert.Equal(t, NewTokensSourcePrompt, source)

		_, _ = tracker.NewTokensWithSessionLimits("m", "s2", 10, -1, time.Hour, 1)
		third, source := tracker.NewTokensWithSessionLimits("m", "s2", 90, -1, time.Hour, 1)
		assert.Equal(t, 90, third, "maxSessions 1 rejects a new session, so its next turn is charged in full")
		assert.Equal(t, NewTokensSourcePrompt, source)
	})

	t.Run("per-entry ttl", func(t *testing.T) {
		tracker, clock := newTestTokenLoadTracker(t, TokenLoadConfig{TTL: time.Hour})
		tracker.AcquirePrefillWithTTL("short", "pod-a", 10, 5*time.Second)
		tracker.AcquirePrefillWithTTL("never", "pod-a", 10, 0)
		tracker.AcquirePrefill("default", "pod-b", 5)

		clock.Advance(6 * time.Second)
		assert.Equal(t, 1, tracker.sweepExpired(), "only the charge past its own TTL is released")
		assertLoad(t, tracker, "pod-a", 10, 10)
		assertLoad(t, tracker, "pod-b", 5, 5)

		clock.Advance(2 * time.Hour)
		assert.Equal(t, 1, tracker.sweepExpired(), "the default-TTL charge ages out, the 0-TTL charge never does")
		assertLoad(t, tracker, "pod-a", 10, 10)
		assertLoad(t, tracker, "pod-b", 0, 0)
	})
}
