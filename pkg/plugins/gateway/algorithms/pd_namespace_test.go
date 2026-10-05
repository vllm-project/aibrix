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

package routingalgorithms

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// sameNamedPDPod returns a ready "worker-0" pod in namespace that belongs to roleset.
func sameNamedPDPod(namespace, ip, roleset string) *v1.Pod {
	pod := sameNamedPod(namespace, ip)
	pod.Labels = map[string]string{PDRoleSetIdentifier: roleset}
	return pod
}

// TestPDPrefillImbalance_SameNamedPodsInTwoNamespaces checks that in-flight prefill
// requests on one pod are not merged with a same-named pod in another namespace, so
// the load-imbalance check still sees the idle pod and selects it.
func TestPDPrefillImbalance_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPDPod("team-a", "10.0.0.1", "rs-a")
	teamB := sameNamedPDPod("team-b", "10.0.0.2", "rs-b")
	pods := []*v1.Pod{teamB, teamA}

	tracker := pd.NewPrefillRequestTracker()
	busy := int(aibrixPrefillLoadImbalanceMinSpread) + 1
	for i := 0; i < busy; i++ {
		tracker.AddPrefillRequest(fmt.Sprintf("req-%d", i), utils.GeneratePodKey(teamB.Namespace, teamB.Name))
	}

	r := &pdRouter{}
	targetPod, imbalanced := r.loadImbalanceSelectPrefillPod(pods,
		tracker.GetPrefillRequestCountsForPods(pods), aibrixPrefillLoadImbalanceMinSpread)
	require.True(t, imbalanced, "team-b/worker-0 is busy and team-a/worker-0 is idle")
	require.NotNil(t, targetPod)
	assert.Equal(t, "team-a", targetPod.Namespace)
}

// TestPDDecodeScoring_SameNamedPodsInTwoNamespaces checks that decode request counts
// collected for one pod are not applied to a same-named pod in another namespace.
func TestPDDecodeScoring_SameNamedPodsInTwoNamespaces(t *testing.T) {
	teamA := sameNamedPDPod("team-a", "10.0.0.1", "rs-a")
	teamB := sameNamedPDPod("team-b", "10.0.0.2", "rs-b")
	pods := []*v1.Pod{teamB, teamA}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"worker-0": {
			metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 0},
			metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 100},
			metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.5},
		},
	})

	pending := pd.NewPendingDecodeTracker()
	for i := 0; i < 10; i++ {
		pending.AddPendingDecode(fmt.Sprintf("req-%d", i), utils.GeneratePodKey(teamB.Namespace, teamB.Name))
	}
	r := &pdRouter{cache: c, pendingDecodeTracker: pending}
	ctx := &types.RoutingContext{RequestID: "test-request", Model: "m1"}

	_, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage :=
		r.loadImbalanceSelectDecodePod(ctx, pods)
	run := r.scoreDecodePods(ctx, pods, maxRequestCount, maxThroughput, maxFreeGPUUsage,
		podRequestCounts, podThroughputs, podFreeGpuUsage, pd.LeastRequestDecodePolicy{})
	require.NoError(t, run.Err)
	require.Contains(t, run.PerRoleset, "rs-a")
	require.Contains(t, run.PerRoleset, "rs-b")
	assert.Equal(t, 0.0, run.PerRoleset["rs-a"].Score, "team-a/worker-0 has no requests")
	assert.Equal(t, 10.0, run.PerRoleset["rs-b"].Score, "team-b/worker-0 has 10 pending decodes")
}
