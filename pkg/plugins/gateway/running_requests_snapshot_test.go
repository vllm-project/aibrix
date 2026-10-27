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

package gateway

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// TestSelectTargetPod_ReadsRunningRequestsOnce routes one request per strategy over n ready
// pods against a Redis protocol server and counts the commands the routing path sends. The
// inflight filter, the load-imbalance gate, the strategy and its auto-blend all read the same
// pods' running counts; one routing decision must read them from Redis once (n HGETALLs plus
// the live-gateways read), not once per reader.
func TestSelectTargetPod_ReadsRunningRequestsOnce(t *testing.T) {
	const n = 100
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%03d", i), Namespace: "ns"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, "m")
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	// One fleet read is n HGETALLs and one ZRANGEBYSCORE of the live gateways, in one pipeline.
	const oneRead = n + 1
	strategies := []types.RoutingAlgorithm{
		least.RouterLeastRequest,
		routing.RouterLoadBalance,
		routing.RouterThroughput,
		least.RouterLeastLatency,
		least.RouterLeastKvCache,
		prefixcache.RouterPrefixCache,
		routing.RouterPowerOfTwo,
		routing.RouterRandom,
	}
	for _, inflight := range []int64{0, 1000} {
		for _, strategy := range strategies {
			t.Run(fmt.Sprintf("%s/inflight=%d", strategy, inflight), func(t *testing.T) {
				ctx := types.NewRoutingContext(context.Background(), strategy, "m", "hello world", fmt.Sprintf("req-%s-%d", strategy, inflight), "")
				t.Cleanup(ctx.Delete)
				if inflight > 0 {
					ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: inflight}
				}
				before := mr.CommandCount()
				addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
				require.NoError(t, err)
				require.NotEmpty(t, addr)
				sent := mr.CommandCount() - before
				t.Logf("%d Redis commands for one request over %d pods", sent, n)
				// A few commands besides the read are allowed (e.g. the admission under an
				// inflight cap); a second fleet read is not.
				assert.GreaterOrEqual(t, sent, oneRead, "the ready pods are read")
				assert.Less(t, sent, oneRead+n/2, "the ready pods are read once, not once per reader")
			})
		}
	}
}
