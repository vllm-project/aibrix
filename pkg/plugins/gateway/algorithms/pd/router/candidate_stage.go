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
	"math/rand/v2"
	"sort"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const (
	pdCandidatePathRanked         = "ranked"
	pdCandidatePathEstimateFailed = "random_estimate_failed"
	pdCandidatePathNoRankedSlot   = "random_no_ranked_slot"
	pdCandidateScoutFactor        = 4
	pdRolesetLoadSampleCap        = 4
)

var _ types.CandidateSelector = (*pdRouter)(nil)

type pdCandidateRoleset struct {
	id           string
	prefills     []*v1.Pod
	decodes      []*v1.Pod
	encoders     []*v1.Pod
	bestMatchPct int
	isRanked     bool
}

type pdCandidatePools struct {
	eligible     []pdCandidateRoleset
	combinedPods []*v1.Pod
	totalP       int
	totalD       int
}

// SelectCandidates runs PD's RoleSet-aligned two-level candidate stage (RFC #3620 Phase 2).
//
// Invariants:
//   - Zero Redis reads: uses only in-memory trackers, prefix index, and
//     cache.ApproxRunningRequestsProvider (process memory).
//   - Never mutates readyPods.
//   - Bypasses unchanged when k <= 0, bucket_serve is enabled for the request,
//     no eligible PD rolesets or combined pods match, or the fleet already fits
//     within the budget (N_p <= k && N_d + N_c <= k).
//   - Guarantees RoleSet pairing (every kept roleset contributes >= 1 prefill
//     and >= 1 decode), preserves all encoders for every kept roleset, and
//     bounds |P_cand| <= k and |D_cand| + |C_cand| <= k so downstream Redis
//     aibrix:rrq:* reads stay strictly <= k.
func (r *pdRouter) SelectCandidates(ctx *types.RoutingContext, readyPods []*v1.Pod, k int) ([]*v1.Pod, string) {
	if ctx == nil || k <= 0 || len(readyPods) == 0 {
		return readyPods, ""
	}
	// selectTargetPod runs ResolveRoutingOverrides before calling SelectCandidates,
	// whereas SetPDOverrides is only called upon entering Route. Read RoutingOverrides().PD
	// directly so per-profile overrides (PromptLengthBucketing, BucketServe, MinMatchPct,
	// TokenLoad.KVWeight) apply without mutating PDLegState.
	pdOverrides := ctx.RoutingOverrides().PD
	if pdOverrides.BucketServe {
		// bucketServeGroups requires full-fleet per-roleset replica counts to keep
		// adaptive cut points and PDBucketServeBandMax gauges stable.
		return readyPods, ""
	}

	pools := collectPDCandidatePools(ctx, readyPods, pdOverrides.PromptLengthBucketing)
	totalC := len(pools.combinedPods)
	if len(pools.eligible) == 0 && totalC == 0 {
		// Preserve unlabeled fixtures, not-ready errors, and exact prompt-length
		// bucket-mismatch 503 messages without tripping a false 429.
		return readyPods, ""
	}
	if pools.totalP <= k && pools.totalD+totalC <= k {
		return readyPods, ""
	}

	prefillPol, decodePol, policyErr := r.effectiveScorePolicies(ctx)
	if policyErr != nil {
		// Do not fall through to the full ready set on an invalid routingConfig
		// decodeScorePolicy: narrow with default policies so narrowPDToInflightCapacity
		// still stays O(K) before Route surfaces the exact config error.
		prefillPol = nil
		decodePol = loadBalancingDecodePolicy
	}
	if decodePol == nil {
		decodePol = loadBalancingDecodePolicy
	}

	path := pdCandidatePathRanked
	if k == 1 {
		path = pdCandidatePathNoRankedSlot
	} else if policyErr != nil {
		path = pdCandidatePathEstimateFailed
	}
	inflightLimit := pdInflightCap(ctx)

	if len(pools.eligible) == 0 {
		// Prompt-length bucketing matched no PD rolesets, only >k combined spillover pods.
		return r.selectCombinedCandidates(pools.combinedPods, k, inflightLimit), path
	}

	prefillBudget, decodeBudget, combinedBudget := splitPDTargetBudget(k, pools.totalP, pools.totalD, totalC)

	var matchedPods map[string]int
	var prefillKeys map[*v1.Pod]string
	if k >= 2 {
		var seedFailed bool
		matchedPods, prefillKeys, seedFailed = seedPDPrefillCandidates(ctx, prefillPol, pools.eligible, pools.totalP)
		if seedFailed && path == pdCandidatePathRanked {
			path = pdCandidatePathEstimateFailed
		}
		attachRolesetPrefixMatches(pools.eligible, prefillKeys, matchedPods, prefillPol, pdOverrides.MinMatchPct)
	}

	usesPrefillTokenLoad := pd.UsesTokenLoad(prefillPol)
	usesDecodeTokenLoad := pd.UsesDecodeTokenLoad(decodePol)
	kvWeight := pdOverrides.TokenLoad.KVWeight

	keptRolesets := r.selectCandidateRolesets(
		pools.eligible, k, prefillBudget, decodeBudget,
		prefillKeys, usesPrefillTokenLoad, usesDecodeTokenLoad, kvWeight, inflightLimit,
	)

	prefillQuotas := allocateRolesetQuotas(keptRolesets, prefillBudget, true)
	decodeQuotas := allocateRolesetQuotas(keptRolesets, decodeBudget, false)

	totalEncoders := 0
	for i := range keptRolesets {
		totalEncoders += len(keptRolesets[i].encoders)
	}
	out := make([]*v1.Pod, 0, prefillBudget+decodeBudget+combinedBudget+totalEncoders)
	for i := range keptRolesets {
		rs := &keptRolesets[i]
		pPods := r.selectPrefillCandidatesForRoleset(
			rs, prefillQuotas[i], k, prefillKeys, matchedPods,
			prefillPol, pdOverrides.MinMatchPct, usesPrefillTokenLoad, kvWeight,
		)
		dPods := r.selectDecodeCandidatesForRoleset(
			rs, decodeQuotas[i], k, usesDecodeTokenLoad, inflightLimit,
		)
		out = append(out, pPods...)
		out = append(out, dPods...)
		out = append(out, rs.encoders...)
	}

	if combinedBudget > 0 {
		out = append(out, r.selectCombinedCandidates(pools.combinedPods, combinedBudget, inflightLimit)...)
	}
	return out, path
}

func pdInflightCap(ctx *types.RoutingContext) int64 {
	if ctx == nil || ctx.ConfigProfile == nil {
		return 0
	}
	return ctx.ConfigProfile.RequestsInflight
}

func collectPDCandidatePools(ctx *types.RoutingContext, readyPods []*v1.Pod, bucketing bool) pdCandidatePools {
	var promptLength int
	if bucketing {
		promptLength, _ = ctx.PromptLength()
	}
	var cfgCache pd.RoutingConfigCache

	byRoleset := make(map[string]*pdRolesetBucket)
	var encodersByRoleset map[string][]*v1.Pod
	var combinedPods []*v1.Pod

	for _, pod := range readyPods {
		if pod == nil {
			continue
		}
		roleSetID, roleID, ok := pd.RoutablePDLabels(pod)
		if !ok {
			continue
		}
		switch roleID {
		case PDRolePrefill, PDRoleDecode:
			if bucketing && !pd.IsPromptLengthInBucket(cfgCache.Resolve(ctx, pod), promptLength) {
				continue
			}
			b := byRoleset[roleSetID]
			if b == nil {
				b = &pdRolesetBucket{}
				byRoleset[roleSetID] = b
			}
			if roleID == PDRolePrefill {
				b.prefills = append(b.prefills, pod)
			} else {
				b.decodes = append(b.decodes, pod)
			}
		default:
			if encode.IsEncoder(pod) {
				if encodersByRoleset == nil {
					encodersByRoleset = make(map[string][]*v1.Pod)
				}
				encodersByRoleset[roleSetID] = append(encodersByRoleset[roleSetID], pod)
			} else if bucketing {
				cfg := cfgCache.Resolve(ctx, pod)
				if pd.IsCombinedRoutingConfig(cfg) && pd.IsPromptLengthInBucket(cfg, promptLength) {
					combinedPods = append(combinedPods, pod)
				}
			}
		}
	}

	eligible := make([]pdCandidateRoleset, 0, len(byRoleset))
	totalP, totalD := 0, 0
	for id, b := range byRoleset {
		if !pd.IsRolesetEligible(len(b.prefills), len(b.decodes)) {
			continue
		}
		eligible = append(eligible, pdCandidateRoleset{
			id:       id,
			prefills: b.prefills,
			decodes:  b.decodes,
			encoders: encodersByRoleset[id],
		})
		totalP += len(b.prefills)
		totalD += len(b.decodes)
	}
	return pdCandidatePools{
		eligible:     eligible,
		combinedPods: combinedPods,
		totalP:       totalP,
		totalD:       totalD,
	}
}

// splitPDTargetBudget caps prefill candidates at k and decode+combined targets
// together at k so downstream Redis aibrix:rrq:* reads stay strictly <= k even
// when prompt-length bucketing has combined spillover pods.
func splitPDTargetBudget(k, totalP, totalD, totalC int) (prefillBudget, decodeBudget, combinedBudget int) {
	prefillBudget = min(totalP, k)
	if totalD+totalC <= k {
		return prefillBudget, totalD, totalC
	}
	if totalC == 0 {
		return prefillBudget, min(totalD, k), 0
	}
	if k == 1 {
		// Must keep at least 1 decode pod for the kept PD roleset.
		return prefillBudget, 1, 0
	}
	combinedBudget = min(totalC, max(1, (k*totalC)/(totalD+totalC)))
	if combinedBudget >= k {
		combinedBudget = k - 1
	}
	decodeBudget = min(totalD, k-combinedBudget)
	if decodeBudget+combinedBudget < k && combinedBudget < totalC {
		combinedBudget = min(totalC, k-decodeBudget)
	}
	return prefillBudget, decodeBudget, combinedBudget
}

// seedPDPrefillCandidates delegates prefix-holder discovery to the effective
// prefill policy itself so tokenization and index lookups always use the exact
// same tokenizer and prefixCacheIndexer instance that Prepare will score with.
// Note: building readyKeySet scans eligible prefill pods (O(N_p)) so
// MatchPrefix can discover prefix holders across the fleet without leaking
// filtered or blacklisted pods; downstream Redis reads and scoring remain O(K).
func seedPDPrefillCandidates(
	ctx *types.RoutingContext,
	prefillPol pd.PrefillScorePolicy,
	eligible []pdCandidateRoleset,
	totalP int,
) (map[string]int, map[*v1.Pod]string, bool) {
	seedable, ok := prefillPol.(pd.PrefixSeedablePrefillPolicy)
	if !ok || totalP == 0 {
		return nil, nil, false
	}
	prefillKeys := make(map[*v1.Pod]string, totalP)
	readyKeySet := make(map[string]struct{}, totalP)
	for i := range eligible {
		for _, pod := range eligible[i].prefills {
			key := utils.GeneratePodKey(pod.Namespace, pod.Name)
			prefillKeys[pod] = key
			readyKeySet[key] = struct{}{}
		}
	}
	matchedPods, _, seeded := seedable.SeedPrefixMatch(ctx, readyKeySet)
	if !seeded {
		return nil, prefillKeys, true
	}
	return matchedPods, prefillKeys, false
}

func effectivePrefixMatchPct(rawMatch int, prefillPol pd.PrefillScorePolicy, minMatchPct float64) int {
	if rawMatch <= 0 {
		return 0
	}
	if prefillPol != nil && prefillPol.Name() == pd.PrefillScorePolicyConductor {
		return rawMatch
	}
	return pd.ClampMinMatch(rawMatch, minMatchPct)
}

func attachRolesetPrefixMatches(
	eligible []pdCandidateRoleset,
	prefillKeys map[*v1.Pod]string,
	matchedPods map[string]int,
	prefillPol pd.PrefillScorePolicy,
	minMatchPct float64,
) {
	if len(matchedPods) == 0 {
		return
	}
	for i := range eligible {
		best := 0
		for _, pod := range eligible[i].prefills {
			key := podKeyLookup(pod, prefillKeys)
			if pct := effectivePrefixMatchPct(matchedPods[key], prefillPol, minMatchPct); pct > best {
				best = pct
			}
		}
		eligible[i].bestMatchPct = best
	}
}

func (r *pdRouter) selectCandidateRolesets(
	eligible []pdCandidateRoleset,
	k, prefillBudget, decodeBudget int,
	prefillKeys map[*v1.Pod]string,
	usesPrefillTokenLoad, usesDecodeTokenLoad bool,
	kvWeight float64,
	inflightLimit int64,
) []pdCandidateRoleset {
	multiDecode := 0
	for i := range eligible {
		if len(eligible[i].decodes) >= 2 {
			multiDecode++
		}
	}
	minPerRS := 1
	if decodeBudget >= 4 && multiDecode*2 >= len(eligible) {
		minPerRS = 2
	}
	maxRolesets := min(len(eligible), prefillBudget, decodeBudget)
	targetRolesets := min(maxRolesets, max(1, decodeBudget/minPerRS))

	if len(eligible) <= targetRolesets {
		out := make([]pdCandidateRoleset, len(eligible))
		for i := range eligible {
			out[i] = eligible[i]
			out[i].isRanked = k >= 2
		}
		return out
	}

	if k == 1 {
		chosen := eligible[rand.IntN(len(eligible))]
		chosen.isRanked = false
		return []pdCandidateRoleset{chosen}
	}

	nRanked := targetRolesets / 2
	if targetRolesets == 1 {
		nRanked = 1
	}

	scoutIndices := scoutRolesetIndices(eligible, targetRolesets)
	scores := make([]pdCandidateScore, len(scoutIndices))
	for i, rsIdx := range scoutIndices {
		rs := &eligible[rsIdx]
		scores[i] = pdCandidateScore{
			matchPct: rs.bestMatchPct,
			load:     r.estimateRolesetLoad(rs, prefillKeys, usesPrefillTokenLoad, usesDecodeTokenLoad, kvWeight, inflightLimit),
		}
	}

	picked := make(map[int]struct{}, targetRolesets)
	out := make([]pdCandidateRoleset, 0, targetRolesets)
	for _, pos := range bestCandidateIndices(scores, nRanked) {
		rsIdx := scoutIndices[pos]
		picked[rsIdx] = struct{}{}
		rs := eligible[rsIdx]
		rs.isRanked = true
		out = append(out, rs)
	}
	sampleRolesets(eligible, targetRolesets, picked, &out)
	return out
}

func scoutRolesetIndices(eligible []pdCandidateRoleset, targetRolesets int) []int {
	scoutLimit := pdCandidateScoutFactor * targetRolesets
	if 2*scoutLimit >= len(eligible) {
		all := make([]int, len(eligible))
		for i := range eligible {
			all[i] = i
		}
		return all
	}
	scout := make([]int, 0, scoutLimit+targetRolesets)
	seen := make(map[int]struct{}, scoutLimit+targetRolesets)

	// Always include prefix-holder rolesets first (highest match first, up to targetRolesets).
	var holders []int
	for i := range eligible {
		if eligible[i].bestMatchPct > 0 {
			holders = append(holders, i)
		}
	}
	if len(holders) > 1 {
		sort.Slice(holders, func(i, j int) bool {
			return eligible[holders[i]].bestMatchPct > eligible[holders[j]].bestMatchPct
		})
	}
	if len(holders) > targetRolesets {
		holders = holders[:targetRolesets]
	}
	for _, idx := range holders {
		seen[idx] = struct{}{}
		scout = append(scout, idx)
	}

	target := min(len(eligible), len(scout)+scoutLimit)
	for len(scout) < target {
		idx := rand.IntN(len(eligible))
		if _, dup := seen[idx]; !dup {
			seen[idx] = struct{}{}
			scout = append(scout, idx)
		}
	}
	return scout
}

func sampleRolesets(eligible []pdCandidateRoleset, target int, picked map[int]struct{}, out *[]pdCandidateRoleset) {
	target = min(target, len(eligible))
	if 2*target > len(eligible) {
		for _, idx := range rand.Perm(len(eligible)) {
			if len(*out) >= target {
				return
			}
			if _, dup := picked[idx]; !dup {
				picked[idx] = struct{}{}
				rs := eligible[idx]
				rs.isRanked = false
				*out = append(*out, rs)
			}
		}
		return
	}
	for len(*out) < target {
		idx := rand.IntN(len(eligible))
		if _, dup := picked[idx]; !dup {
			picked[idx] = struct{}{}
			rs := eligible[idx]
			rs.isRanked = false
			*out = append(*out, rs)
		}
	}
}

// estimateRolesetLoad estimates a roleset's in-memory load (lower is better)
// from a bounded sample of its prefill and decode pods without reading Redis.
func (r *pdRouter) estimateRolesetLoad(
	rs *pdCandidateRoleset,
	prefillKeys map[*v1.Pod]string,
	usesPrefillTokenLoad, usesDecodeTokenLoad bool,
	kvWeight float64,
	inflightLimit int64,
) float64 {
	pSample := samplePodSubset(rs.prefills, pdRolesetLoadSampleCap)
	pMin := 0.0
	for i, pod := range pSample {
		load := r.inMemoryPrefillLoad(podKeyLookup(pod, prefillKeys), usesPrefillTokenLoad, kvWeight)
		if i == 0 || load < pMin {
			pMin = load
		}
	}

	dSample := samplePodSubset(rs.decodes, pdRolesetLoadSampleCap)
	approx := approxRunningForPods(r.cache, dSample)
	dMin := 0.0
	hasUnsaturated := false
	for i, pod := range dSample {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		saturated := inflightLimit > 0 && len(approx) > 0 && approx[key] >= inflightLimit
		load := r.inMemoryDecodeLoad(pod, key, approx, usesDecodeTokenLoad)
		if saturated {
			load += 1e6
		} else {
			hasUnsaturated = true
		}
		if i == 0 || load < dMin {
			dMin = load
		}
	}
	if !hasUnsaturated && inflightLimit > 0 && len(approx) > 0 {
		dMin += 1e6
	}
	return pMin + dMin
}

// allocateRolesetQuotas guarantees >= 1 slot per kept roleset and distributes
// the remaining budget up to totalBudget proportionally to remaining capacity,
// breaking rounding remainders in favor of prefix-holder rolesets first.
func allocateRolesetQuotas(rolesets []pdCandidateRoleset, totalBudget int, forPrefill bool) []int {
	n := len(rolesets)
	quotas := make([]int, n)
	if n == 0 || totalBudget <= 0 {
		return quotas
	}
	caps := make([]int, n)
	totalCap := 0
	for i := range rolesets {
		if forPrefill {
			caps[i] = len(rolesets[i].prefills)
		} else {
			caps[i] = len(rolesets[i].decodes)
		}
		totalCap += caps[i]
	}
	if totalCap <= totalBudget {
		copy(quotas, caps)
		return quotas
	}

	used := 0
	remCap := 0
	for i := range rolesets {
		if caps[i] > 0 && used < totalBudget {
			quotas[i] = 1
			used++
			remCap += caps[i] - 1
		}
	}
	remaining := totalBudget - used
	if remaining <= 0 || remCap <= 0 {
		return quotas
	}

	// Proportional share of remaining capacity.
	for i := range rolesets {
		slack := caps[i] - quotas[i]
		if slack <= 0 {
			continue
		}
		share := (remaining * slack) / remCap
		if share > slack {
			share = slack
		}
		quotas[i] += share
		used += share
	}

	// Distribute rounding remainder: prefix-holder rolesets first, then largest slack.
	leftover := totalBudget - used
	if leftover <= 0 {
		return quotas
	}
	order := rand.Perm(n)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if rolesets[a].bestMatchPct != rolesets[b].bestMatchPct {
			return rolesets[a].bestMatchPct > rolesets[b].bestMatchPct
		}
		return (caps[a] - quotas[a]) > (caps[b] - quotas[b])
	})
	for leftover > 0 {
		progressed := false
		for _, idx := range order {
			if leftover == 0 {
				break
			}
			if quotas[idx] < caps[idx] {
				quotas[idx]++
				leftover--
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return quotas
}

func roleRankedSlots(quota, k int, rolesetRanked bool) int {
	if k <= 1 || quota <= 0 {
		return 0
	}
	if quota == 1 {
		if rolesetRanked {
			return 1
		}
		return 0
	}
	return quota / 2
}

func (r *pdRouter) selectPrefillCandidatesForRoleset(
	rs *pdCandidateRoleset,
	quota, k int,
	prefillKeys map[*v1.Pod]string,
	matchedPods map[string]int,
	prefillPol pd.PrefillScorePolicy,
	minMatchPct float64,
	usesTokenLoad bool,
	kvWeight float64,
) []*v1.Pod {
	if quota >= len(rs.prefills) {
		return rs.prefills
	}
	if quota <= 0 {
		return nil
	}

	out := make([]*v1.Pod, 0, quota)
	picked := make(map[*v1.Pod]struct{}, quota)
	nRanked := roleRankedSlots(quota, k, rs.isRanked)
	// Even when Level 1 marked this roleset as an exploration slot with quota==1,
	// force-include a local prefix-cache holder if one exists so tied/shared prefix
	// fleets do not randomly drop holders inside a kept roleset.
	if nRanked == 0 && k >= 2 && rs.bestMatchPct > 0 {
		nRanked = 1
	}
	if nRanked > 0 {
		scout := scoutPrefillPods(rs.prefills, quota, prefillKeys, matchedPods, prefillPol, minMatchPct)
		scores := make([]pdCandidateScore, len(scout))
		for i, pod := range scout {
			key := podKeyLookup(pod, prefillKeys)
			scores[i] = pdCandidateScore{
				matchPct: effectivePrefixMatchPct(matchedPods[key], prefillPol, minMatchPct),
				load:     r.inMemoryPrefillLoad(key, usesTokenLoad, kvWeight),
			}
		}
		for _, pos := range bestCandidateIndices(scores, nRanked) {
			pod := scout[pos]
			picked[pod] = struct{}{}
			out = append(out, pod)
		}
	}
	samplePods(rs.prefills, quota, picked, &out)
	return out
}

func scoutPrefillPods(
	prefills []*v1.Pod,
	quota int,
	prefillKeys map[*v1.Pod]string,
	matchedPods map[string]int,
	prefillPol pd.PrefillScorePolicy,
	minMatchPct float64,
) []*v1.Pod {
	scoutLimit := pdCandidateScoutFactor * quota
	if 2*scoutLimit >= len(prefills) {
		return prefills
	}
	scout := make([]*v1.Pod, 0, scoutLimit+quota)
	seen := make(map[*v1.Pod]struct{}, scoutLimit+quota)

	if len(matchedPods) > 0 {
		type holder struct {
			pod *v1.Pod
			pct int
		}
		var holders []holder
		for _, pod := range prefills {
			pct := effectivePrefixMatchPct(matchedPods[podKeyLookup(pod, prefillKeys)], prefillPol, minMatchPct)
			if pct > 0 {
				holders = append(holders, holder{pod: pod, pct: pct})
			}
		}
		if len(holders) > 1 {
			sort.Slice(holders, func(i, j int) bool { return holders[i].pct > holders[j].pct })
		}
		if len(holders) > quota {
			holders = holders[:quota]
		}
		for _, h := range holders {
			seen[h.pod] = struct{}{}
			scout = append(scout, h.pod)
		}
	}
	samplePods(prefills, len(scout)+scoutLimit, seen, &scout)
	return scout
}

func (r *pdRouter) selectDecodeCandidatesForRoleset(
	rs *pdCandidateRoleset,
	quota, k int,
	usesDecodeTokenLoad bool,
	inflightLimit int64,
) []*v1.Pod {
	if quota >= len(rs.decodes) {
		return rs.decodes
	}
	if quota <= 0 {
		return nil
	}

	out := make([]*v1.Pod, 0, quota)
	picked := make(map[*v1.Pod]struct{}, quota)
	nRanked := roleRankedSlots(quota, k, rs.isRanked)
	if nRanked > 0 {
		scout := samplePodSubset(rs.decodes, pdCandidateScoutFactor*quota)
		approx := approxRunningForPods(r.cache, scout)
		scout = softExcludeInflightSaturated(scout, approx, inflightLimit)
		scores := make([]pdCandidateScore, len(scout))
		for i, pod := range scout {
			key := utils.GeneratePodKey(pod.Namespace, pod.Name)
			scores[i] = pdCandidateScore{
				load: r.inMemoryDecodeLoad(pod, key, approx, usesDecodeTokenLoad),
			}
		}
		for _, pos := range bestCandidateIndices(scores, nRanked) {
			pod := scout[pos]
			picked[pod] = struct{}{}
			out = append(out, pod)
		}
	} else if k >= 2 && inflightLimit > 0 {
		// Even on a quota==1 exploration roleset, steer the single decode slot away
		// from in-memory saturated replicas (randomly among unsaturated scouted pods,
		// falling back to unfiltered if all scouted pods are at cap).
		scout := samplePodSubset(rs.decodes, pdCandidateScoutFactor*quota)
		approx := approxRunningForPods(r.cache, scout)
		softPool := softExcludeInflightSaturated(scout, approx, inflightLimit)
		samplePods(softPool, quota, picked, &out)
	}
	samplePods(rs.decodes, quota, picked, &out)
	return out
}

func (r *pdRouter) selectCombinedCandidates(combinedPods []*v1.Pod, budget int, inflightLimit int64) []*v1.Pod {
	if budget >= len(combinedPods) {
		return combinedPods
	}
	if budget <= 0 {
		return nil
	}
	out := make([]*v1.Pod, 0, budget)
	picked := make(map[*v1.Pod]struct{}, budget)
	nRanked := budget / 2
	if nRanked > 0 {
		scout := samplePodSubset(combinedPods, pdCandidateScoutFactor*budget)
		approx := approxRunningForPods(r.cache, scout)
		scout = softExcludeInflightSaturated(scout, approx, inflightLimit)
		scores := make([]pdCandidateScore, len(scout))
		for i, pod := range scout {
			key := utils.GeneratePodKey(pod.Namespace, pod.Name)
			scores[i] = pdCandidateScore{
				load: r.inMemoryDecodeLoad(pod, key, approx, false),
			}
		}
		for _, pos := range bestCandidateIndices(scores, nRanked) {
			pod := scout[pos]
			picked[pod] = struct{}{}
			out = append(out, pod)
		}
	}
	samplePods(combinedPods, budget, picked, &out)
	return out
}

// softExcludeInflightSaturated drops scouted pods whose in-memory approximate
// running-request count already reaches inflightLimit so saturated pods do not
// waste ranked slots. If every scouted pod is at cap (or approx is unavailable),
// scout is returned unchanged so Redis narrowPDToInflightCapacity remains the
// authoritative 429 gate.
func softExcludeInflightSaturated(scout []*v1.Pod, approx map[string]int64, inflightLimit int64) []*v1.Pod {
	if inflightLimit <= 0 || len(approx) == 0 || len(scout) == 0 {
		return scout
	}
	kept := make([]*v1.Pod, 0, len(scout))
	for _, pod := range scout {
		if approx[utils.GeneratePodKey(pod.Namespace, pod.Name)] >= inflightLimit {
			continue
		}
		kept = append(kept, pod)
	}
	if len(kept) == 0 {
		return scout
	}
	return kept
}

func approxRunningForPods(c cache.Cache, pods []*v1.Pod) map[string]int64 {
	if c == nil || len(pods) == 0 {
		return nil
	}
	provider, ok := c.(cache.ApproxRunningRequestsProvider)
	if !ok {
		return nil
	}
	return provider.GetPodsRunningRequestsApprox(pods)
}

func (r *pdRouter) inMemoryPrefillLoad(podKey string, usesTokenLoad bool, kvWeight float64) float64 {
	if usesTokenLoad && r.tokenLoadTracker != nil {
		return r.tokenLoadTracker.GetPriorityWithKVWeight(podKey, kvWeight)
	}
	if r.prefillRequestTracker != nil {
		return float64(r.prefillRequestTracker.GetPrefillRequestCountsForPod(podKey))
	}
	return 0
}

func (r *pdRouter) inMemoryDecodeLoad(pod *v1.Pod, podKey string, approx map[string]int64, usesDecodeTokenLoad bool) float64 {
	pending := 0.0
	if r.pendingDecodeTracker != nil {
		pending = r.pendingDecodeTracker.GetPendingDecodeCount(podKey)
	}
	if usesDecodeTokenLoad && r.tokenLoadTracker != nil {
		tokenLoad := r.tokenLoadTracker.GetDecodeLoad(podKey)
		if tokenLoad > 0 {
			return tokenLoad
		}
	}
	if count, ok := approx[podKey]; ok {
		return float64(count) + pending
	}
	if r.cache != nil && pod != nil {
		if m, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning); err == nil && m != nil {
			return m.GetSimpleValue() + pending
		}
	}
	return pending
}

func podKeyLookup(pod *v1.Pod, cache map[*v1.Pod]string) string {
	if cache != nil {
		if key, ok := cache[pod]; ok {
			return key
		}
	}
	return utils.GeneratePodKey(pod.Namespace, pod.Name)
}

type pdCandidateScore struct {
	matchPct int
	load     float64
}

func (a pdCandidateScore) betterThan(b pdCandidateScore) bool {
	if a.matchPct != b.matchPct {
		return a.matchPct > b.matchPct
	}
	return a.load < b.load
}

// bestCandidateIndices returns the indices of the n best scores, best first.
// Ties are broken uniformly at random via rand.Perm so equal-load fleets spread
// across pods rather than herding onto index 0.
func bestCandidateIndices(scores []pdCandidateScore, n int) []int {
	if n <= 0 || len(scores) == 0 {
		return nil
	}
	if n > len(scores) {
		n = len(scores)
	}
	top := make([]int, 0, n)
	for _, idx := range rand.Perm(len(scores)) {
		s := scores[idx]
		if len(top) == n && !s.betterThan(scores[top[n-1]]) {
			continue
		}
		pos := sort.Search(len(top), func(j int) bool {
			return s.betterThan(scores[top[j]])
		})
		if len(top) < n {
			top = append(top, 0)
		}
		copy(top[pos+1:], top[pos:len(top)-1])
		top[pos] = idx
	}
	return top
}

func samplePodSubset(pods []*v1.Pod, limit int) []*v1.Pod {
	if limit <= 0 || 2*limit >= len(pods) {
		return pods
	}
	out := make([]*v1.Pod, 0, limit)
	seen := make(map[*v1.Pod]struct{}, limit)
	samplePods(pods, limit, seen, &out)
	return out
}

func samplePods(pods []*v1.Pod, target int, picked map[*v1.Pod]struct{}, out *[]*v1.Pod) {
	add := func(pod *v1.Pod) {
		if _, dup := picked[pod]; !dup {
			picked[pod] = struct{}{}
			*out = append(*out, pod)
		}
	}
	target = min(target, len(pods))
	if 2*target > len(pods) {
		for _, idx := range rand.Perm(len(pods)) {
			if len(*out) >= target {
				return
			}
			add(pods[idx])
		}
		return
	}
	for len(*out) < target {
		add(pods[rand.IntN(len(pods))])
	}
}
