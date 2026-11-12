/*
Copyright 2024 The Aibrix Team.

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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func withCapacityWarmup(t *testing.T, window time.Duration) {
	t.Helper()
	orig := loadBalanceCapacityWarmup
	loadBalanceCapacityWarmup = window
	t.Cleanup(func() { loadBalanceCapacityWarmup = orig })
}

// readySinceStore overrides the readySince the test store stamped at add time, by pod name.
type readySinceStore struct {
	*cache.Store
	readySince map[string]int64
	calls      int
}

func (s *readySinceStore) GetPodsReadySince(pods []*v1.Pod) (map[string]int64, error) {
	s.calls++
	out := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if rs, ok := s.readySince[pod.Name]; ok {
			out[utils.GeneratePodKey(pod.Namespace, pod.Name)] = rs
		}
	}
	return out, nil
}

func (s *readySinceStore) LatestPodReadySince() int64 {
	var latest int64
	for _, rs := range s.readySince {
		latest = max(latest, rs)
	}
	return latest
}

// skewedAfterRollout is the state #3450 measured one minute after a rollout: the pod that got
// ~73% of the first requests runs 5 and has a capacity estimate of 271 tokens/s, the other runs
// 2 at 96. Both estimates are just the traffic each pod happened to get, so their scores are
// about equal (5/271 = 0.0185, 2/96 = 0.0208) and the busy pod keeps winning.
func skewedAfterRollout() (*cache.Store, *utils.PodArray) {
	pods := []*v1.Pod{makeLBPod("busy", "1.1.1.1"), makeLBPod("idle", "2.2.2.2")}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", map[string]map[string]metrics.MetricValue{
		"busy": lbPodMetrics(5, 271, 0),
		"idle": lbPodMetrics(2, 96, 0),
	})
	return c, podsFromCache(c)
}

func lbScoresByName(t *testing.T, r *loadBalanceRouter, pods *utils.PodArray) map[string]float64 {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterLoadBalance, "m1", "input", "req1", "")
	scores, _, err := r.ScoreAll(ctx, pods)
	require.NoError(t, err)
	byName := map[string]float64{}
	for i, pod := range pods.All() {
		byName[pod.Name] = scores[i]
	}
	return byName
}

// TestLoadBalanceCapacityWarmup_JustRoutablePodsUseUniformCapacity is a regression test for
// #3450. Right after a rollout every pod's estimate covers only the first seconds of traffic.
// Scoring with it hands the next request to the busier pod; with the warm-up every pod gets the
// same capacity, so the pod with fewer running requests wins.
func TestLoadBalanceCapacityWarmup_JustRoutablePodsUseUniformCapacity(t *testing.T) {
	withCapacityWarmup(t, time.Minute)
	c, pods := skewedAfterRollout() // the test store stamps both pods routable now

	scores := lbScoresByName(t, &loadBalanceRouter{cache: c}, pods)
	assert.InDelta(t, 5.0, scores["busy"], 1e-9, "capacity 1.0: the score is the running count")
	assert.InDelta(t, 2.0, scores["idle"], 1e-9)
	assert.Less(t, scores["idle"], scores["busy"])
}

// TestLoadBalanceCapacityWarmup_OneYoungPodMakesEveryCapacityUniform covers a scale-up or the
// second pod of a rolling update: one pod is warm, the other just became routable. Every
// candidate gets the same capacity, so the warm pod's estimate cannot outweigh the new pod's
// partial one.
func TestLoadBalanceCapacityWarmup_OneYoungPodMakesEveryCapacityUniform(t *testing.T) {
	withCapacityWarmup(t, time.Minute)
	c, pods := skewedAfterRollout()
	now := time.Now()
	s := &readySinceStore{Store: c, readySince: map[string]int64{
		"busy": now.Add(-10 * time.Minute).UnixNano(),
		"idle": now.Add(-10 * time.Second).UnixNano(),
	}}

	scores := lbScoresByName(t, &loadBalanceRouter{cache: s}, pods)
	assert.InDelta(t, 5.0, scores["busy"], 1e-9)
	assert.InDelta(t, 2.0, scores["idle"], 1e-9)
}

// TestLoadBalanceCapacityWarmup_WarmFleetUsesEstimates checks that the warm-up does not touch a
// fleet whose pods have all been routable for longer than the window, or whose readySince is
// unknown (0 or missing), and that LatestPodReadySince skips the per-pod read for a warm fleet.
func TestLoadBalanceCapacityWarmup_WarmFleetUsesEstimates(t *testing.T) {
	withCapacityWarmup(t, time.Minute)
	old := time.Now().Add(-10 * time.Minute).UnixNano()

	for name, readySince := range map[string]map[string]int64{
		"both warm":     {"busy": old, "idle": old},
		"never stamped": {"busy": 0, "idle": 0},
		"no entry":      {},
	} {
		t.Run(name, func(t *testing.T) {
			c, pods := skewedAfterRollout()
			s := &readySinceStore{Store: c, readySince: readySince}

			scores := lbScoresByName(t, &loadBalanceRouter{cache: s}, pods)
			assert.InDelta(t, 5.0/271, scores["busy"], 1e-9, "the measured capacity is used")
			assert.InDelta(t, 2.0/96, scores["idle"], 1e-9)
			assert.Zero(t, s.calls, "a warm fleet is decided by LatestPodReadySince alone")
		})
	}
}

// TestLoadBalanceCapacityWarmup_Disabled keeps today's behavior when the window is 0.
func TestLoadBalanceCapacityWarmup_Disabled(t *testing.T) {
	withCapacityWarmup(t, 0)
	c, pods := skewedAfterRollout()

	scores := lbScoresByName(t, &loadBalanceRouter{cache: c}, pods)
	assert.InDelta(t, 5.0/271, scores["busy"], 1e-9)
	assert.InDelta(t, 2.0/96, scores["idle"], 1e-9)
}
