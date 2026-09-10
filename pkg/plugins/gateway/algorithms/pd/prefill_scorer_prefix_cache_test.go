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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

type prefixCacheTestFixture struct {
	policy PrefillScorePolicy
	table  *prefixcacheindexer.PrefixHashTable
	pods   []*v1.Pod
	ready  map[string]struct{}
}

func newPrefixCacheTestFixture(t *testing.T, cfg PrefixCacheConfig, podNames ...string) *prefixCacheTestFixture {
	t.Helper()
	f := &prefixCacheTestFixture{
		table: prefixcacheindexer.NewPrefixHashTable(),
		ready: map[string]struct{}{},
	}
	f.policy = NewPrefixCachePrefillPolicyWithConfig(tokenizer.NewCharacterTokenizer(), f.table, cfg)
	for _, name := range podNames {
		f.pods = append(f.pods, tokenLoadTestPod(name))
		f.ready[name] = struct{}{}
	}
	return f
}

// seedPrefix makes pod hold the first matchPct percent of hybridTestMessage.
func (f *prefixCacheTestFixture) seedPrefix(t *testing.T, pod string, matchPct int) {
	t.Helper()
	tokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(hybridTestMessage)
	require.NoError(t, err)
	_, hashes := f.table.MatchPrefix(tokens, testModelName, nil)
	require.Len(t, hashes, 10)
	f.table.AddPrefix(hashes[:len(hashes)*matchPct/100], testModelName, pod)
}

func (f *prefixCacheTestFixture) prepare(t *testing.T) PrefillScorer {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, hybridTestMessage, "req-1", "")
	scorer, err := f.policy.Prepare(ctx, f.pods, f.ready)
	require.NoError(t, err)
	return scorer
}

// TestPrefixCachePrefillPolicy_IncidentalMatchOutweighsLoad documents the
// default behaviour the minimum match exists for: a 10 % match on an
// otherwise busier pod beats an idle pod with no match, because the cache
// term spans 10.0 and the load term only 1.0.
func TestPrefixCachePrefillPolicy_IncidentalMatchOutweighsLoad(t *testing.T) {
	f := newPrefixCacheTestFixture(t, PrefixCacheConfig{}, "idle", "magnet")
	f.seedPrefix(t, "magnet", 10)

	scorer := f.prepare(t)
	assert.NotEmpty(t, scorer.PrefixHashes())
	idle := scorer.ScorePod(f.pods[0], 0, 4)
	magnet := scorer.ScorePod(f.pods[1], 4, 4)
	assert.Equal(t, float64(10), idle, "(100 - 0) × 0.1 + 0 / 4")
	assert.Equal(t, float64(10), magnet, "(100 - 10) × 0.1 + 4 / 4")
	assert.Less(t, scorer.ScorePod(f.pods[1], 3, 4), idle, "any match beats the whole load range short of saturation")
}

// TestPrefixCachePrefillPolicy_MinMatchClamp: with a threshold the incidental
// match is ignored, so the request follows the load; a match at the threshold
// still counts.
func TestPrefixCachePrefillPolicy_MinMatchClamp(t *testing.T) {
	f := newPrefixCacheTestFixture(t, PrefixCacheConfig{MinMatchPct: 30}, "idle", "weak", "strong")
	f.seedPrefix(t, "weak", 10)
	f.seedPrefix(t, "strong", 30)

	scorer := f.prepare(t)
	assert.Equal(t, float64(10), scorer.ScorePod(f.pods[0], 0, 4))
	assert.Equal(t, float64(10.25), scorer.ScorePod(f.pods[1], 1, 4), "a match below the threshold is no match, so load decides")
	assert.Equal(t, float64(7.25), scorer.ScorePod(f.pods[2], 1, 4), "a match at the threshold counts: (100 - 30) × 0.1 + 1 / 4")
}

func TestPrefixCachePrefillPolicy_DefaultHasNoThreshold(t *testing.T) {
	f := &prefixCacheTestFixture{table: prefixcacheindexer.NewPrefixHashTable(), ready: map[string]struct{}{"weak": {}}}
	f.policy = NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), f.table)
	f.pods = []*v1.Pod{tokenLoadTestPod("weak")}
	f.seedPrefix(t, "weak", 10)

	scorer := f.prepare(t)
	assert.Equal(t, float64(9), scorer.ScorePod(f.pods[0], 0, 1), "the plain constructor keeps every match")
}

func TestDefaultPrefixCacheConfig(t *testing.T) {
	t.Setenv("AIBRIX_MIN_MATCH_PCT", "")
	assert.Equal(t, DefaultMinMatchPct, DefaultPrefixCacheConfig().MinMatchPct)

	t.Setenv("AIBRIX_MIN_MATCH_PCT", "30")
	assert.Equal(t, float64(30), DefaultPrefixCacheConfig().MinMatchPct)
	assert.Equal(t, float64(30), DefaultHybridCacheLoadConfig().MinMatchPct, "both policies read the same knob")

	for _, raw := range []string{"-1", "101", "abc"} {
		t.Setenv("AIBRIX_MIN_MATCH_PCT", raw)
		assert.Equalf(t, DefaultMinMatchPct, DefaultPrefixCacheConfig().MinMatchPct, "%q is invalid", raw)
	}
}
