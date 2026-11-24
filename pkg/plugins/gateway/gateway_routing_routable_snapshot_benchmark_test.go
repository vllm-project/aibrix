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
	"encoding/json"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
)

// BenchmarkSelectTargetPod_RoutableSnapshot measures validateModelAvailability +
// selectTargetPod over a shared model snapshot across fleet sizes N={50, 500, 2000}
// and candidate-stage settings K={0, 16}. At K=0 the shuffle still needs a private
// slice copy, so caching PodArray.Routable() saves the per-request readiness scans
// (CountRoutablePods + FilterRoutablePods); at K=16 the candidate stage returns a
// fresh K-pod slice, so caching also eliminates the O(N) ready-pod slice allocation.
func BenchmarkSelectTargetPod_RoutableSnapshot(b *testing.B) {
	for _, k := range []int{0, 16} {
		for _, n := range []int{50, 500, 2000} {
			b.Run(fmt.Sprintf("K=%d/N=%d", k, n), func(b *testing.B) {
				pods := make([]*v1.Pod, n)
				for i := range pods {
					pods[i] = &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("pod-%04d", i),
							Namespace: "ns",
							Labels:    map[string]string{constants.ModelLabelPort: "8000"},
						},
						Status: v1.PodStatus{
							PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
							Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
						},
					}
				}
				store := cache.InitWithPods(cache.New(nil, nil, nil), pods, "m")
				rm := routing.NewRouterManagerWithCache(store)
				rm.Init()
				s := &Server{cache: store, routerManager: rm}

				var profile *types.ResolvedConfigProfile
				if k > 0 {
					raw := json.RawMessage(fmt.Sprintf(`{"candidate":{"k":%d}}`, k))
					profile = &types.ResolvedConfigProfile{
						RoutingConfig: raw,
						Routing:       configprofiles.ParseRoutingConfig(raw),
					}
				}

				// Warm the snapshot lookup once before timing.
				if _, errRes := s.validateModelAvailability("warm", "m"); errRes != nil {
					b.Fatalf("warm validateModelAvailability failed: %v", errRes)
				}

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					podsArr, errRes := s.validateModelAvailability("req-bench", "m")
					if errRes != nil {
						b.Fatalf("validateModelAvailability failed: %v", errRes)
					}
					ctx := types.NewRoutingContext(context.Background(), least.RouterLeastRequest, "m", "hello world", "req-bench", "")
					ctx.ConfigProfile = profile
					if _, err := s.selectTargetPod(context.Background(), ctx, podsArr, ""); err != nil {
						b.Fatal(err)
					}
					ctx.Delete()
				}
			})
		}
	}
}
