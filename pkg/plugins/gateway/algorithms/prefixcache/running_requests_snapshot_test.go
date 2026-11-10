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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// runningReadCounter counts the running-request reads that reach the cache.
type runningReadCounter struct {
	cache.Cache
	reads int
}

func (c *runningReadCounter) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	c.reads++
	return c.Cache.GetPodsRunningRequests(pods)
}

// Inside a routing decision, both prefix-cache routes (the local indexer and KV sync) answer
// their load check from the counts an earlier reader of the decision (e.g. the gateway's
// load-imbalance gate) already read, instead of reading them again.
func TestPrefixCacheRoute_SharesDecisionRead(t *testing.T) {
	pods := make([]*v1.Pod, 3)
	podMetrics := map[string]map[string]metrics.MetricValue{}
	for i := range pods {
		name := fmt.Sprintf("pod%d", i)
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{constants.ModelLabelName: "test-model"}},
			Status:     v1.PodStatus{PodIP: fmt.Sprintf("1.1.1.%d", i+1)},
		}
		podMetrics[name] = map[string]metrics.MetricValue{metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 0}}
	}
	store := cache.NewWithPodsMetricsForTest(pods, "test-model", podMetrics)
	tok, err := tokenizer.NewTokenizer("character", nil)
	require.NoError(t, err)

	routers := map[string]func(c cache.Cache) prefixCacheRouter{
		"local indexer": func(c cache.Cache) prefixCacheRouter {
			return prefixCacheRouter{cache: c, tokenizer: tok, prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable()}
		},
		"kv sync": func(c cache.Cache) prefixCacheRouter {
			return prefixCacheRouter{
				cache:              c,
				tokenizer:          tok,
				prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
				kvSyncRouter: &kvSyncPrefixCacheRouter{
					cache:         c,
					tokenizerPool: &mockTokenizerPool{tokenizer: tok},
					syncIndexer:   syncindexer.NewSyncPrefixHashTable(),
				},
			}
		},
	}
	for name, newRouter := range routers {
		t.Run(name, func(t *testing.T) {
			counter := &runningReadCounter{Cache: store}
			router := newRouter(counter)
			ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", "hello world", "req-"+name, "")
			t.Cleanup(ctx.Delete)
			ctx.StartRunningRequestsSnapshot()
			podList := testPodsFromCache(store)

			_, err := ctx.RunningRequests(podList.All(), counter.GetPodsRunningRequests)
			require.NoError(t, err)
			_, err = router.Route(ctx, podList)
			require.NoError(t, err)
			assert.Equal(t, 1, counter.reads, "the route reuses the decision's read")
		})
	}
}
