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

package routingalgorithms

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// sameNamedPod returns a ready pod named "worker-0" in namespace, the shape a
// StatefulSet or LeaderWorkerSet produces in each namespace it is deployed to.
func sameNamedPod(namespace, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: namespace},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// TestPrefixCache_SameNamedPodsInTwoNamespaces checks that a prefix cached on one
// pod is not credited to a same-named pod in another namespace.
func TestPrefixCache_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPod("team-a", "10.0.0.1")
	teamB := sameNamedPod("team-b", "10.0.0.2")
	// team-b is listed first, so resolving the matched pod by name alone would pick it.
	pods := []*v1.Pod{teamB, teamA}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"worker-0": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}},
	})
	podList := &utils.PodArray{Pods: pods}

	tok, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)
	router := prefixCacheRouter{
		cache:              c,
		tokenizer:          tok,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}

	message := strings.Repeat("shared conversation ", 8)
	newCtx := func(requestID string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", message, requestID, "")
		t.Cleanup(ctx.Delete)
		return ctx
	}

	require.NoError(t, router.PostRouteUpdate(newCtx("seed"), podList, teamA))

	scores, scored, err := router.ScoreAll(newCtx("score"), podList)
	require.NoError(t, err)
	require.Equal(t, []bool{true, true}, scored)
	assert.Equal(t, 0.0, scores[0], "team-b/worker-0 does not hold the prefix")
	assert.Equal(t, 100.0, scores[1], "team-a/worker-0 holds the prefix")

	ctx := newCtx("route")
	_, err = router.Route(ctx, podList)
	require.NoError(t, err)
	require.NotNil(t, ctx.TargetPod())
	assert.Equal(t, "team-a", ctx.TargetPod().Namespace, "the request must go to the pod that holds the prefix")
}

// TestLeastRequestSelection_SameNamedPodsInTwoNamespaces checks that the least-loaded pod
// is resolved by pod key, so the selected pod is the one whose count was lowest.
func TestLeastRequestSelection_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPod("team-a", "10.0.0.1")
	teamB := sameNamedPod("team-b", "10.0.0.2")
	pods := []*v1.Pod{teamB, teamA}
	counts := map[string]int{"team-a/worker-0": 0, "team-b/worker-0": 20}

	selected := selectTargetPodWithLeastRequestCountFromCounts(counts, pods)
	require.NotNil(t, selected)
	assert.Equal(t, "team-a", selected.Namespace)

	leastPods, _, _, imbalanced := getTargetPodListOnLoadImbalance(counts, pods, 2, 8)
	require.True(t, imbalanced)
	require.Len(t, leastPods, 1)
	assert.Equal(t, "team-a", leastPods[0].Namespace)
}
