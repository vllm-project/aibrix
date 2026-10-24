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

package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BenchmarkGetPodsRunningRequests_Redis measures the cross-gateway running-request read that
// least-request, load-balance, the load-imbalance gate and the inflight filter each make per
// routed request (RFC #3188). It needs a real Redis, because the cost is the round trip and the
// per-pod HGETALLs: set AIBRIX_BENCH_REDIS_ADDR (for example localhost:6379). The benchmark
// flushes that Redis's current database.
func BenchmarkGetPodsRunningRequests_Redis(b *testing.B) {
	addr := os.Getenv("AIBRIX_BENCH_REDIS_ADDR")
	if addr == "" {
		b.Skip("set AIBRIX_BENCH_REDIS_ADDR to a Redis this benchmark may flush")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	b.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	saved := runningRequestsReadTimeout
	runningRequestsReadTimeout = 10 * time.Second // measure the read, not the fallback
	b.Cleanup(func() { runningRequestsReadTimeout = saved })

	for _, n := range []int{50, 500, 2000} {
		if err := client.FlushDB(ctx).Err(); err != nil {
			b.Fatal(err)
		}
		pods := make([]*v1.Pod, n)
		for i := range pods {
			pods[i] = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%04d", i), Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			}
		}
		store := NewWithPodsForTest(pods, "m1")
		store.redisClient = client
		// Four live gateways, each with a few requests on every pod.
		now := float64(time.Now().UnixMilli())
		for g := 0; g < 4; g++ {
			gw := fmt.Sprintf("gw-%d", g)
			client.ZAdd(ctx, runningRequestsGatewaysKey, redis.Z{Score: now + float64(time.Hour.Milliseconds()), Member: gw})
			pipe := client.Pipeline()
			for i, pod := range pods {
				pipe.HSet(ctx, runningRequestsKey(pod.Namespace, pod.Name), gw, (i+g)%5)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
		}
		for _, k := range []int{n, 16} {
			if k > n {
				continue
			}
			subset := pods[:k]
			b.Run(fmt.Sprintf("N=%d/read=%d", n, k), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					counts, _ := store.GetPodsRunningRequests(subset)
					if len(counts) != k {
						b.Fatalf("got %d counts, want %d", len(counts), k)
					}
				}
			})
		}
	}
}
