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
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// BenchmarkSelectTargetPod_Redis measures one routing decision end to end, running-request
// reads included, against a real Redis (#3322). Set AIBRIX_BENCH_REDIS_ADDR (for example
// localhost:6379); the benchmark flushes that Redis's current database.
func BenchmarkSelectTargetPod_Redis(b *testing.B) {
	addr := os.Getenv("AIBRIX_BENCH_REDIS_ADDR")
	if addr == "" {
		b.Skip("set AIBRIX_BENCH_REDIS_ADDR to a Redis this benchmark may flush")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	b.Cleanup(func() { _ = client.Close() })

	for _, n := range []int{500, 2000} {
		if err := client.FlushDB(context.Background()).Err(); err != nil {
			b.Fatal(err)
		}
		pods := make([]*v1.Pod, n)
		for i := range pods {
			pods[i] = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%04d", i), Namespace: "ns"},
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

		for _, strategy := range []types.RoutingAlgorithm{least.RouterLeastRequest, routing.RouterLoadBalance, routing.RouterRandom} {
			b.Run(fmt.Sprintf("%s/N=%d", strategy, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					ctx := types.NewRoutingContext(context.Background(), strategy, "m", "hello world", "req-bench", "")
					if _, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, ""); err != nil {
						b.Fatal(err)
					}
					ctx.Delete()
				}
			})
		}
	}
}
