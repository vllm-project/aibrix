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
	"math"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

// withTrackerDefaults pins the process default table to the VTC environment
// values for one test, so the tracker a request resolves to is compared against
// a known table instead of whatever the test run's environment configures.
func withTrackerDefaults(t *testing.T) {
	t.Helper()
	restore := types.DefaultRoutingOverrides()
	types.SetDefaultRoutingOverrides(&types.RoutingOverrides{VTC: EnvOverrides()})
	t.Cleanup(func() { types.SetDefaultRoutingOverrides(restore) })
}

// newTrackerTestRouter builds a router the way NewVTCBasicRouter does, with the
// process-wide tracker the environment configures.
func newTrackerTestRouter() *BasicVTCRouter {
	config := DefaultVTCConfig()
	return &BasicVTCRouter{
		tokenTracker:   NewInMemorySlidingWindowTokenTracker(&config),
		tokenEstimator: NewSimpleTokenEstimator(),
		config:         &config,
	}
}

// scopedCtx returns a routing context whose VTC overrides carry knobs and the
// two token weights, the way ResolveRoutingOverrides parks them.
func scopedCtx(t *testing.T, knobs types.VTCTokenTrackerOverrides, inputWeight, outputWeight float64) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterVTCBasic, "model1", "message", "req-tracker-scope", "user1")
	t.Cleanup(ctx.Delete)
	overrides := EnvOverrides()
	overrides.InputTokenWeight = inputWeight
	overrides.OutputTokenWeight = outputWeight
	overrides.TokenTracker = knobs
	ctx.SetRoutingOverrides(&types.RoutingOverrides{VTC: overrides})
	return ctx
}

// TestTrackerForKeepsTheProcessTrackerForDefaultKnobs pins the zero-cost case:
// a request whose profile leaves the six tracker knobs at the process defaults
// shares the one tracker the router was built with, exactly as before the
// profile overrides existed.
func TestTrackerForKeepsTheProcessTrackerForDefaultKnobs(t *testing.T) {
	withTrackerDefaults(t)
	router := newTrackerTestRouter()

	plain := types.NewRoutingContext(context.Background(), RouterVTCBasic, "model1", "message", "req-plain", "user1")
	t.Cleanup(plain.Delete)
	tracker, knobs := router.trackerFor(plain)
	assert.Same(t, router.tokenTracker, tracker, "a request without a profile shares the process-wide tracker")
	assert.Equal(t, EnvTokenTrackerKnobs(), knobs, "and the process-wide tracker's floors come with it")

	// A profile that only retunes the per-request score knobs still resolves
	// all six tracker knobs to the process defaults.
	scored := scopedCtx(t, EnvTokenTrackerKnobs(), inputTokenWeight, outputTokenWeight)
	tracker, knobs = router.trackerFor(scored)
	assert.Same(t, router.tokenTracker, tracker)
	assert.Equal(t, EnvTokenTrackerKnobs(), knobs)
}

// TestTrackerForScopesTheTrackerToTheResolvedKnobs checks that a profile that
// changes a tracker knob or a token weight gets a tracker of its own, that
// profiles resolving the same knobs share one, and that neither reads nor
// writes leak between them.
func TestTrackerForScopesTheTrackerToTheResolvedKnobs(t *testing.T) {
	withTrackerDefaults(t)
	router := newTrackerTestRouter()

	knobs := EnvTokenTrackerKnobs()
	knobs.WindowSize += 30
	knobs.TimeUnit = "seconds"
	knobs.MinTokens = 1234
	knobs.MaxTokens = 4321

	first := scopedCtx(t, knobs, 3.0, 4.0)
	second := scopedCtx(t, knobs, 3.0, 4.0)
	otherWeight := scopedCtx(t, knobs, 5.0, 4.0)

	firstTracker, firstKnobs := router.trackerFor(first)
	secondTracker, _ := router.trackerFor(second)
	otherTracker, _ := router.trackerFor(otherWeight)

	assert.NotSame(t, router.tokenTracker, firstTracker, "a profile that changes the window gets its own tracker")
	assert.Equal(t, knobs, firstKnobs, "the scoped tracker comes with the scope's floors")
	assert.Same(t, firstTracker, secondTracker, "profiles that resolve the same knobs share one tracker")
	assert.NotSame(t, firstTracker, otherTracker, "a different weight resolves a different tracker")

	scoped, ok := firstTracker.(*InMemorySlidingWindowTokenTracker)
	require.True(t, ok)
	assert.Equal(t, time.Duration(knobs.WindowSize)*time.Second, scoped.windowSize, "the tracker keeps the profile's window")
	require.NotNil(t, scoped.config)
	assert.Equal(t, 3.0, scoped.config.InputTokenWeight)
	assert.Equal(t, 4.0, scoped.config.OutputTokenWeight)

	// Both families of the profile's knobs land in the tracker: the floors it
	// reports while the window holds no activity, and the weights it prices an
	// update with.
	minTokens, err := firstTracker.GetMinTokenCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1234.0, minTokens)
	maxTokens, err := firstTracker.GetMaxTokenCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4321.0, maxTokens)

	require.NoError(t, firstTracker.UpdateTokenCount(context.Background(), "user1", 10, 20))
	total, err := firstTracker.GetTokenCount(context.Background(), "user1")
	require.NoError(t, err)
	assert.Equal(t, 10*3.0+20*4.0, total)

	total, err = otherTracker.GetTokenCount(context.Background(), "user1")
	require.NoError(t, err)
	assert.Zero(t, total, "another scope must not see the update")
	total, err = router.tokenTracker.GetTokenCount(context.Background(), "user1")
	require.NoError(t, err)
	assert.Zero(t, total, "the process-wide tracker must not see a scoped profile's activity")
}

// TestTrackerForConcurrentRequestsShareOneTrackerPerScope exercises the
// build-outside-the-lock path of the registry: racing requests of one profile
// must end up on the same tracker.
func TestTrackerForConcurrentRequestsShareOneTrackerPerScope(t *testing.T) {
	withTrackerDefaults(t)
	router := newTrackerTestRouter()

	knobs := EnvTokenTrackerKnobs()
	knobs.WindowSize += 7
	ctx := scopedCtx(t, knobs, inputTokenWeight, outputTokenWeight)

	const callers = 16
	trackers := make([]TokenTracker, callers)
	var wg sync.WaitGroup
	for i := range trackers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			trackers[i], _ = router.trackerFor(ctx)
		}(i)
	}
	wg.Wait()

	assert.NotSame(t, router.tokenTracker, trackers[0])
	for i, tracker := range trackers {
		assert.Same(t, trackers[0], tracker, "caller %d got a different tracker", i)
	}
}

// TestTrackerScopeLimitFallsBackToTheProcessTracker checks the bound on scoped
// state: beyond maxTrackerScopes a profile keeps the process-wide tracker and
// its requests still route, instead of failing or growing the process without
// bound.
func TestTrackerScopeLimitFallsBackToTheProcessTracker(t *testing.T) {
	withTrackerDefaults(t)
	router := newTrackerTestRouter()

	base := EnvTokenTrackerKnobs()
	seen := make(map[TokenTracker]struct{})
	for i := 1; i <= maxTrackerScopes; i++ {
		knobs := base
		knobs.WindowSize = base.WindowSize + 100 + i
		tracker, _ := router.trackerFor(scopedCtx(t, knobs, inputTokenWeight, outputTokenWeight))
		require.NotSame(t, router.tokenTracker, tracker, "scope %d must get its own tracker", i)
		seen[tracker] = struct{}{}
	}
	require.Len(t, seen, maxTrackerScopes, "every scope must have its own tracker")

	knobs := base
	knobs.WindowSize = base.WindowSize + 1000
	// Ask for a floor far above the shared tracker's, so scoring with the
	// profile's floors instead of the tracker's would move the metric below.
	knobs.MinTokens = base.MaxTokens + 1000
	overflow := scopedCtx(t, knobs, inputTokenWeight, outputTokenWeight)
	tracker, trackerKnobs := router.trackerFor(overflow)
	assert.Same(t, router.tokenTracker, tracker, "beyond the limit a request keeps the process-wide tracker")
	assert.Equal(t, base, trackerKnobs, "and it keeps that tracker's floors with it")

	testGauge, cleanup := metrics.SetupMetricsForTest(metrics.VTCBucketSizeActive, []string{"pod", "model"})
	defer cleanup()

	pods := createTestPodsForMetrics(2)
	addr, err := router.Route(overflow, NewSimplePodList(pods))
	require.NoError(t, err)
	assert.NotEmpty(t, addr, "the fallback keeps the request routable on the environment values")

	// The shared tracker reports its own floors, so the bucket size must come
	// from them: the profile's floor is far above them, and using it would
	// score the request against a floor the tracker does not have.
	wantBucket := math.Max(base.MinTokens, (base.MinTokens+base.MaxTokens)/2)
	for _, pod := range pods {
		assert.Equal(t, wantBucket, testutil.ToFloat64(testGauge.WithLabelValues(pod.Name, "model1")),
			"the fallback must be scored with the shared tracker's floors")
	}
}

// TestWithWindowSizeDoesNotLeakIntoTheProcessDefault guards the change that let
// a scoped tracker take its own window: the option used to overwrite the
// process-wide default, so one profile's window silently retuned every tracker
// created afterwards.
func TestWithWindowSizeDoesNotLeakIntoTheProcessDefault(t *testing.T) {
	before := tokenTrackerWindowSize
	config := DefaultVTCConfig()

	scoped, ok := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(42), WithTimeUnit(Seconds)).(*InMemorySlidingWindowTokenTracker)
	require.True(t, ok)
	assert.Equal(t, 42*time.Second, scoped.windowSize)
	assert.Equal(t, before, tokenTrackerWindowSize, "building a tracker must not retune the process default")

	after, ok := NewInMemorySlidingWindowTokenTracker(&config).(*InMemorySlidingWindowTokenTracker)
	require.True(t, ok)
	want := time.Duration(before) * timeUnitDuration[timeUnitFromName(timeUnitStr)]
	assert.Equal(t, want, after.windowSize, "a tracker built afterwards keeps the process default window")
}
