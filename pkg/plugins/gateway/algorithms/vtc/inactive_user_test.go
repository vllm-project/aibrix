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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestExpiredInactiveUserDoesNotInflateGlobalMaximum(t *testing.T) {
	config := DefaultVTCConfig()
	config.InputTokenWeight = 1
	tracker := NewInMemorySlidingWindowTokenTracker(&config,
		WithWindowSize(60000), WithTimeUnit(Milliseconds)).(*InMemorySlidingWindowTokenTracker)
	ctx := context.Background()
	require.NoError(t, tracker.UpdateTokenCount(ctx, "departed", 100000, 0))
	backdateOldestBucket(tracker, "departed", 2*time.Minute)
	require.NoError(t, tracker.UpdateTokenCount(ctx, "live", 5000, 0))

	// The departed user never sends another request or receives an explicit read.
	live, err := tracker.GetTokenCount(ctx, "live")
	require.NoError(t, err)
	require.Equal(t, 5000.0, live)
	maximum, err := tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5000.0, maximum, "global maximum must exclude expired inactive users")
}

func TestExpiredInactiveUserDoesNotSkewPodScores(t *testing.T) {
	config := DefaultVTCConfig()
	router := &BasicVTCRouter{
		cache: NewSimpleCache(), config: &config,
		tokenTracker:   NewInMemorySlidingWindowTokenTracker(&config),
		tokenEstimator: NewSimpleTokenEstimator(),
	}
	ctx := types.NewRoutingContext(context.Background(), RouterVTCBasic, "model1", "message", "req-expired-user", "live")
	t.Cleanup(ctx.Delete)
	overrides := EnvOverrides()
	overrides.InputTokenWeight, overrides.OutputTokenWeight = 1, 1
	overrides.FairnessWeight, overrides.UtilizationWeight, overrides.MaxPodLoad = 1, 1, 100
	overrides.TokenTracker = types.VTCTokenTrackerOverrides{
		WindowSize: 60000, TimeUnit: "milliseconds", MinTokens: 1000, MaxTokens: 8000,
	}
	ctx.SetRoutingOverrides(&types.RoutingOverrides{VTC: overrides})
	scoped, _ := router.trackerFor(ctx)
	tracker, ok := scoped.(*InMemorySlidingWindowTokenTracker)
	require.True(t, ok)
	require.NoError(t, tracker.UpdateTokenCount(ctx.Context, "departed", 100000, 0))
	backdateOldestBucket(tracker, "departed", 2*time.Minute)
	require.NoError(t, tracker.UpdateTokenCount(ctx.Context, "live", 5000, 0))

	// Both pods have zero load. Current-window service puts live at index 1.
	pods := createTestPodsForMetrics(2)
	scores, scored, err := router.ScoreAll(ctx, NewSimplePodList(pods))
	require.NoError(t, err)
	require.Len(t, scores, 2)
	require.Equal(t, []bool{true, true}, scored)
	require.Equal(t, types.PolarityLeast, router.Polarity())
	assert.Greater(t, scores[0]-scores[1], 0.8,
		"pod-metrics-b (index 1) must beat pod-metrics-a beyond the 0.1 random term")
}
