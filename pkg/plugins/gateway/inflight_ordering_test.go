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

// TestSelectTargetPod_RedisCommandsPerDecision counts the Redis commands one routing decision
// sends over a large ready set, for each combination of the per-replica inflight cap
// (requestsInflight) and the candidate stage (AIBRIX_ROUTING_CANDIDATE_K). It is the code-path
// check for #3463, which measured the same ordering against one Redis node: the cap's saturation
// filter runs over every ready pod before SelectCandidates, its read fills the per-decision
// running-request snapshot, and the candidate stage is left with nothing to save.
//
// Measured numbers are logged rather than only asserted, so the table in #3463 can be
// reproduced from the test output. The bounds below are the behavior this commit pins: the
// without-cap decision is bounded by K, and with the cap it is bounded by the ready set.
//
// The ready set here is 1,000 pods, not the 5,000 used in #3463: a 5,000-pod read can overrun the
// 100 ms bound on one running-request read (pkg/cache/cache_running_requests.go) under the race
// detector, which CI runs, and the test would then measure that timeout rather than the ordering.
// The counts scale with the ready set, so the comparison between the four cases is unaffected.
func TestSelectTargetPod_RedisCommandsPerDecision(t *testing.T) {
	const n = 1000
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%05d", i), Namespace: "ns"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.%d.%d.%d", i/(256*256), (i/256)%256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, "m")
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	route := func(name string, inflight int64, routingConfig string) int {
		t.Helper()
		ctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "req-"+name, "")
		t.Cleanup(ctx.Delete)
		cp := &types.ResolvedConfigProfile{}
		if routingConfig != "" {
			raw := json.RawMessage(routingConfig)
			cp.RoutingConfig = raw
			cp.Routing = configprofiles.ParseRoutingConfig(raw)
		}
		// requestsInflight is set on the model config profile, not in the routing config JSON,
		// so the test sets the resolved field the same way applyConfigProfile does.
		cp.RequestsInflight = inflight
		ctx.ConfigProfile = cp
		before := mr.CommandCount()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		return mr.CommandCount() - before
	}

	// A cap far above the fleet's load, so the filter keeps every pod and only costs reads.
	const limit = 100000
	baseline := route("baseline", 0, "")
	sampled := route("candidate", 0, `{"candidate":{"k":16}}`)
	inflight := route("inflight", limit, "")
	both := route("both", limit, `{"candidate":{"k":16}}`)

	t.Logf("Redis commands for one decision over %d ready pods:", n)
	t.Logf("  neither setting            %5d", baseline)
	t.Logf("  candidate k=16 only        %5d", sampled)
	t.Logf("  requestsInflight only      %5d", inflight)
	t.Logf("  both                       %5d", both)

	assert.GreaterOrEqual(t, baseline, n, "without any setting every ready pod is read")
	assert.Less(t, baseline, 2*n, "one read of the ready set, not two")
	assert.LessOrEqual(t, sampled, 40, "the candidate stage alone bounds the read")
	assert.GreaterOrEqual(t, inflight, n,
		"without the candidate stage the inflight filter reads every ready pod")
	// The upper bound is what catches a filter that reads Redis itself instead of through the
	// per-decision snapshot: that doubles the read, so the count would land near 2n.
	assert.Less(t, inflight, 2*n, "the filter shares the snapshot instead of reading twice")
	// Pinned current behavior, and the reason #3463 exists: the cap's filter reads the full
	// ready set first, which fills the snapshot the candidate stage would have read, so the
	// two settings do not compose. The ordering change proposes 22 here instead of 1,006.
	assert.GreaterOrEqual(t, both, n,
		"with requestsInflight the filter reads the full ready set before candidate selection, "+
			"so the candidate stage does not bound the read")
}
