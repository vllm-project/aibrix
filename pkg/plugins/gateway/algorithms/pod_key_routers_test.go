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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
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

	ctx := createTestRoutingContext("m1", "hello", "req-throughput")
	_, err := router.Route(ctx, &MockPodList{pods: []*v1.Pod{teamA, teamB}})
	require.NoError(t, err)
	require.NotNil(t, ctx.TargetPod())
	assert.Equal(t, "team-b", ctx.TargetPod().Namespace, "team-b/worker-0 processed fewer tokens")
}

// TestPreble_SameNamedPodsInTwoNamespaces checks that a prefix routed to one pod is
// not credited to a same-named pod in another namespace.
func TestPreble_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPod("team-a", "10.0.0.1")
	teamB := sameNamedPod("team-b", "10.0.0.2")
	// team-b is listed last, so resolving the cached pod by name alone would pick it.
	podList := &MockPodList{pods: []*v1.Pod{teamA, teamB}}
	router := newTestRouter(2, nil)

	const message = "Hello world shared content extra"
	require.NoError(t, router.PostRouteUpdate(createTestRoutingContext("test-model", message, "seed"), podList, teamA))

	_, scored, err := router.ScoreAll(createTestRoutingContext("test-model", message, "score"), podList)
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, scored, "only team-a/worker-0 holds the prefix")

	ctx := createTestRoutingContext("test-model", message, "route")
	_, err = router.Route(ctx, podList)
	require.NoError(t, err)
	require.NotNil(t, ctx.TargetPod())
	assert.Equal(t, "team-a", ctx.TargetPod().Namespace, "the request must go to the pod that holds the prefix")
}
