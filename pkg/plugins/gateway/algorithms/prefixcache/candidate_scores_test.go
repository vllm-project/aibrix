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
