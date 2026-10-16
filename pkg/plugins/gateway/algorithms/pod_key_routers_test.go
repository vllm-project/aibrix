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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
)

// namespacedMetricsCache serves per-model metrics by namespace, so two
// same-named pods in different namespaces can report different values.
type namespacedMetricsCache struct {
	cache.Cache
	values map[string]float64 // namespace -> value of every metric
}

func (c *namespacedMetricsCache) GetMetricValueByPodModel(podName, podNamespace, modelName, metricName string) (metrics.MetricValue, error) {
	v, ok := c.values[podNamespace]
	if !ok {
		return nil, fmt.Errorf("no metrics for %s/%s", podNamespace, podName)
	}
	return &metrics.SimpleMetricValue{Value: v}, nil
}

// TestThroughput_SameNamedPodsInTwoNamespaces checks that the least-loaded pod is
// selected even when a same-named pod in another namespace is listed first.
func TestThroughput_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPod("team-a", "10.0.0.1")
	teamB := sameNamedPod("team-b", "10.0.0.2")
	router := throughputRouter{cache: &namespacedMetricsCache{values: map[string]float64{"team-a": 100, "team-b": 1}}}

	ctx := types.NewRoutingContext(context.Background(), RouterThroughput, "m1", "hello", "req-throughput", "")
	_, err := router.Route(ctx, newMockPodList([]*v1.Pod{teamA, teamB}, nil))
	require.NoError(t, err)
	require.NotNil(t, ctx.TargetPod())
	assert.Equal(t, "team-b", ctx.TargetPod().Namespace, "team-b/worker-0 processed fewer tokens")
}

// TestLeastRequestSelection_SameNamedPodsInTwoNamespaces checks that the least-loaded pod
// is resolved by pod key, so the selected pod is the one whose count was lowest.
func TestLeastRequestSelection_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPod("team-a", "10.0.0.1")
	teamB := sameNamedPod("team-b", "10.0.0.2")
	pods := []*v1.Pod{teamB, teamA}
	counts := map[string]int{"team-a/worker-0": 0, "team-b/worker-0": 20}

	selected := routeutil.SelectTargetPodWithLeastRequestCountFromCounts(counts, pods)
	require.NotNil(t, selected)
	assert.Equal(t, "team-a", selected.Namespace)

	leastPods, _, _, imbalanced := getTargetPodListOnLoadImbalance(counts, pods, 2, 8)
	require.True(t, imbalanced)
	require.Len(t, leastPods, 1)
	assert.Equal(t, "team-a", leastPods[0].Namespace)
}
