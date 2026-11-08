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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

type retainedKVCache struct{ *cache.Store }

func (c *retainedKVCache) GetMetricRecordByPodModel(pod, ns, model, metric string) (cache.MetricRecord, error) {
	record, err := c.Store.GetMetricRecordByPodModel(pod, ns, model, metric)
	if pod == "a" && metric == metrics.KVCacheUsagePerc {
		record.Timestamp = time.Unix(1, 0)
	}
	return record, err
}

func TestKVFreshnessSharedConsumers(t *testing.T) {
	pods := []*v1.Pod{
		newPod("a", "1.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("b", "2.2.2.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	base := cache.NewWithPodsModelMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"a": {metrics.KVCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.1}},
		"b": {metrics.KVCacheUsagePerc: &metrics.SimpleMetricValue{Value: 0.4}},
	})
	c := &retainedKVCache{Store: base}
	// No running requests: load-balance has equal primary scores, so KV breaks the tie.
	lb, err := NewLoadBalanceRouterWithCache(c)
	require.NoError(t, err)
	manager := NewRouterManagerWithCache(c)
	manager.Init()
	alias, err := manager.Lookup(RouterLeastGpuCache)
	require.NoError(t, err)
	for name, router := range map[string]types.Router{"load-balance tie-breaker": lb, "least-gpu-cache alias": alias} {
		t.Run(name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "test", "m1", "input", "req", "")
			target, e := router.Route(ctx, podsFromCache(base))
			require.NoError(t, e)
			require.Equal(t, "2.2.2.2:8000", target, "retained low KV sample must not win")
		})
	}
}
