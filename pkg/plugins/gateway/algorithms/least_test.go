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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// least-gpu-cache is a deprecated alias for least-kv-cache: the name must stay registered (existing
// routing-strategy headers and config profiles pass it) but must resolve to the kv-cache router,
// wired to the same cache the manager was built with.
func TestLeastGpuCache_IsAliasForLeastKvCache(t *testing.T) {
	t.Run("default manager registers the alias", func(t *testing.T) {
		defaultRM.routerMu.RLock()
		defer defaultRM.routerMu.RUnlock()
		assert.Contains(t, defaultRM.routerConstructor, RouterLeastGpuCache)
	})

	t.Run("isolated manager resolves it to the kv-cache router on its own cache", func(t *testing.T) {
		pods := []*v1.Pod{newPod("p1", "1.1.1.1", true, nil)}
		c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{})
		rm := NewRouterManagerWithCache(c)
		rm.Init()

		_, valid := rm.Validate(string(RouterLeastGpuCache))
		assert.True(t, valid, "least-gpu-cache must stay a valid routing strategy")

		router, err := rm.Lookup(RouterLeastGpuCache)
		require.NoError(t, err)
		// The router type is unexported by package least, so compare against what least-kv-cache
		// itself builds on the same cache: same dynamic type and same cache handle.
		want, err := least.NewLeastKvCacheRouterWithCache(c)
		require.NoError(t, err)
		assert.Equal(t, reflect.TypeOf(want), reflect.TypeOf(router), "least-gpu-cache must resolve to the kv-cache router")
		assert.Equal(t, want, router)
	})
}

// Through the alias, routing follows kv_cache_usage_perc, not the legacy gpu_cache_usage_perc the
// removed router scored: the two metrics disagree here, and the pod with the lower KV usage wins.
func TestLeastGpuCache_AliasRoutesByKvCacheUsage(t *testing.T) {
	pods := []*v1.Pod{
		newPod("p1", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("p2", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	c := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.9},
			metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1},
		},
		"p2": {
			metrics.KVCacheUsagePerc:  &metrics.SimpleMetricValue{Value: 0.1},
			metrics.GPUCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.9},
		},
	})
	rm := NewRouterManagerWithCache(c)
	rm.Init()
	router, err := rm.Lookup(RouterLeastGpuCache)
	require.NoError(t, err)

	for i := 0; i < 20; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterLeastGpuCache, "m1", "message", "req-alias", "user")
		address, err := router.Route(ctx, podsFromCache(c))
		require.NoError(t, err)
		assert.Equal(t, "2.2.2.2:8000", address)
	}
}

// least-request is a deprecated alias for least-request-top-k: the name must stay registered
// (existing routing-strategy headers and config profiles pass it) but must resolve to the top-k
// router, wired to the same cache the manager was built with.
func TestLeastRequest_IsAliasForLeastRequestTopK(t *testing.T) {
	t.Run("default manager registers the alias", func(t *testing.T) {
		defaultRM.routerMu.RLock()
		defer defaultRM.routerMu.RUnlock()
		assert.Contains(t, defaultRM.routerConstructor, RouterLeastRequest)
	})

	t.Run("isolated manager resolves it to the top-k router on its own cache", func(t *testing.T) {
		pods := []*v1.Pod{newPod("p1", "1.1.1.1", true, nil)}
		c := cache.NewWithPodsMetricsForTest(pods, po2TestModel, map[string]map[string]metrics.MetricValue{})
		rm := NewRouterManagerWithCache(c)
		rm.Init()

		_, valid := rm.Validate(string(RouterLeastRequest))
		assert.True(t, valid, "least-request must stay a valid routing strategy")

		router, err := rm.Lookup(RouterLeastRequest)
		require.NoError(t, err)
		topK, ok := router.(*least.LeastRequestTopKRouter)
		require.True(t, ok, "least-request must resolve to *LeastRequestTopKRouter, got %T", router)
		assert.Same(t, c, topK.Cache())
	})
}

// Through the alias, a request must still avoid a pod that is far busier than the rest of the fleet:
// only the least-loaded pods within the top-k/epsilon window (default K=5, epsilon=4) are eligible,
// so p4 never wins.
func TestLeastRequest_AliasNeverPicksHeavilyLoadedPod(t *testing.T) {
	pods := []*v1.Pod{
		newPod("p1", "1.1.1.1", true, nil),
		newPod("p2", "2.2.2.2", true, nil),
		newPod("p3", "3.3.3.3", true, nil),
		newPod("p4", "4.4.4.4", true, nil),
	}
	c := cache.NewWithPodsMetricsForTest(pods, po2TestModel, map[string]map[string]metrics.MetricValue{
		"p1": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
		"p2": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}},
		"p3": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 3}},
		"p4": {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 9}},
	})
	rm := NewRouterManagerWithCache(c)
	rm.Init()
	router, err := rm.Lookup(RouterLeastRequest)
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		ctx := types.NewRoutingContext(context.Background(), RouterLeastRequest, po2TestModel, "hello", "req-alias", "")
		address, err := router.Route(ctx, podsFromCache(c))
		require.NoError(t, err)
		assert.Contains(t, []string{"1.1.1.1:8000", "2.2.2.2:8000", "3.3.3.3:8000"}, address)
	}
}
