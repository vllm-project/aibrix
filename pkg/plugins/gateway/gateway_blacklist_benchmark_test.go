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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/blacklist"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// BenchmarkSelectTargetPod_BlacklistTracked measures one least-request routing decision over
// 5,000 ready pods with the candidate stage at K=8, as the blacklist tracks more of them. The
// blacklist filters the K candidates, not the fleet, so the cost of a decision must not grow with
// the number of tracked pods: a pod is tracked once a sweep has reported a failed probe for it, closed or not.
// None of the pods is excluded in any case, so every case routes over the same candidates.
func BenchmarkSelectTargetPod_BlacklistTracked(b *testing.B) {
	const n = 5000
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("pod-%04d", i), Namespace: "ns",
				Labels: map[string]string{constants.ModelLabelPort: "8000"},
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
	raw := json.RawMessage(`{"candidate":{"k":8}}`)
	profile := &types.ResolvedConfigProfile{RoutingConfig: raw, Routing: configprofiles.ParseRoutingConfig(raw)}

	for _, tc := range []struct {
		name    string
		tracked []*v1.Pod
	}{
		{"tracked=0_fastpath", nil},
		{"tracked=1_unrelated_pod", pods[:1]},
		{"tracked=all_5000_closed", pods},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s := &Server{cache: store, routerManager: rm}
			for _, pod := range tc.tracked {
				// One failed sweep creates the entry and leaves the breaker closed: it takes
				// several in a row to open it.
				s.podBlacklist.RecordProbeResult(pod, blacklist.ProbeDegraded, "GET /v1/models did not list the model")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx := types.NewRoutingContext(context.Background(), least.RouterLeastRequest, "m", "hello world", "req-bench", "")
				ctx.ConfigProfile = profile
				if _, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, ""); err != nil {
					b.Fatal(err)
				}
				ctx.Delete()
			}
		})
	}
}
