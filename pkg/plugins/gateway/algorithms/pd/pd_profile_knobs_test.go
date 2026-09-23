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

func ptrOf[T any](v T) *T { return &v }

// knobsContext returns a request whose PD leg carries the given profile
// knobs, the way the PD router parks them on the request path. A nil knobs
// argument models a request with no profile overrides.
func knobsContext(t *testing.T, knobs *types.PDRuntimeKnobs) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "message", "req-knobs", "user")
	t.Cleanup(ctx.Delete)
	ctx.SetPDKnobs(knobs)
	return ctx
}

func TestDecodeAbortKnobsFromProfile(t *testing.T) {
	// Without knobs the request keeps the environment default, and a nil leg
	// is safe: the abort may run for a request that never had one.
	assert.Equal(t, decodeAbortTimeout(), decodeAbortTimeoutFor(knobsContext(t, nil).PDLeg()))
	assert.Equal(t, decodeAbortRetryDelay(), decodeAbortRetryDelayFor(knobsContext(t, nil).PDLeg()))
	assert.Equal(t, decodeAbortTimeout(), decodeAbortTimeoutFor(nil))
	assert.Equal(t, decodeAbortRetryDelay(), decodeAbortRetryDelayFor(nil))

	leg := knobsContext(t, &types.PDRuntimeKnobs{
		DecodeAbortTimeoutSeconds:    ptrOf(0),
		DecodeAbortRetryDelaySeconds: ptrOf(7),
	}).PDLeg()
	assert.Equal(t, time.Duration(0), decodeAbortTimeoutFor(leg), "0 disables decode aborts for the request")
	assert.Equal(t, 7*time.Second, decodeAbortRetryDelayFor(leg), "0 would mean a single attempt; 7 is passed through")
}

func TestLoadBalancingDecodeWeightsFromProfile(t *testing.T) {
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

	wantEnv := (decodeLBWeightRunningReq*0.5 + decodeLBWeightThroughput*0.9) / 0.5
	assert.InDelta(t, wantEnv, policy.ScoreDecodePod(knobsContext(t, nil), pod, input), 1e-9, "env weights")

	knobs := &types.PDRuntimeKnobs{DecodeLBWeightRunning: ptrOf(3.0), DecodeLBWeightThroughput: ptrOf(1.0)}
	wantProfile := (3.0*0.5 + 1.0*0.9) / 0.5
	assert.InDelta(t, wantProfile, policy.ScoreDecodePod(knobsContext(t, knobs), pod, input), 1e-9, "profile weights")
}

func TestTokenLoadScorersUseProfileKnobs(t *testing.T) {
	tracker, _ := newTestTokenLoadTracker(t, TokenLoadConfig{KVWeight: 0.5})
	tracker.AcquirePrefill("charge", "prefill-1", 1000) // active 1000, resident KV 1000
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-1"}}

	t.Run("token_load kv weight", func(t *testing.T) {
		scorer, err := NewTokenLoadPrefillPolicy(tracker).Prepare(knobsContext(t, nil), nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1000+0.5*1000, scorer.ScorePod(pod, 0, 0), "env weight")

		scorer, err = NewTokenLoadPrefillPolicy(tracker).Prepare(
			knobsContext(t, &types.PDRuntimeKnobs{TokenLoadKVWeight: ptrOf(2.0)}), nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1000+2.0*1000, scorer.ScorePod(pod, 0, 0), "profile weight")
	})

	t.Run("hybrid_cache_load factor, min match and kv weight", func(t *testing.T) {
		policy := NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(),
			prefixcacheindexer.NewPrefixHashTable(), tracker, HybridCacheLoadConfig{Factor: 0.5, MinMatchPct: 10})
		knobs := &types.PDRuntimeKnobs{
			HybridCacheLoadFactor: ptrOf(1.0),
			MinMatchPct:           ptrOf(0.0),
			TokenLoadKVWeight:     ptrOf(2.0),
		}
		scorer, err := policy.Prepare(knobsContext(t, knobs), nil, map[string]struct{}{"prefill-1": {}})
		require.NoError(t, err)
		hybrid, ok := scorer.(*hybridCacheLoadScorer)
		require.True(t, ok)
		assert.Equal(t, 1.0, hybrid.cfg.Factor)
		assert.Equal(t, 0.0, hybrid.cfg.MinMatchPct, "zero disables the minimum-match clamp")
		assert.Equal(t, 2.0, hybrid.kvWeight)
		assert.Equal(t, 1000+2.0*1000, hybrid.ScorePod(pod, 0, 0), "the profile weights reach the score")
	})

	t.Run("prefix_cache min match", func(t *testing.T) {
		policy := NewPrefixCachePrefillPolicyWithConfig(tokenizer.NewCharacterTokenizer(),
			prefixcacheindexer.NewPrefixHashTable(), PrefixCacheConfig{MinMatchPct: 10})
		scorer, err := policy.Prepare(knobsContext(t, &types.PDRuntimeKnobs{MinMatchPct: ptrOf(0.0)}), nil, nil)
		require.NoError(t, err)
		prefix, ok := scorer.(*prefixCacheScorer)
		require.True(t, ok)
		assert.Equal(t, 0.0, prefix.minMatchPct)
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
