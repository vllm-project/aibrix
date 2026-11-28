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

package router

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type countingTokenizer struct {
	inner tokenizer.Tokenizer
	calls atomic.Int64
}

func (c *countingTokenizer) TokenizeInputText(text string) ([]byte, error) {
	c.calls.Add(1)
	return c.inner.TokenizeInputText(text)
}

// candidateStageTestCache wraps cache.Cache to assert zero Redis-path
// GetPodsRunningRequests calls during SelectCandidates while providing
// controllable in-memory ApproxRunningRequestsProvider counts.
type candidateStageTestCache struct {
	cache.Cache
	approxCounts   map[string]int64
	redisReadCalls atomic.Int64
}

func (c *candidateStageTestCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	c.redisReadCalls.Add(1)
	return c.Cache.GetPodsRunningRequests(pods)
}

func (c *candidateStageTestCache) GetPodsRunningRequestsApprox(pods []*v1.Pod) map[string]int64 {
	if c.approxCounts == nil {
		return nil
	}
	out := make(map[string]int64, len(pods))
	for _, p := range pods {
		if p == nil {
			continue
		}
		key := utils.GeneratePodKey(p.Namespace, p.Name)
		if v, ok := c.approxCounts[key]; ok {
			out[key] = v
		}
	}
	return out
}

func makeFleetWithEncoders(rolesets, replicas, encodersPerRS int) []*v1.Pod {
	pods := make([]*v1.Pod, 0, rolesets*(replicas*2+encodersPerRS))
	for rs := 0; rs < rolesets; rs++ {
		rsID := fmt.Sprintf("rs-%03d", rs)
		for rep := 0; rep < replicas; rep++ {
			pods = append(pods, makePDPod(fmt.Sprintf("p-%03d-%03d", rs, rep), rsID, PDRolePrefill, nil))
			pods = append(pods, makePDPod(fmt.Sprintf("d-%03d-%03d", rs, rep), rsID, PDRoleDecode, nil))
		}
		for e := 0; e < encodersPerRS; e++ {
			pods = append(pods, makePDPod(fmt.Sprintf("e-%03d-%02d", rs, e), rsID, encode.Role, nil))
		}
	}
	return pods
}

func newCandidateTestRouter(c cache.Cache, prefillPol pd.PrefillScorePolicy, decodePol pd.DecodeScorePolicy, idx *prefixcacheindexer.PrefixHashTable) *pdRouter {
	if c == nil {
		c = cache.NewForTest()
	}
	if idx == nil {
		idx = prefixcacheindexer.NewPrefixHashTable()
	}
	if prefillPol == nil {
		prefillPol = pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), idx)
	}
	if decodePol == nil {
		decodePol = pd.LoadBalancingDecodePolicy{}
	}
	return &pdRouter{
		cache:                 c,
		prefillPolicy:         prefillPol,
		decodePolicy:          decodePol,
		prefixCacheIndexer:    idx,
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		tokenLoadTracker:      pd.NewTokenLoadTracker(),
		bucketServe:           pd.NewBucketServeTracker(),
		selectionCounts:       map[string]int64{},
	}
}

func assertPDPairingAndBudget(t *testing.T, candidates []*v1.Pod, k int, expectedEncodersPerRS int) (prefillCount, decodeCount, encoderCount, combinedCount int, rolesets map[string]struct{}) {
	t.Helper()
	prefillsByRS := map[string]int{}
	decodesByRS := map[string]int{}
	encodersByRS := map[string]int{}
	seenPods := map[*v1.Pod]struct{}{}
	rolesets = map[string]struct{}{}

	for _, pod := range candidates {
		require.NotNil(t, pod)
		_, dup := seenPods[pod]
		require.False(t, dup, "duplicate pod %s in candidates", pod.Name)
		seenPods[pod] = struct{}{}

		rsID := pod.Labels[PDRoleSetIdentifier]
		role := pod.Labels[PDRoleIdentifier]
		switch role {
		case PDRolePrefill:
			prefillCount++
			prefillsByRS[rsID]++
			rolesets[rsID] = struct{}{}
		case PDRoleDecode:
			decodeCount++
			decodesByRS[rsID]++
			rolesets[rsID] = struct{}{}
		case encode.Role:
			encoderCount++
			encodersByRS[rsID]++
		default:
			combinedCount++
		}
	}

	assert.LessOrEqual(t, prefillCount, k, "prefill candidates must be <= K")
	assert.LessOrEqual(t, decodeCount+combinedCount, k, "decode + combined targets must be <= K")
	for rsID := range rolesets {
		assert.GreaterOrEqual(t, prefillsByRS[rsID], 1, "kept roleset %s must have >=1 prefill", rsID)
		assert.GreaterOrEqual(t, decodesByRS[rsID], 1, "kept roleset %s must have >=1 decode", rsID)
		if expectedEncodersPerRS >= 0 {
			assert.Equal(t, expectedEncodersPerRS, encodersByRS[rsID], "kept roleset %s must retain all its encoders", rsID)
		}
	}
	for rsID := range encodersByRS {
		_, kept := rolesets[rsID]
		assert.True(t, kept, "encoder from non-kept roleset %s must not leak into candidates", rsID)
	}
	return prefillCount, decodeCount, encoderCount, combinedCount, rolesets
}

func TestPDSelectCandidates_TopologiesPairingAndEncoders(t *testing.T) {
	topologies := []struct {
		name          string
		rolesets      int
		replicas      int
		encodersPerRS int
		k             int
		wantPath      string
	}{
		{"50rsx5_K64", 50, 5, 2, 64, pdCandidatePathRanked},
		{"1rsx250_K64", 1, 250, 2, 64, pdCandidatePathRanked},
		{"200rsx1_K64", 200, 1, 1, 64, pdCandidatePathRanked},
		{"50rsx5_K1", 50, 5, 1, 1, pdCandidatePathNoRankedSlot},
		{"1rsx250_K1", 1, 250, 1, 1, pdCandidatePathNoRankedSlot},
	}

	for _, tt := range topologies {
		t.Run(tt.name, func(t *testing.T) {
			tc := &candidateStageTestCache{Cache: cache.NewForTest()}
			r := newCandidateTestRouter(tc, nil, nil, nil)
			pods := makeFleetWithEncoders(tt.rolesets, tt.replicas, tt.encodersPerRS)
			origCopy := append([]*v1.Pod(nil), pods...)

			ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hello world", "req-topo", "user")
			defer ctx.Delete()

			cands, path := r.SelectCandidates(ctx, pods, tt.k)
			assert.Equal(t, tt.wantPath, path)
			assert.Equal(t, origCopy, pods, "SelectCandidates must not mutate readyPods")
			assert.Less(t, len(cands), len(pods))
			assert.Zero(t, tc.redisReadCalls.Load(), "SelectCandidates must not call Redis GetPodsRunningRequests")

			pCnt, dCnt, eCnt, cCnt, keptRS := assertPDPairingAndBudget(t, cands, tt.k, tt.encodersPerRS)
			assert.Equal(t, tt.k, pCnt)
			assert.Equal(t, tt.k, dCnt)
			assert.Equal(t, len(keptRS)*tt.encodersPerRS, eCnt)
			assert.Zero(t, cCnt)
			if tt.name == "50rsx5_K64" {
				// minPerRS=2 so K=64 selects 32 rolesets with 2P+2D each.
				assert.Equal(t, 32, len(keptRS))
			}
		})
	}
}

func TestPDSelectCandidates_BypassCases(t *testing.T) {
	t.Run("k_non_positive", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		pods := makeFleetWithEncoders(10, 10, 0)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 0)
		assert.Equal(t, len(pods), len(cands))
		assert.Empty(t, path)
	})

	t.Run("small_fleet_within_k", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		// 64P + 64D fits within K=64 -> full bypass.
		pods := makeFleetWithEncoders(8, 8, 1)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 64)
		assert.Equal(t, len(pods), len(cands))
		assert.Empty(t, path)
	})

	t.Run("bucket_serve_enabled_bypasses", func(t *testing.T) {
		withPromptLengthBucketing(t, true)
		withBucketServe(t, true, pd.BucketModeRPS)

		r := newCandidateTestRouter(nil, nil, nil, nil)
		pods := makeFleetWithEncoders(50, 5, 0)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req-bs", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 16)
		assert.Equal(t, len(pods), len(cands), "bucket_serve must bypass candidate narrowing so bucketServeGroups sees full fleet")
		assert.Empty(t, path)
	})

	t.Run("no_eligible_rolesets_bypasses", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		// Only prefill pods, no decode pods -> no eligible roleset.
		pods := benchmarkPDPods(VLLMEngine, PDRolePrefill, 10, 10)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 8)
		assert.Equal(t, len(pods), len(cands))
		assert.Empty(t, path)
	})

	t.Run("unlabeled_pods_bypass", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		pods := make([]*v1.Pod, 100)
		for i := range pods {
			pods[i] = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("plain-%d", i)}}
		}
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 8)
		assert.Equal(t, len(pods), len(cands))
		assert.Empty(t, path)
	})
}

func TestPDSelectCandidates_PromptLengthBucketingAndCombinedBudget(t *testing.T) {
	withPromptLengthBucketing(t, true)

	t.Run("filters_rolesets_by_bucket_before_narrowing_and_caps_decode_plus_combined_at_k", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		annoMatch := pdConfigAnnotation(0, 99999, false)
		annoMiss := pdConfigAnnotation(100000, 200000, false)
		annoCombined := pdConfigAnnotation(0, 99999, true)

		var pods []*v1.Pod
		// 10 matching rolesets x 3P/3D = 30P + 30D
		for rs := 0; rs < 10; rs++ {
			rsID := fmt.Sprintf("rs-match-%d", rs)
			for rep := 0; rep < 3; rep++ {
				pods = append(pods,
					makePDPod(fmt.Sprintf("pm-%d-%d", rs, rep), rsID, PDRolePrefill, map[string]string{constants.ModelAnnoConfig: annoMatch}),
					makePDPod(fmt.Sprintf("dm-%d-%d", rs, rep), rsID, PDRoleDecode, map[string]string{constants.ModelAnnoConfig: annoMatch}),
				)
			}
		}
		// 20 non-matching rolesets x 3P/3D = 60P + 60D
		for rs := 0; rs < 20; rs++ {
			rsID := fmt.Sprintf("rs-miss-%d", rs)
			for rep := 0; rep < 3; rep++ {
				pods = append(pods,
					makePDPod(fmt.Sprintf("px-%d-%d", rs, rep), rsID, PDRolePrefill, map[string]string{constants.ModelAnnoConfig: annoMiss}),
					makePDPod(fmt.Sprintf("dx-%d-%d", rs, rep), rsID, PDRoleDecode, map[string]string{constants.ModelAnnoConfig: annoMiss}),
				)
			}
		}
		// 10 suitable combined pods
		for i := 0; i < 10; i++ {
			pods = append(pods, makePDPod(fmt.Sprintf("comb-%d", i), "rs-comb", "combined", map[string]string{constants.ModelAnnoConfig: annoCombined}))
		}

		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "short prompt", "req-bkt", "user")
		defer ctx.Delete()

		const k = 8
		cands, path := r.SelectCandidates(ctx, pods, k)
		assert.Equal(t, pdCandidatePathRanked, path)
		pCnt, dCnt, _, cCnt, keptRS := assertPDPairingAndBudget(t, cands, k, -1)
		assert.Equal(t, k, pCnt)
		assert.GreaterOrEqual(t, dCnt, 1)
		assert.GreaterOrEqual(t, cCnt, 1, "combined spillover pods should receive budget share")
		assert.LessOrEqual(t, dCnt+cCnt, k, "decode + combined targets must stay <= K")
		for rsID := range keptRS {
			assert.Contains(t, rsID, "rs-match-", "only matching-bucket rolesets may be selected")
		}
	})

	t.Run("bucket_mismatch_with_no_combined_bypasses_and_preserves_503_error", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		annoMiss := pdConfigAnnotation(100000, 200000, false)
		var pods []*v1.Pod
		for rs := 0; rs < 20; rs++ {
			rsID := fmt.Sprintf("rs-miss-%d", rs)
			for rep := 0; rep < 5; rep++ {
				pods = append(pods,
					makePDPod(fmt.Sprintf("px-%d-%d", rs, rep), rsID, PDRolePrefill, map[string]string{constants.ModelAnnoConfig: annoMiss}),
					makePDPod(fmt.Sprintf("dx-%d-%d", rs, rep), rsID, PDRoleDecode, map[string]string{constants.ModelAnnoConfig: annoMiss}),
				)
			}
		}

		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "short prompt", "req-miss", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 8)
		assert.Equal(t, len(pods), len(cands), "bucket mismatch with no combined must bypass")
		assert.Empty(t, path)

		_, _, err := r.filterPrefillDecodePods(ctx, cands)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no prompt-length bucket matches prompt length")
	})

	t.Run("combined_only_exceeds_k_narrows_to_k", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		annoMiss := pdConfigAnnotation(100000, 200000, false)
		annoCombined := pdConfigAnnotation(0, 99999, true)
		var pods []*v1.Pod
		pods = append(pods,
			makePDPod("px-0", "rs-miss", PDRolePrefill, map[string]string{constants.ModelAnnoConfig: annoMiss}),
			makePDPod("dx-0", "rs-miss", PDRoleDecode, map[string]string{constants.ModelAnnoConfig: annoMiss}),
		)
		for i := 0; i < 20; i++ {
			pods = append(pods, makePDPod(fmt.Sprintf("comb-%d", i), "rs-comb", "combined", map[string]string{constants.ModelAnnoConfig: annoCombined}))
		}
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "short prompt", "req-comb-only", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 6)
		assert.Equal(t, pdCandidatePathRanked, path)
		assert.Len(t, cands, 6)
		for _, pod := range cands {
			assert.Equal(t, "combined", pod.Labels[PDRoleIdentifier])
		}
	})
}

func TestPDSelectCandidates_PrefixSeedingMatchesPolicyAndReusesSafely(t *testing.T) {
	const model = "seed-model"
	const prompt = "shared long prefix prompt for pd candidate seeding test"

	policies := []struct {
		name  string
		build func(tok tokenizer.Tokenizer, idx *prefixcacheindexer.PrefixHashTable, c cache.Cache, tlt *pd.TokenLoadTracker) pd.PrefillScorePolicy
	}{
		{
			name: "prefix_cache",
			build: func(tok tokenizer.Tokenizer, idx *prefixcacheindexer.PrefixHashTable, _ cache.Cache, _ *pd.TokenLoadTracker) pd.PrefillScorePolicy {
				return pd.NewPrefixCachePrefillPolicy(tok, idx)
			},
		},
		{
			name: "conductor",
			build: func(tok tokenizer.Tokenizer, idx *prefixcacheindexer.PrefixHashTable, c cache.Cache, _ *pd.TokenLoadTracker) pd.PrefillScorePolicy {
				return pd.NewConductorPrefillPolicy(tok, idx, c, pd.NewConductorPrefillPolicyConfig())
			},
		},
		{
			name: "hybrid_cache_load",
			build: func(tok tokenizer.Tokenizer, idx *prefixcacheindexer.PrefixHashTable, _ cache.Cache, tlt *pd.TokenLoadTracker) pd.PrefillScorePolicy {
				return pd.NewHybridCacheLoadPrefillPolicy(tok, idx, tlt)
			},
		},
	}

	for _, pol := range policies {
		t.Run(pol.name+"_same_tokenizer_and_index_reused", func(t *testing.T) {
			tok := &countingTokenizer{inner: tokenizer.NewCharacterTokenizer()}
			idx := prefixcacheindexer.NewPrefixHashTable()
			tlt := pd.NewTokenLoadTracker()
			c := cache.NewForTest()

			pods := makeFleetWithEncoders(30, 4, 0) // 120P + 120D
			// Seed prefix onto rs-017/p-017-002 using the exact same tokenizer.
			rawTokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(prompt)
			require.NoError(t, err)
			hashes := idx.GetPrefixHashes(rawTokens)
			holderKey := utils.GeneratePodKey("", "p-017-002")
			idx.AddPrefix(hashes, model, holderKey)

			prefillPol := pol.build(tok, idx, c, tlt)
			r := newCandidateTestRouter(c, prefillPol, pd.LoadBalancingDecodePolicy{}, idx)
			r.tokenLoadTracker = tlt

			ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-seed", "user")
			defer ctx.Delete()

			cands, path := r.SelectCandidates(ctx, pods, 8)
			assert.Equal(t, pdCandidatePathRanked, path)
			require.NotNil(t, ctx.SeededPrefixMatch())
			assert.Equal(t, idx, ctx.SeededPrefixMatch().Index)
			assert.Equal(t, model, ctx.SeededPrefixMatch().Model)
			assert.Equal(t, int64(1), tok.calls.Load(), "SelectCandidates should tokenize once with policy's own tokenizer")

			// Verify prefix holder survived candidate narrowing.
			foundHolder := false
			for _, p := range cands {
				if p.Name == "p-017-002" {
					foundHolder = true
					break
				}
			}
			assert.True(t, foundHolder, "prefix holder pod must be retained in narrowed candidates")

			// Now run filterPrefillDecodePods: Prepare must reuse SeededPrefixMatch with ZERO extra tokenization.
			selectedP, selectedD, err := r.filterPrefillDecodePods(ctx, cands)
			require.NoError(t, err)
			require.NotNil(t, selectedP)
			require.NotNil(t, selectedD)
			assert.Equal(t, "p-017-002", selectedP.Name)
			assert.Equal(t, int64(1), tok.calls.Load(), "Prepare must reuse SeededPrefixMatch without a second tokenization")
		})
	}

	t.Run("mismatched_index_or_model_falls_back_to_policy_match", func(t *testing.T) {
		tok := &countingTokenizer{inner: tokenizer.NewCharacterTokenizer()}
		realIdx := prefixcacheindexer.NewPrefixHashTable()
		otherIdx := prefixcacheindexer.NewPrefixHashTable()

		rawTokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(prompt)
		require.NoError(t, err)
		realIdx.AddPrefix(realIdx.GetPrefixHashes(rawTokens), model, utils.GeneratePodKey("", "p-000-000"))

		pol := pd.NewPrefixCachePrefillPolicy(tok, realIdx)
		pod := makePDPod("p-000-000", "rs-000", PDRolePrefill, nil)
		readyMap := map[string]struct{}{utils.GeneratePodKey("", "p-000-000"): {}}

		// Wrong index pointer -> must fall back to realIdx.MatchPrefix.
		ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-mismatch-idx", "user")
		defer ctx.Delete()
		ctx.SetSeededPrefixMatch(&types.SeededPrefixMatch{
			Index:   otherIdx,
			Model:   model,
			LoRAID:  -1,
			Pods:    readyMap,
			Matches: map[string]int{},
		})
		scorer, err := pol.Prepare(ctx, []*v1.Pod{pod}, readyMap)
		require.NoError(t, err)
		assert.Equal(t, int64(1), tok.calls.Load(), "mismatched index must fall back to policy's own tokenization & MatchPrefix")
		assert.NotEmpty(t, scorer.PrefixHashes())

		// Wrong model -> must also fall back.
		ctx2 := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-mismatch-model", "user")
		defer ctx2.Delete()
		ctx2.SetSeededPrefixMatch(&types.SeededPrefixMatch{
			Index:   realIdx,
			Model:   "other-model",
			LoRAID:  -1,
			Pods:    readyMap,
			Matches: map[string]int{},
		})
		_, err = pol.Prepare(ctx2, []*v1.Pod{pod}, readyMap)
		require.NoError(t, err)
		assert.Equal(t, int64(2), tok.calls.Load(), "mismatched model must fall back to policy's own MatchPrefix")
	})

	t.Run("non_prefix_policy_does_not_seed", func(t *testing.T) {
		r := newCandidateTestRouter(nil, pd.NewLeastRequestPrefillPolicy(), pd.LoadBalancingDecodePolicy{}, nil)
		pods := makeFleetWithEncoders(10, 10, 0)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-lr", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 8)
		assert.Equal(t, pdCandidatePathRanked, path)
		assert.Less(t, len(cands), len(pods))
		assert.Nil(t, ctx.SeededPrefixMatch())
		assert.Nil(t, ctx.PrefixTokens())
	})
}

func TestPDSelectCandidates_SoftExcludeInflightSaturatedDecodes(t *testing.T) {
	t.Run("ranked_slots_avoid_saturated_decodes", func(t *testing.T) {
		pods := makeFleetWithEncoders(1, 20, 0) // 1 roleset x 20P/20D
		approx := make(map[string]int64, 20)
		for i := 0; i < 20; i++ {
			key := utils.GeneratePodKey("", fmt.Sprintf("d-000-%03d", i))
			if i < 16 {
				approx[key] = 10 // at cap
			} else {
				approx[key] = 0 // idle
			}
		}
		tc := &candidateStageTestCache{Cache: cache.NewForTest(), approxCounts: approx}
		r := newCandidateTestRouter(tc, pd.NewLeastRequestPrefillPolicy(), pd.LoadBalancingDecodePolicy{}, nil)

		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req-soft", "user")
		defer ctx.Delete()
		ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: 10}

		// With K=4 (2 ranked + 2 random) and 4 idle pods among 20 (scout covers all 20),
		// both ranked slots must pick idle pods so every trial includes >=2 unsaturated decodes.
		for trial := 0; trial < 20; trial++ {
			cands, path := r.SelectCandidates(ctx, pods, 4)
			assert.Equal(t, pdCandidatePathRanked, path)
			idleDecodes := 0
			for _, p := range cands {
				if p.Labels[PDRoleIdentifier] == PDRoleDecode {
					if approx[utils.GeneratePodKey(p.Namespace, p.Name)] < 10 {
						idleDecodes++
					}
				}
			}
			assert.GreaterOrEqual(t, idleDecodes, 2, "trial %d: ranked decode slots must avoid in-memory saturated pods", trial)
		}
		assert.Zero(t, tc.redisReadCalls.Load())
	})

	t.Run("all_saturated_falls_back_to_unfiltered_scout", func(t *testing.T) {
		pods := makeFleetWithEncoders(1, 20, 0)
		approx := make(map[string]int64, 20)
		for i := 0; i < 20; i++ {
			approx[utils.GeneratePodKey("", fmt.Sprintf("d-000-%03d", i))] = 10
		}
		tc := &candidateStageTestCache{Cache: cache.NewForTest(), approxCounts: approx}
		r := newCandidateTestRouter(tc, pd.NewLeastRequestPrefillPolicy(), pd.LoadBalancingDecodePolicy{}, nil)

		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req-all-sat", "user")
		defer ctx.Delete()
		ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: 10}

		cands, path := r.SelectCandidates(ctx, pods, 4)
		assert.Equal(t, pdCandidatePathRanked, path)
		pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 4, -1)
		assert.Equal(t, 4, pCnt)
		assert.Equal(t, 4, dCnt, "all-saturated soft-exclude must fall back to unfiltered scout so Redis owns the 429")
	})
}

func TestPDSelectCandidates_RandomHalfExploresBeyondLowestLoad(t *testing.T) {
	pods := makeFleetWithEncoders(1, 20, 0) // 1rs x 20P/20D
	approx := make(map[string]int64, 20)
	for i := 0; i < 20; i++ {
		key := utils.GeneratePodKey("", fmt.Sprintf("d-000-%03d", i))
		if i < 4 {
			approx[key] = 0 // always win ranked slots
		} else {
			approx[key] = 50 // high load; can only enter via random half
		}
	}
	tc := &candidateStageTestCache{Cache: cache.NewForTest(), approxCounts: approx}
	r := newCandidateTestRouter(tc, pd.NewLeastRequestPrefillPolicy(), pd.LoadBalancingDecodePolicy{}, nil)

	ctx := types.NewRoutingContext(context.Background(), RouterPD, "m1", "hi", "req-rand", "user")
	defer ctx.Delete()

	sawHighLoadDecode := false
	for trial := 0; trial < 30; trial++ {
		cands, _ := r.SelectCandidates(ctx, pods, 4) // 2 ranked + 2 random
		for _, p := range cands {
			if p.Labels[PDRoleIdentifier] == PDRoleDecode && approx[utils.GeneratePodKey(p.Namespace, p.Name)] == 50 {
				sawHighLoadDecode = true
				break
			}
		}
		if sawHighLoadDecode {
			break
		}
	}
	assert.True(t, sawHighLoadDecode, "50/50 random half must continue sampling non-top-ranked decode pods")
}

func TestPDSelectCandidates_BoundedScoutRetainsPrefixHoldersBeyond8K(t *testing.T) {
	const model = "scout-8k-model"
	const prompt = "bounded scout prefix holder test prompt"

	rawTokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(prompt)
	require.NoError(t, err)

	t.Run("many_rolesets_exceeds_8K_scout_retains_prefix_holder_roleset", func(t *testing.T) {
		idx := prefixcacheindexer.NewPrefixHashTable()
		pods := makeFleetWithEncoders(100, 1, 0) // 100 rolesets > 8*K (K=4 -> scoutLimit=16)
		hashes := idx.GetPrefixHashes(rawTokens)
		idx.AddPrefix(hashes, model, utils.GeneratePodKey("", "p-073-000"))
		idx.AddPrefix(hashes[:max(1, len(hashes)/2)], model, utils.GeneratePodKey("", "p-041-000"))

		pol := pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), idx)
		r := newCandidateTestRouter(nil, pol, pd.LoadBalancingDecodePolicy{}, idx)
		ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-scout-rs", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 4)
		assert.Equal(t, pdCandidatePathRanked, path)
		pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 4, -1)
		assert.Equal(t, 4, pCnt)
		assert.Equal(t, 4, dCnt)

		foundBestHolder := false
		for _, p := range cands {
			if p.Name == "p-073-000" {
				foundBestHolder = true
				break
			}
		}
		assert.True(t, foundBestHolder, "best prefix-holder roleset must enter bounded roleset scout and win a ranked slot")
	})

	t.Run("single_roleset_exceeds_8K_scout_retains_prefix_holder_and_uses_token_load", func(t *testing.T) {
		idx := prefixcacheindexer.NewPrefixHashTable()
		tlt := pd.NewTokenLoadTracker()
		pods := makeFleetWithEncoders(1, 100, 0) // 100 prefills > 8*K (K=4 -> scoutLimit=16)
		hashes := idx.GetPrefixHashes(rawTokens)
		idx.AddPrefix(hashes, model, utils.GeneratePodKey("", "p-000-088"))
		idx.AddPrefix(hashes[:max(1, len(hashes)/2)], model, utils.GeneratePodKey("", "p-000-022"))
		// Charge decode token load on most decode pods so inMemoryDecodeLoad exercises token_load.
		for i := 0; i < 95; i++ {
			tlt.AcquireDecodeWithTTL(fmt.Sprintf("dreq-%d", i), utils.GeneratePodKey("", fmt.Sprintf("d-000-%03d", i)), 5000, 0)
		}

		pol := pd.NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(), idx, tlt)
		r := newCandidateTestRouter(nil, pol, pd.TokenLoadDecodePolicy{}, idx)
		r.tokenLoadTracker = tlt
		ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "req-scout-1rs", "user")
		defer ctx.Delete()

		cands, path := r.SelectCandidates(ctx, pods, 4)
		assert.Equal(t, pdCandidatePathRanked, path)
		pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 4, -1)
		assert.Equal(t, 4, pCnt)
		assert.Equal(t, 4, dCnt)

		foundBestHolder := false
		for _, p := range cands {
			if p.Name == "p-000-088" {
				foundBestHolder = true
				break
			}
		}
		assert.True(t, foundBestHolder, "best prefix-holder prefill pod must enter bounded prefill scout and win a ranked slot")
	})

	t.Run("splitPDTargetBudget_edge_cases", func(t *testing.T) {
		p, d, c := splitPDTargetBudget(1, 10, 10, 5)
		assert.Equal(t, 1, p)
		assert.Equal(t, 1, d)
		assert.Equal(t, 0, c)

		p, d, c = splitPDTargetBudget(8, 20, 2, 10)
		assert.Equal(t, 8, p)
		assert.Equal(t, 2, d)
		assert.Equal(t, 6, c)
		assert.LessOrEqual(t, d+c, 8)
	})
}

func TestPDSelectCandidates_QuotaOneExplorationKeepsHoldersAndSoftExcludes(t *testing.T) {
	t.Run("tied_multi_holder_fleet_quota_one_exploration_retains_local_holders", func(t *testing.T) {
		const model = "tied-holders-model"
		const prompt = "tied shared prefix prompt across rolesets"

		idx := prefixcacheindexer.NewPrefixHashTable()
		rawTokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(prompt)
		require.NoError(t, err)
		hashes := idx.GetPrefixHashes(rawTokens)

		// 10 rolesets, each with 3 prefills and 1 decode -> minPerRS=1, K=4 keeps
		// 4 rolesets (2 ranked + 2 exploration), each with prefill quota=1.
		// Every roleset has exactly 1 prefix holder among its 3 prefills (all tied).
		var pods []*v1.Pod
		holders := make(map[string]bool, 10)
		for rs := 0; rs < 10; rs++ {
			rsID := fmt.Sprintf("rs-%02d", rs)
			for rep := 0; rep < 3; rep++ {
				name := fmt.Sprintf("p-%02d-%d", rs, rep)
				pods = append(pods, makePDPod(name, rsID, PDRolePrefill, nil))
				if rep == 0 {
					idx.AddPrefix(hashes, model, utils.GeneratePodKey("", name))
					holders[name] = true
				}
			}
			pods = append(pods, makePDPod(fmt.Sprintf("d-%02d-0", rs), rsID, PDRoleDecode, nil))
		}

		pol := pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), idx)
		r := newCandidateTestRouter(nil, pol, pd.LoadBalancingDecodePolicy{}, idx)

		for trial := 0; trial < 20; trial++ {
			ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, fmt.Sprintf("req-tied-%d", trial), "user")
			cands, path := r.SelectCandidates(ctx, pods, 4)
			ctx.Delete()
			assert.Equal(t, pdCandidatePathRanked, path)
			pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 4, -1)
			assert.Equal(t, 4, pCnt)
			assert.Equal(t, 4, dCnt)
			for _, p := range cands {
				if p.Labels[PDRoleIdentifier] == PDRolePrefill {
					assert.True(t, holders[p.Name], "trial %d: quota=1 exploration roleset must still retain its local prefix holder, got %s", trial, p.Name)
				}
			}
		}
	})

	t.Run("quota_one_exploration_decode_soft_excludes_saturated", func(t *testing.T) {
		// 10 rolesets, each with 1 prefill and 4 decodes; K=3 keeps decodeBudget < 4 so
		// minPerRS=1 and targetRolesets=3 (1 ranked + 2 exploration) with decode quota=1
		// per roleset. In every roleset, 3 of 4 decodes are at cap=10 and 1 is idle (0).
		var pods []*v1.Pod
		approx := make(map[string]int64, 40)
		for rs := 0; rs < 10; rs++ {
			rsID := fmt.Sprintf("rs-%02d", rs)
			pods = append(pods, makePDPod(fmt.Sprintf("p-%02d-0", rs), rsID, PDRolePrefill, nil))
			for rep := 0; rep < 4; rep++ {
				name := fmt.Sprintf("d-%02d-%d", rs, rep)
				pods = append(pods, makePDPod(name, rsID, PDRoleDecode, nil))
				if rep == 0 {
					approx[utils.GeneratePodKey("", name)] = 0
				} else {
					approx[utils.GeneratePodKey("", name)] = 10
				}
			}
		}
		tc := &candidateStageTestCache{Cache: cache.NewForTest(), approxCounts: approx}
		r := newCandidateTestRouter(tc, pd.NewLeastRequestPrefillPolicy(), pd.LoadBalancingDecodePolicy{}, nil)

		for trial := 0; trial < 20; trial++ {
			ctx := types.NewRoutingContext(context.Background(), RouterPD, "m-exp-dec", "hi", fmt.Sprintf("req-exp-dec-%d", trial), "user")
			ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: 10}
			cands, path := r.SelectCandidates(ctx, pods, 3)
			ctx.Delete()
			assert.Equal(t, pdCandidatePathRanked, path)
			pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 3, -1)
			assert.Equal(t, 3, pCnt)
			assert.Equal(t, 3, dCnt)
			for _, p := range cands {
				if p.Labels[PDRoleIdentifier] == PDRoleDecode {
					assert.Less(t, approx[utils.GeneratePodKey(p.Namespace, p.Name)], int64(10),
						"trial %d: quota=1 exploration roleset must steer decode slot away from saturated pod %s", trial, p.Name)
				}
			}
		}
	})

	t.Run("invalid_decode_policy_narrows_instead_of_full_fleet_and_nil_policy_prepare_errors", func(t *testing.T) {
		r := newCandidateTestRouter(nil, nil, nil, nil)
		pods := makeFleetWithEncoders(20, 5, 0) // 100P + 100D
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "m-bad-pol", "hi", "req-bad-pol", "user")
		defer ctx.Delete()
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			Routing: &types.RoutingConfig{DecodeScorePolicy: "nonexistent_policy"},
		}

		cands, path := r.SelectCandidates(ctx, pods, 8)
		assert.Equal(t, pdCandidatePathEstimateFailed, path)
		pCnt, dCnt, _, _, _ := assertPDPairingAndBudget(t, cands, 8, -1)
		assert.Equal(t, 8, pCnt)
		assert.Equal(t, 8, dCnt)

		// Nil tokenizer/indexer in Prepare must return an error rather than panic.
		brokenPol := pd.NewPrefixCachePrefillPolicy(nil, nil)
		_, err := brokenPol.Prepare(ctx, pods[:1], map[string]struct{}{"p": {}})
		require.Error(t, err)
	})
}
