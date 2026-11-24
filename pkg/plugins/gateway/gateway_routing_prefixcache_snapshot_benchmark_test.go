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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
)

func silenceGatewayBenchKlog(b *testing.B) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	klog.SetOutput(io.Discard)
	b.Cleanup(func() {
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("stderrthreshold", "ERROR")
		klog.SetOutput(os.Stderr)
	})
}

// BenchmarkSelectTargetPod_PrefixCache measures validateModelAvailability +
// selectTargetPod for prefix-cache routing across N={50, 500, 2000} and
// K={0, 16}. Comparing K=0 and K=16 isolates the per-K gain from PodArray's
// cached KeySet/ByKey index and the O(holders + K) FindCandidates path.
func BenchmarkSelectTargetPod_PrefixCache(b *testing.B) {
	silenceGatewayBenchKlog(b)
	prompts := make([]string, 32)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("session %d: ", i) + strings.Repeat("shared prefix prompt text ", 32)
	}

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

				// Warm the prefix index with all 32 prompts so requests hit holders.
				podsArr, errRes := s.validateModelAvailability("warm", "m")
				if errRes != nil {
					b.Fatalf("warm validateModelAvailability failed: %v", errRes)
				}
				for i, prompt := range prompts {
					ctx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompt, fmt.Sprintf("warm-%d", i), "")
					ctx.ConfigProfile = profile
					if _, err := s.selectTargetPod(context.Background(), ctx, podsArr, ""); err != nil {
						b.Fatal(err)
					}
					ctx.Delete()
				}

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					podsArr, errRes := s.validateModelAvailability("req-bench", "m")
					if errRes != nil {
						b.Fatalf("validateModelAvailability failed: %v", errRes)
					}
					ctx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", prompts[i%len(prompts)], "req-bench", "")
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
