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

package prefixcache

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// oracleFindCandidates reproduces the pre-#3545c scoreAll-based FindCandidates
// implementation as a differential oracle for testing equivalence.
func oracleFindCandidates(r *prefixCacheRouter, ctx *types.RoutingContext, readyPodList types.PodList, max int) []*v1.Pod {
	if max <= 0 {
		return nil
	}
	scores, scored, err := r.scoreAll(ctx, readyPodList, true)
	if err != nil {
		return nil
	}
	pods := readyPodList.All()
	matched := make([]int, 0, max)
	for i := range pods {
		if scored[i] && scores[i] > 0 {
			matched = append(matched, i)
		}
	}
	rand.Shuffle(len(matched), func(a, b int) { matched[a], matched[b] = matched[b], matched[a] })
	sort.SliceStable(matched, func(a, b int) bool { return scores[matched[a]] > scores[matched[b]] })
	if len(matched) > max {
		matched = matched[:max]
	}
	found := make([]*v1.Pod, len(matched))
	for j, i := range matched {
		found[j] = pods[i]
	}
	return found
}

type nonPodArrayList struct {
	pods []*v1.Pod
}

func (n *nonPodArrayList) Len() int                          { return len(n.pods) }
func (n *nonPodArrayList) All() []*v1.Pod                    { return n.pods }
func (n *nonPodArrayList) ListByIndex(string) []*v1.Pod      { return n.pods }
func (n *nonPodArrayList) Indexes() []string                 { return nil }
func (n *nonPodArrayList) ListPortsForPod() map[string][]int { return nil }

// TestFindCandidates_OracleEquivalence verifies the O(holders) FindCandidates
// implementation against the old scoreAll-based oracle across both local and
// KV-sync indexers and both *utils.PodArray and generic PodList inputs:
//   - exact same holder set (including skipping 0%-rounded sub-1% matches)
//   - identical descending match-length tier ordering
//   - unbiased random tie-shuffling within tied tiers
//   - exact truncation when max < len(holders)
//   - SeededPrefixMatch coverage equivalence for downstream CandidateScores/ScoreAll
func TestFindCandidates_OracleEquivalence(t *testing.T) {
	full := strings.Repeat("shared conversation prompt block ", 16)
	threeQuarters := full[:len(full)*3/4]
	half := full[:len(full)/2]
	quarter := full[:len(full)/4]

	for _, kvSync := range []bool{false, true} {
		for _, wrapPodArray := range []bool{true, false} {
			name := fmt.Sprintf("kvSync=%v/podArray=%v", kvSync, wrapPodArray)
			t.Run(name, func(t *testing.T) {
				r, podArr := newSeededMatchRouter(t, 24, kvSync)
				pods := podArr.Pods

				var podList types.PodList = podArr
				if !wrapPodArray {
					podList = &nonPodArrayList{pods: pods}
				}

				// Seed tiered holders:
				//   tier 1 (100%): pods 2, 7, 15
				//   tier 2 (~75%): pods 4, 19
				//   tier 3 (~50%): pod 11
				//   tier 4 (~25%): pods 0, 9, 22
				tierSeeds := map[int]string{
					2: full, 7: full, 15: full,
					4: threeQuarters, 19: threeQuarters,
					11: half,
					0:  quarter, 9: quarter, 22: quarter,
				}
				for idx, p := range tierSeeds {
					addSeededMatch(t, r, podList, pods[idx], p)
				}

				// Capture exact per-pod scores from ScoreAll as ground truth for tier checking.
				gtCtx := newSeededMatchContext(full)
				gtScores, _, err := r.ScoreAll(gtCtx, podList)
				gtCtx.Delete()
				require.NoError(t, err)
				scoreOf := make(map[*v1.Pod]float64, len(pods))
				for i, pod := range pods {
					scoreOf[pod] = gtScores[i]
				}

				// 1. Exact holder set and descending tier ordering for max >= holders and max < holders.
				for _, max := range []int{1, 2, 3, 4, 5, 6, 8, 9, 16} {
					oracleCtx := newSeededMatchContext(full)
					oracle := oracleFindCandidates(r, oracleCtx, podList, max)
					oracleCtx.Delete()

					gotCtx := newSeededMatchContext(full)
					got := r.FindCandidates(gotCtx, podList, max)
					require.NotNil(t, gotCtx.SeededPrefixMatch(), "FindCandidates must retain SeededPrefixMatch")

					require.Len(t, got, len(oracle), "max=%d length mismatch", max)

					// Scores sequence must match oracle element-by-element (exact same descending
					// match-length profile, even though ties within a tier are randomly shuffled).
					oracleProfile := make([]float64, len(oracle))
					gotProfile := make([]float64, len(got))
					for i := range oracle {
						oracleProfile[i] = scoreOf[oracle[i]]
						gotProfile[i] = scoreOf[got[i]]
						assert.Greater(t, gotProfile[i], float64(0), "returned pod must have >0 match")
						if i > 0 {
							assert.GreaterOrEqual(t, gotProfile[i-1], gotProfile[i], "must be sorted longest match first")
						}
					}
					assert.Equal(t, oracleProfile, gotProfile, "max=%d match-score profile differs from oracle", max)

					if max >= len(tierSeeds) {
						assert.ElementsMatch(t, oracle, got, "full holder set must match oracle exactly")
					}

					// Downstream CandidateScores on a subset and ScoreAll on final candidates
					// must reuse SeededPrefixMatch and return the exact same scores as a fresh ScoreAll.
					subset := &utils.PodArray{Pods: []*v1.Pod{pods[2], pods[4], pods[11], pods[3]}}
					subScores, subScored, err := r.CandidateScores(gotCtx, subset)
					require.NoError(t, err)
					assert.Equal(t, []bool{true, true, true, true}, subScored)
					assert.Equal(t, []float64{scoreOf[pods[2]], scoreOf[pods[4]], scoreOf[pods[11]], 0}, subScores)

					gotCtx.Delete()
				}

				// 2. Unbiased tie-shuffle over 3 tied 100% holders (pods 2, 7, 15).
				// Checking all 3! = 6 permutations pins rand.Shuffle: Go map iteration
				// randomizes the starting bucket but visits entries in cyclic order
				// (at most 3 permutations), so dropping rand.Shuffle fails this check.
				permCounts := map[string]int{}
				includedTop2 := map[*v1.Pod]int{}
				const trials = 600
				for i := 0; i < trials; i++ {
					ctx := newSeededMatchContext(full)
					got := r.FindCandidates(ctx, podList, 3)
					ctx.Delete()
					require.Len(t, got, 3)
					perm := got[0].Name + "," + got[1].Name + "," + got[2].Name
					permCounts[perm]++
					includedTop2[got[0]]++
					includedTop2[got[1]]++
				}
				tiedFull := []*v1.Pod{pods[2], pods[7], pods[15]}
				assert.Len(t, includedTop2, len(tiedFull), "only full-match holders should be picked in top-3")
				assert.Len(t, permCounts, 6, "all 6 permutations of 3 tied holders must appear (rand.Shuffle required)")
				for perm, count := range permCounts {
					assert.GreaterOrEqual(t, count, 40, "permutation %s appeared only %d/%d times; expected ~%d", perm, count, trials, trials/6)
				}

				// 3. Cache miss returns empty without failing and retains SeededPrefixMatch.
				missCtx := newSeededMatchContext("completely unrelated prompt that matches nothing")
				assert.Empty(t, r.FindCandidates(missCtx, podList, 8))
				assert.NotNil(t, missCtx.SeededPrefixMatch())
				missCtx.Delete()

				// 4. Nil readyPodList returns nil safely without panicking or populating SeededPrefixMatch.
				nilCtx := newSeededMatchContext(full)
				assert.Nil(t, r.FindCandidates(nilCtx, nil, 8))
				assert.Nil(t, nilCtx.SeededPrefixMatch())
				nilCtx.Delete()
			})
		}
	}
}

// TestFindCandidates_SkipsZeroPercentRoundedMatches verifies that when a prompt
// has >100 blocks and a pod holds only 1 block (integer-dividing to 0%),
// FindCandidates skips it identically to the scoreAll oracle (scores[i] > 0).
func TestFindCandidates_SkipsZeroPercentRoundedMatches(t *testing.T) {
	// Default local block size is 4 bytes, kvSync block size is 16 bytes.
	// 200 blocks at 16 bytes = 3200 bytes -> 1 matching block rounds to 0%.
	longPrompt := strings.Repeat("0123456789abcdef", 220)
	tinyPrefix := longPrompt[:16]

	for _, kvSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("kvSync=%v", kvSync), func(t *testing.T) {
			r, podArr := newSeededMatchRouter(t, 8, kvSync)
			addSeededMatch(t, r, podArr, podArr.Pods[1], tinyPrefix)
			addSeededMatch(t, r, podArr, podArr.Pods[3], longPrompt)

			oracleCtx := newSeededMatchContext(longPrompt)
			oracle := oracleFindCandidates(r, oracleCtx, podArr, 8)
			oracleCtx.Delete()

			gotCtx := newSeededMatchContext(longPrompt)
			got := r.FindCandidates(gotCtx, podArr, 8)
			gotCtx.Delete()

			assert.Equal(t, oracle, got)
			if kvSync {
				assert.Equal(t, []*v1.Pod{podArr.Pods[3]}, got, "kvSync 0%-rounded match must be excluded")
			} else {
				// Local PrefixHashTable clamps sub-1% hits to 1%, so both holders appear ordered longest first.
				assert.Equal(t, []*v1.Pod{podArr.Pods[3], podArr.Pods[1]}, got)
			}
		})
	}
}

// TestFindCandidates_FlatAllocationsAcrossFleetSize checks that on a warmed
// *utils.PodArray snapshot, FindCandidates does not scale allocations with N.
func TestFindCandidates_FlatAllocationsAcrossFleetSize(t *testing.T) {
	prompt := strings.Repeat("shared conversation ", 64)
	for _, kvSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("kvSync=%v", kvSync), func(t *testing.T) {
			measure := func(n int) float64 {
				r, podArr := newSeededMatchRouter(t, n, kvSync)
				for _, pod := range podArr.Pods[:8] {
					addSeededMatch(t, r, podArr, pod, prompt)
				}
				// Warm tokenizer output + PodArray KeySet/ByKey caches.
				warm := newSeededMatchContext(prompt)
				found := r.FindCandidates(warm, podArr, 16)
				require.Len(t, found, 8)
				tokens := append([]byte(nil), warm.PrefixTokens()...)
				warm.Delete()

				return testing.AllocsPerRun(50, func() {
					ctx := newSeededMatchContext(prompt)
					ctx.SetPrefixTokens(tokens)
					got := r.FindCandidates(ctx, podArr, 16)
					if len(got) != 8 {
						t.Fatalf("got %d holders, want 8", len(got))
					}
					ctx.Delete()
				})
			}

			allocs50 := measure(50)
			allocs2000 := measure(2000)
			assert.InDelta(t, allocs50, allocs2000, 2,
				"FindCandidates allocations must stay flat from N=50 (%.1f) to N=2000 (%.1f)", allocs50, allocs2000)
		})
	}
}
