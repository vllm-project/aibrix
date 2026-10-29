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

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// TestSelectTargetPod_CandidateStageBoundsRedisReads routes one least-request request over 200
// ready pods against a real Redis protocol server and counts the commands the routing path
// sends. Without the stage, every ready pod's running count is read once per decision (the
// gate, least-request and the load-balance blend share that read, #3329). With
// routingConfig.candidate.k set, only the candidates are read, so the command count stops
// depending on the fleet size.
func TestSelectTargetPod_CandidateStageBoundsRedisReads(t *testing.T) {
	const n = 200
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

	route := func(routingConfig string) int {
		t.Helper()
		ctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "req-"+routingConfig, "")
		t.Cleanup(ctx.Delete)
		if routingConfig != "" {
			raw := json.RawMessage(routingConfig)
			ctx.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: raw, Routing: configprofiles.ParseRoutingConfig(raw)}
		}
		before := mr.CommandCount()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		return mr.CommandCount() - before
	}

	full := route("")
	sampled := route(`{"candidate":{"k":8}}`)
	t.Logf("Redis commands per request over %d pods: %d without the candidate stage, %d with k=8", n, full, sampled)
	assert.GreaterOrEqual(t, full, n, "without the stage every ready pod is read")
	assert.LessOrEqual(t, sampled, 8+8, "with the candidate stage only the 8 candidates are read")
}
