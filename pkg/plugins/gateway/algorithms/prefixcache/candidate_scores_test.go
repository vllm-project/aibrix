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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// TestCandidateScores_AreThePrefixScores checks that the candidate estimate of prefix-cache is
// its exact score, longer matches higher and pods without the prefix at zero, and that the match
// is kept on the context for the scoring that follows the candidate stage.
func TestCandidateScores_AreThePrefixScores(t *testing.T) {
	pods := make([]*v1.Pod, 6)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "default"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	podList := &utils.PodArray{Pods: pods}
	tok, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)
	router := prefixCacheRouter{
		cache:              cache.NewWithPodsForTest(pods, "m1"),
		tokenizer:          tok,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}

	full := strings.Repeat("shared conversation ", 16)
	half := full[:len(full)/2]
	newCtx := func(id, message string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", message, id, "")
		t.Cleanup(ctx.Delete)
		return ctx
	}
	// pod-4 holds the whole prompt, pod-1 only its first half.
	require.NoError(t, router.PostRouteUpdate(newCtx("a", full), podList, pods[4]))
	require.NoError(t, router.PostRouteUpdate(newCtx("b", half), podList, pods[1]))

	ctx := newCtx("estimate", full)
	estimates, scored, err := router.CandidateScores(ctx, podList)
	require.NoError(t, err)
	require.NotNil(t, ctx.SeededPrefixMatch(), "the match is kept for the rest of the decision")
	exact, _, err := router.ScoreAll(newCtx("exact", full), podList)
	require.NoError(t, err)
	assert.Equal(t, exact, estimates)
	assert.Equal(t, []bool{true, true, true, true, true, true}, scored)
	assert.Equal(t, float64(100), estimates[4])
	assert.Greater(t, estimates[1], float64(0))
	assert.Less(t, estimates[1], estimates[4], "a longer match scores higher")
	for _, i := range []int{0, 2, 3, 5} {
		assert.Zero(t, estimates[i], "pod-%d does not hold the prefix", i)
	}
	assert.Equal(t, types.PolarityMost, router.Polarity())
}

// findCandidatesRouter returns a local-index prefix-cache router over n ready pods and the pod
// list it routes over.
func findCandidatesRouter(t *testing.T, n int) (prefixCacheRouter, []*v1.Pod, *utils.PodArray) {
	t.Helper()
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "default"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	tok, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)
	return prefixCacheRouter{
		cache:              cache.NewWithPodsForTest(pods, "m1"),
		tokenizer:          tok,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}, pods, &utils.PodArray{Pods: pods}
}

func newFindCtx(t *testing.T, id, message string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", message, id, "")
	t.Cleanup(ctx.Delete)
	return ctx
}

// TestFindCandidates_LongestMatchFirstAndTruncated checks that the found pods are the holders
// of the request's prefix, longest match first, cut to max, and that pods without it are left
// out. With more holders than max, the longest matches must be the ones kept.
func TestFindCandidates_LongestMatchFirstAndTruncated(t *testing.T) {
	router, pods, podList := findCandidatesRouter(t, 8)
	full := strings.Repeat("shared conversation ", 16)
	half := full[:len(full)/2]
	quarter := full[:len(full)/4]
	// pods 2 and 6 hold the whole prompt, pod 4 half of it, pods 1 and 7 a quarter.
	for i, holder := range map[int]string{2: full, 6: full, 4: half, 1: quarter, 7: quarter} {
		require.NoError(t, router.PostRouteUpdate(newFindCtx(t, fmt.Sprintf("add-%d", i), holder), podList, pods[i]))
	}

	found := router.FindCandidates(newFindCtx(t, "all", full), podList, 8)
	require.Len(t, found, 5, "only the holders are found")
	assert.ElementsMatch(t, []*v1.Pod{pods[2], pods[6]}, found[:2], "the full matches come first")
	assert.Same(t, pods[4], found[2], "then the half match")
	assert.ElementsMatch(t, []*v1.Pod{pods[1], pods[7]}, found[3:], "then the quarter matches")

	// More holders than max: the longest matches are kept, the weakest dropped.
	for _, max := range []int{1, 2, 3} {
		found = router.FindCandidates(newFindCtx(t, fmt.Sprintf("max-%d", max), full), podList, max)
		require.Len(t, found, max)
		for _, pod := range found[:min(max, 2)] {
			assert.Contains(t, []*v1.Pod{pods[2], pods[6]}, pod, "max=%d keeps a full match", max)
		}
		if max == 3 {
			assert.Same(t, pods[4], found[2], "max=3 keeps the half match over the quarter ones")
		}
	}

	assert.Empty(t, router.FindCandidates(newFindCtx(t, "miss", "an unrelated prompt"), podList, 4))
}

// TestFindCandidates_NonPositiveMaxSkipsTheLookup checks that max <= 0 finds nothing and does
// not even query the index: no match is published on the context.
func TestFindCandidates_NonPositiveMaxSkipsTheLookup(t *testing.T) {
	router, pods, podList := findCandidatesRouter(t, 4)
	prompt := strings.Repeat("shared conversation ", 16)
	require.NoError(t, router.PostRouteUpdate(newFindCtx(t, "add", prompt), podList, pods[1]))
	for _, max := range []int{0, -1} {
		ctx := newFindCtx(t, fmt.Sprintf("max%d", max), prompt)
		assert.Empty(t, router.FindCandidates(ctx, podList, max))
		assert.Nil(t, ctx.SeededPrefixMatch(), "max=%d must not query the index", max)
	}
}

// TestFindCandidates_SpreadsTiesOverAllHolders checks that when more pods hold the prefix equally
// than fit in max, the found pods rotate over all of them instead of always naming the same ones.
func TestFindCandidates_SpreadsTiesOverAllHolders(t *testing.T) {
	router, pods, podList := findCandidatesRouter(t, 8)
	prompt := strings.Repeat("shared conversation ", 16)
	for i, pod := range pods {
		require.NoError(t, router.PostRouteUpdate(newFindCtx(t, fmt.Sprintf("add-%d", i), prompt), podList, pod))
	}
	seen := map[*v1.Pod]bool{}
	for i := 0; i < 200; i++ {
		found := router.FindCandidates(newFindCtx(t, fmt.Sprintf("find-%d", i), prompt), podList, 2)
		require.Len(t, found, 2)
		for _, pod := range found {
			seen[pod] = true
		}
	}
	assert.Len(t, seen, len(pods), "every holder is found some of the time")
}
