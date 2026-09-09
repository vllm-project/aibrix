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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

// hybridTestMessage is 40 characters: with the character tokenizer and the
// default 4-byte prefix block that is 10 blocks, so a pod holding the first
// n blocks reports a 10·n % match.
const hybridTestMessage = "0123456789abcdefghijklmnopqrstuvwxyzABCD"

var hybridTestConfig = HybridCacheLoadConfig{Factor: 0.5, MinMatchPct: 0}

// hybridTestFixture is a hybrid policy over a fresh prefix table and tracker.
type hybridTestFixture struct {
	policy  PrefillScorePolicy
	table   *prefixcacheindexer.PrefixHashTable
	tracker *TokenLoadTracker
	pods    []*v1.Pod
	ready   map[string]struct{}
}

func newHybridTestFixture(t *testing.T, cfg HybridCacheLoadConfig, podNames ...string) *hybridTestFixture {
	t.Helper()
	f := &hybridTestFixture{
		table:   prefixcacheindexer.NewPrefixHashTable(),
		tracker: newTokenLoadTracker(TokenLoadConfig{KVWeight: 0.5}, nil),
		ready:   map[string]struct{}{},
	}
	f.policy = NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(), f.table, f.tracker, cfg)
	for _, name := range podNames {
		f.pods = append(f.pods, tokenLoadTestPod(name))
		f.ready[name] = struct{}{}
	}
	return f
}

// seedPrefix records the first blocks of hybridTestMessage covering matchPct
// percent of it as resident on pod.
func (f *hybridTestFixture) seedPrefix(t *testing.T, pod string, matchPct int) {
	t.Helper()
	tokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(hybridTestMessage)
	require.NoError(t, err)
	_, hashes := f.table.MatchPrefix(tokens, testModelName, nil)
	require.Len(t, hashes, 10)
	f.table.AddPrefix(hashes[:len(hashes)*matchPct/100], testModelName, pod)
}

func (f *hybridTestFixture) prepare(t *testing.T) PrefillScorer {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, hybridTestMessage, "req-1", "")
	scorer, err := f.policy.Prepare(ctx, f.pods, f.ready)
	require.NoError(t, err)
	return scorer
}

func TestHybridCacheLoadPrefillPolicy_Name(t *testing.T) {
	f := newHybridTestFixture(t, hybridTestConfig)
	assert.Equal(t, PrefillScorePolicyHybridCacheLoad, f.policy.Name())
	assert.True(t, UsesTokenLoad(f.policy), "the router must charge the tracker for hybrid_cache_load")
}

// TestHybridCacheLoadPrefillPolicy_IdlePodsFollowThePrefix: with no load
// anywhere every score is a bare discount below 1, so the best match wins.
func TestHybridCacheLoadPrefillPolicy_IdlePodsFollowThePrefix(t *testing.T) {
	f := newHybridTestFixture(t, hybridTestConfig, "cold", "half", "hot")
	f.seedPrefix(t, "half", 50)
	f.seedPrefix(t, "hot", 100)

	scorer := f.prepare(t)
	assert.NotEmpty(t, scorer.PrefixHashes(), "hashes are needed to warm the index after selection")
	assert.Equal(t, float64(1), scorer.ScorePod(f.pods[0], 0, 0), "no match, no load: discount of 1")
	assert.Equal(t, 1-0.25*0.5, scorer.ScorePod(f.pods[1], 0, 0), "50 % match: 1 − 0.5² × factor")
	assert.Equal(t, 1-0.5, scorer.ScorePod(f.pods[2], 0, 0), "100 % match: 1 − factor")

	assert.Equal(t, 0, PrefixMatchPercent(scorer, "cold"))
	assert.Equal(t, 50, PrefixMatchPercent(scorer, "half"))
	assert.Equal(t, 100, PrefixMatchPercent(scorer, "hot"))
	assert.Equal(t, 0, PrefixMatchPercent(scorer, "unknown-pod"), "the lookup ran, so an unlisted pod is a 0 % match, not unknown")
}

// TestHybridCacheLoadPrefillPolicy_LoadOutweighsPrefix: a pod holding the
// prefix but also a long queue loses to an idle neighbour, because the match
// only discounts its load.
func TestHybridCacheLoadPrefillPolicy_LoadOutweighsPrefix(t *testing.T) {
	f := newHybridTestFixture(t, hybridTestConfig, "idle", "hot")
	f.seedPrefix(t, "hot", 100)
	f.tracker.AcquirePrefill("long", "hot", 8000) // priority 8000 + 0.5 × 8000

	scorer := f.prepare(t)
	assert.Equal(t, float64(1), scorer.ScorePod(f.pods[0], 0, 0))
	assert.Equal(t, 12000*0.5, scorer.ScorePod(f.pods[1], 1, 1), "load × (1 − factor)")

	// Barely loaded (below one token) still counts as idle.
	f.tracker.ReleaseAll("long")
	f.tracker.AcquirePrefill("tiny", "hot", 0.5)
	assert.Equal(t, 0.5, scorer.ScorePod(f.pods[1], 1, 1))
}

func TestHybridCacheLoadPrefillPolicy_MinMatchClamp(t *testing.T) {
	cfg := hybridTestConfig
	cfg.MinMatchPct = 60
	f := newHybridTestFixture(t, cfg, "weak", "strong")
	f.seedPrefix(t, "weak", 50)
	f.seedPrefix(t, "strong", 60)

	scorer := f.prepare(t)
	assert.Equal(t, float64(1), scorer.ScorePod(f.pods[0], 0, 0), "a match below the threshold is no match")
	assert.InDelta(t, 1-0.36*0.5, scorer.ScorePod(f.pods[1], 0, 0), 1e-9, "a match at the threshold counts")
	assert.Equal(t, 0, PrefixMatchPercent(scorer, "weak"), "the charge sees the same clamped match as the score")
	assert.Equal(t, 60, PrefixMatchPercent(scorer, "strong"))
}

func TestClampMinMatch(t *testing.T) {
	cases := []struct {
		matchPct int
		minPct   float64
		want     int
	}{
		{0, 0, 0},
		{30, 0, 30},
		{30, 30, 30},
		{29, 30, 0},
		{100, 30, 100},
		{50, 100, 0},
		{50, -1, 50},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, ClampMinMatch(tc.matchPct, tc.minPct), "ClampMinMatch(%d, %g)", tc.matchPct, tc.minPct)
	}
}

func TestHybridCacheLoadPrefillPolicy_NilTrackerScoresByPrefixOnly(t *testing.T) {
	table := prefixcacheindexer.NewPrefixHashTable()
	policy := NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(), table, nil, hybridTestConfig)
	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, hybridTestMessage, "req-1", "")
	scorer, err := policy.Prepare(ctx, nil, map[string]struct{}{"pod-a": {}})
	require.NoError(t, err)
	assert.Equal(t, float64(1), scorer.ScorePod(tokenLoadTestPod("pod-a"), 3, 3))
}

func TestPrefixMatchPercent_UnsupportedScorers(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, testMessage, "req-1", "")
	for _, policy := range []PrefillScorePolicy{NewLeastRequestPrefillPolicy(), NewTokenLoadPrefillPolicy(nil)} {
		scorer, err := policy.Prepare(ctx, nil, nil)
		require.NoError(t, err)
		assert.Equalf(t, -1, PrefixMatchPercent(scorer, "pod-a"), "%s has no prefix information", policy.Name())
	}
}

func TestDefaultHybridCacheLoadConfig(t *testing.T) {
	t.Setenv("AIBRIX_HYBRID_CACHE_LOAD_FACTOR", "0.4")
	t.Setenv("AIBRIX_MIN_MATCH_PCT", "30")
	cfg := DefaultHybridCacheLoadConfig()
	assert.Equal(t, 0.4, cfg.Factor)
	assert.Equal(t, float64(30), cfg.MinMatchPct)

	t.Setenv("AIBRIX_HYBRID_CACHE_LOAD_FACTOR", strings.Repeat("x", 3))
	t.Setenv("AIBRIX_MIN_MATCH_PCT", "0")
	cfg = DefaultHybridCacheLoadConfig()
	assert.Equal(t, DefaultHybridCacheLoadFactor, cfg.Factor, "invalid value falls back to the default")
	assert.Equal(t, float64(0), cfg.MinMatchPct, "0 is a valid setting: no threshold")

	// The range ends are valid; anything outside falls back.
	t.Setenv("AIBRIX_HYBRID_CACHE_LOAD_FACTOR", "1")
	t.Setenv("AIBRIX_MIN_MATCH_PCT", "100")
	cfg = DefaultHybridCacheLoadConfig()
	assert.Equal(t, float64(1), cfg.Factor)
	assert.Equal(t, float64(100), cfg.MinMatchPct)
	t.Setenv("AIBRIX_HYBRID_CACHE_LOAD_FACTOR", "0")
	cfg = DefaultHybridCacheLoadConfig()
	assert.Equal(t, float64(0), cfg.Factor, "a factor of 0 turns the discount off")

	for _, tc := range []struct{ factor, minMatch string }{
		{"1.5", "150"}, {"-0.1", "-1"}, {"NaN", "Inf"},
	} {
		t.Setenv("AIBRIX_HYBRID_CACHE_LOAD_FACTOR", tc.factor)
		t.Setenv("AIBRIX_MIN_MATCH_PCT", tc.minMatch)
		cfg = DefaultHybridCacheLoadConfig()
		assert.Equalf(t, DefaultHybridCacheLoadFactor, cfg.Factor, "factor %q is out of range", tc.factor)
		assert.Equalf(t, DefaultMinMatchPct, cfg.MinMatchPct, "min match %q is out of range", tc.minMatch)
	}
}
