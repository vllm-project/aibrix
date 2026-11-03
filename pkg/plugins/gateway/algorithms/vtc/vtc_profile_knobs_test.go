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

package vtc

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// TestMain installs the VTC half of the process default table, the way the
// routing algorithm package does at startup, so a request without a profile
// reads the environment-derived knobs instead of zeros.
func TestMain(m *testing.M) {
	types.SetDefaultRoutingOverrides(&types.RoutingOverrides{VTC: EnvOverrides()})
	os.Exit(m.Run())
}

// TestVTCBasicProfileKnobsRetuneTheScore checks that the profile knobs of one request
// retune the VTC score for that request only: the same router and pods prefer the
// loaded replica under the environment defaults and the idle one under the profile.
func TestVTCBasicProfileKnobsRetuneTheScore(t *testing.T) {
	newRouter := func(c *SimpleCache) *BasicVTCRouter {
		trackerConfig := &VTCConfig{InputTokenWeight: 1.0, OutputTokenWeight: 1.0, Variant: RouterVTCBasic}
		return &BasicVTCRouter{
			cache:          c,
			tokenTracker:   NewInMemorySlidingWindowTokenTracker(trackerConfig, WithWindowSize(100), WithTimeUnit(Milliseconds)),
			tokenEstimator: NewSimpleTokenEstimator(),
			config:         trackerConfig,
		}
	}

	pods := createTestPodsForMetrics(2) // pod-metrics-a at 192.168.1.1, pod-metrics-b at 192.168.1.2
	podList := NewSimplePodList(pods)

	c := NewSimpleCache()
	c.SetPodMetric(utils.GeneratePodKey("default", "pod-metrics-a"), "model1", metrics.NumRequestsRunning, 2)
	c.SetPodMetric(utils.GeneratePodKey("default", "pod-metrics-b"), "model1", metrics.NumRequestsRunning, 0)

	router := newRouter(c)
	require.NoError(t, router.tokenTracker.UpdateTokenCount(context.Background(), "user1", 0, 0))

	// The process default table carries the environment values; the read sites
	// resolve the request's overrides on top of it.
	restore := types.DefaultRoutingOverrides()
	next := *restore
	next.VTC = types.VTCOverrides{MaxPodLoad: maxPodLoad, FairnessWeight: fairnessWeight, UtilizationWeight: utilizationWeight}
	types.SetDefaultRoutingOverrides(&next)
	t.Cleanup(func() { types.SetDefaultRoutingOverrides(restore) })

	// Environment defaults: fairness index 0 plus a light utilization term make the
	// loaded first replica win (about 0.02 against 1.0).
	plain := types.NewRoutingContext(context.Background(), "vtc-basic", "model1", "test message", "req-plain", "user1")
	addr, err := router.Route(plain, podList)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.1:8000", addr)

	// The profile drops the fairness term and saturates utilization at one request,
	// so the idle second replica wins (about 0.0 against 1.0). The utilization
	// weight it does not set keeps the process default.
	profiled := types.NewRoutingContext(context.Background(), "vtc-basic", "model1", "test message", "req-profiled", "user1")
	profiled.SetRoutingOverrides(&types.RoutingOverrides{VTC: types.VTCOverrides{
		MaxPodLoad:        1.0,
		FairnessWeight:    0.0,
		UtilizationWeight: utilizationWeight,
	}})
	addr, err = router.Route(profiled, podList)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.2:8000", addr)

	// ScoreAll reads the same knobs, with lower-is-better polarity.
	scores, scored, err := router.ScoreAll(profiled, podList)
	require.NoError(t, err)
	assert.True(t, scored[0] && scored[1])
	assert.Greater(t, scores[0], scores[1], "the loaded replica must score worse under the profile")
}
