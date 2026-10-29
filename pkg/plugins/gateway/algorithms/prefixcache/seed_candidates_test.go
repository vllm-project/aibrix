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

// TestSeedCandidates_ReturnsPrefixHoldersLongestFirst checks that the seeds are the pods holding
// the request's prefix, longest match first, capped at max, and that pods without it are left out.
func TestSeedCandidates_ReturnsPrefixHoldersLongestFirst(t *testing.T) {
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

	seeds := router.SeedCandidates(newCtx("seed", full), podList, 4)
	require.Len(t, seeds, 2)
	assert.Same(t, pods[4], seeds[0], "the longest match comes first")
	assert.Same(t, pods[1], seeds[1])

	assert.Equal(t, []*v1.Pod{pods[4]}, router.SeedCandidates(newCtx("cap", full), podList, 1))
	assert.Empty(t, router.SeedCandidates(newCtx("zero", full), podList, 0))
	assert.Empty(t, router.SeedCandidates(newCtx("miss", "an unrelated prompt"), podList, 4))
}

// TestSeedCandidates_SpreadsTiesOverAllHolders checks that when more pods hold the prefix than
// fit in max, the seeds rotate over all of them instead of always naming the same ones.
func TestSeedCandidates_SpreadsTiesOverAllHolders(t *testing.T) {
	pods := make([]*v1.Pod, 8)
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
	prompt := strings.Repeat("shared conversation ", 16)
	for i, pod := range pods {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", prompt, fmt.Sprintf("add-%d", i), "")
		require.NoError(t, router.PostRouteUpdate(ctx, podList, pod))
		ctx.Delete()
	}

	seen := map[*v1.Pod]bool{}
	for i := 0; i < 200; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", prompt, fmt.Sprintf("seed-%d", i), "")
		seeds := router.SeedCandidates(ctx, podList, 2)
		ctx.Delete()
		require.Len(t, seeds, 2)
		for _, pod := range seeds {
			seen[pod] = true
		}
	}
	assert.Len(t, seen, len(pods), "every holder of the prefix is seeded some of the time")
}
