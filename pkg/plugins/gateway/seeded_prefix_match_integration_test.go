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
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// The gate reads running counts after seeding. This optional fault injection
// models a future caller resetting tokens between seeding and final scoring.
type prefixMatchGateCache struct {
	cache.Cache
	beforeCounts func()
}

func (c *prefixMatchGateCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	if c.beforeCounts != nil {
		before := c.beforeCounts
		c.beforeCounts = nil
		before()
	}
	return c.Cache.GetPodsRunningRequests(pods)
}

func prefixMatchOperations(t *testing.T, model string) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	counts := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "aibrix_prefix_cache_match_operations_total" {
			continue
		}
		for _, metric := range family.Metric {
			var metricModel, result string
			for _, label := range metric.Label {
				switch label.GetName() {
				case "model":
					metricModel = label.GetValue()
				case "result":
					result = label.GetValue()
				}
			}
			if metricModel == model {
				counts[result] += metric.GetCounter().GetValue()
			}
		}
	}
	return counts
}

// Use the real gateway, candidate selector, composite router, prefix index and
// PostRouteUpdate. An all-miss first decision also proves that successful
// admission still credits the selected pod while retaining the zero matches.
func TestSelectTargetPodReusesPrefixMatchesThroughRealPipeline(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			algorithm  types.RoutingAlgorithm
			k          int
			invalidate bool
			lookups    float64
			reuses     float64
			rounds     int
		}{
			{"blend", "prefix-cache:1,least-request:1", 2, false, 1, 2, 2},
			{"tokens-reset", "prefix-cache:1,least-request:1", 2, true, 3, 0, 1},
			{"single-candidate", "prefix-cache:1,least-request:1", 1, false, 1, 1, 1},
			{"disabled", "prefix-cache:1,least-request:1", 0, false, 2, 0, 1},
			{"standalone", routing.RouterPrefixCache, 2, false, 1, 1, 1},
		} {
			t.Run(fmt.Sprintf("kvSync=%v/%s", kvSync, tc.name), func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "false")
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, strconv.FormatBool(kvSync))
				model := strings.ReplaceAll(t.Name(), "/", "-")
				pods := make([]*v1.Pod, 8)
				for i := range pods {
					pods[i] = &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: fmt.Sprintf("seeded-%d", i)},
						Status: v1.PodStatus{PodIP: fmt.Sprintf("10.0.0.%d", i+1),
							Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
					}
				}
				store := cache.NewWithPodsForTest(pods, model)
				t.Cleanup(store.Close)
				c := &prefixMatchGateCache{Cache: store}
				index := prefixcacheindexer.NewPrefixHashTable()
				prefix, err := prefixcache.NewPrefixCacheRouterWithOptions(c, index)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, prefix.(interface{ Cleanup() error }).Cleanup()) })
				manager := routing.NewRouterManager()
				manager.RegisterProvider(routing.RouterPrefixCache, func(*types.RoutingContext) (types.Router, error) { return prefix, nil })
				manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
					return least.NewLeastRequestTopKRouterWithCache(c), nil
				})
				manager.Init()
				s := &Server{cache: c, routerManager: manager}
				prompt := strings.Repeat("shared conversation ", 64)
				tokens, err := tokenizer.NewCharacterTokenizer().TokenizeInputText(prompt)
				require.NoError(t, err)
				for round := 0; round < tc.rounds; round++ {
					ctx := types.NewRoutingContext(context.Background(), tc.algorithm, model, prompt, fmt.Sprintf("decision-%d", round), "")
					// Pre-cached tokens exercise both indexes without a remote tokenizer service.
					ctx.SetPrefixTokens(tokens)
					zero, k := 0, tc.k
					ctx.ConfigProfile = &types.ResolvedConfigProfile{Routing: &types.RoutingConfig{
						Candidate: &types.CandidateProfileConfig{K: &k},
						AutoBlend: &types.AutoBlendProfileConfig{LoadBalanceWeight: &zero, LeastRequestWeight: &zero},
					}}
					if tc.invalidate {
						c.beforeCounts = func() { ctx.SetPrefixTokens(append([]byte(nil), tokens...)) }
					}
					before := prefixMatchOperations(t, model)
					_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
					require.NoError(t, err)
					require.NotNil(t, ctx.TargetPod())
					require.Nil(t, ctx.SeededPrefixMatch(), "the gateway releases each decision's retained result")
					after := prefixMatchOperations(t, model)
					var lookups float64
					for result, count := range after {
						if result != "reuse" {
							lookups += count - before[result]
						}
					}
					require.Equal(t, tc.lookups, lookups, "round %d: real index queries, including fallback", round)
					require.Equal(t, tc.reuses, after["reuse"]-before["reuse"], "round %d: scoring/post-update reuse", round)
					ready := map[string]struct{}{utils.GeneratePodKey(ctx.TargetPod().Namespace, ctx.TargetPod().Name): {}}
					var matched map[string]int
					if kvSync {
						matched, _ = syncindexer.GetSharedSyncPrefixHashTable().MatchPrefix(model, -1, tokens, ready)
					} else {
						matched, _ = index.MatchPrefix(tokens, model, ready)
					}
					require.Equal(t, 100, matched[utils.GeneratePodKey(ctx.TargetPod().Namespace, ctx.TargetPod().Name)], "post-route credit must reach the real index")
					ctx.Delete()
				}
			})
		}
	}
}
