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

package pd

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var bucketServeTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testBucketServeConfig(mode BucketMode) BucketServeConfig {
	cfg := DefaultBucketServeConfig()
	cfg.Enabled = true
	cfg.Mode = mode
	return cfg
}

// observeCount records count requests of one prompt length.
func observeCount(tracker *BucketServeTracker, model string, length, count int, now time.Time) {
	for i := 0; i < count; i++ {
		tracker.Observe(model, length, now)
	}
}

func TestBucketServePlanSplitsSharedRange(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 100, 100, bucketServeTestNow)
	observeCount(tracker, "m", 4000, 100, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, groups)

	require.True(t, plan.Events.Refreshed)
	assert.Equal(t, 1, plan.Events.Splits, "one cut creates the two bands")
	require.Len(t, plan.Bands, 2)
	// The bands partition the shared range without gaps.
	assert.Equal(t, 0, plan.Bands[0].Min)
	assert.Equal(t, 4096, plan.Bands[1].Max)
	assert.Equal(t, plan.Bands[0].Max+1, plan.Bands[1].Min)
	assert.Equal(t, "dep-a", plan.Bands[0].Group)
	assert.Equal(t, "dep-b", plan.Bands[1].Group)

	group, ok := plan.GroupFor(100)
	require.True(t, ok)
	assert.Equal(t, "dep-a", group)
	group, ok = plan.GroupFor(4000)
	require.True(t, ok)
	assert.Equal(t, "dep-b", group)
}

func TestBucketServeModeMovesTheCut(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	observe := func(tracker *BucketServeTracker) {
		// A uniform count distribution over [200, 4000]: every length gets one
		// request, so request-count quantiles and token-mass quantiles differ.
		for length := 200; length <= 4000; length += 20 {
			tracker.Observe("m", length, bucketServeTestNow)
		}
	}

	rps := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observe(rps)
	rpsPlan := rps.Plan("m", bucketServeTestNow, groups)
	require.Len(t, rpsPlan.Bands, 2)

	throughput := NewBucketServeTracker(testBucketServeConfig(BucketModeThroughput))
	observe(throughput)
	throughputPlan := throughput.Plan("m", bucketServeTestNow, groups)
	require.Len(t, throughputPlan.Bands, 2)

	// Token mass sits on the longer prompts, so the throughput cut is placed
	// further up the length axis than the request-count cut.
	assert.Less(t, rpsPlan.Bands[0].Max, throughputPlan.Bands[0].Max)
	assert.Equal(t, "dep-a", rpsPlan.Bands[0].Group)
	assert.Equal(t, "dep-b", throughputPlan.Bands[1].Group)
}

func TestBucketServeKeepsThinSharedIntervalsWhole(t *testing.T) {
	groups := []BucketGroup{
		{Name: "short-a", Min: 0, Max: 4000},
		{Name: "short-b", Min: 0, Max: 4000},
		{Name: "long-a", Min: 4001, Max: 40000},
		{Name: "long-b", Min: 4001, Max: 40000},
	}
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 2000, 1000, bucketServeTestNow)
	observeCount(tracker, "m", 30000, 20, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, groups)

	// The busy interval is split into one band per roleset.
	group, ok := plan.GroupFor(2000)
	require.True(t, ok)
	assert.Contains(t, []string{"short-a", "short-b"}, group)
	// The thin interval stays whole: splitting it would starve a roleset.
	group, ok = plan.GroupFor(30000)
	assert.False(t, ok)
	assert.Empty(t, group)
	require.Len(t, plan.Bands, 3)
	assert.Empty(t, plan.Bands[2].Group)
}

func TestBucketServeMaxBandsCapKeepsIntervalsWhole(t *testing.T) {
	cfg := testBucketServeConfig(BucketModeRPS)
	cfg.MaxBands = 2
	groups := []BucketGroup{
		{Name: "a", Min: 0, Max: 4096},
		{Name: "b", Min: 0, Max: 4096},
		{Name: "c", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker(cfg)
	observeCount(tracker, "m", 1000, 100, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, groups)

	require.Len(t, plan.Bands, 1)
	assert.Empty(t, plan.Bands[0].Group)
	_, ok := plan.GroupFor(1000)
	assert.False(t, ok)
}

func TestBucketServeMaxLengthGroupDoesNotOverflow(t *testing.T) {
	// An open range carries math.MaxInt32 in production, which is math.MaxInt on
	// a 32-bit build. The plan adds one to a group's upper bound while it builds
	// its segment bounds, so a bound left at the top of int would wrap there.
	groups := []BucketGroup{
		{Name: "a", Min: 0, Max: math.MaxInt},
		{Name: "b", Min: 0, Max: math.MaxInt},
	}
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 1000, 100, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, groups)

	require.NotEmpty(t, plan.Bands)
	last := plan.Bands[len(plan.Bands)-1]
	assert.LessOrEqual(t, last.Max, math.MaxInt-1, "an upper bound that takes +1 must stay below the top of int")
	for i := 0; i+1 < len(plan.Bands); i++ {
		assert.Equal(t, plan.Bands[i].Max+1, plan.Bands[i+1].Min, "the bands partition the range without gaps")
	}
	group, ok := plan.GroupFor(1000)
	require.True(t, ok)
	assert.Contains(t, []string{"a", "b"}, group)
}

func TestBucketServePlanRefreshAndCutEvents(t *testing.T) {
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 1000, 1, bucketServeTestNow)
	observeCount(tracker, "m", 2000, 1, bucketServeTestNow)
	shared := []BucketGroup{
		{Name: "a", Min: 0, Max: 4096},
		{Name: "b", Min: 0, Max: 4096},
	}

	first := tracker.Plan("m", bucketServeTestNow, shared)
	require.True(t, first.Events.Refreshed)
	assert.Equal(t, 1, first.Events.Splits)
	require.Len(t, first.Bands, 2)

	cached := tracker.Plan("m", bucketServeTestNow.Add(time.Second), shared)
	assert.False(t, cached.Events.Refreshed, "within the refresh interval the plan is cached")
	assert.Equal(t, first.Bands, cached.Bands)

	// A disjoint fleet removes the shared interval, so the cut is merged away.
	disjoint := []BucketGroup{
		{Name: "a", Min: 0, Max: 1000},
		{Name: "b", Min: 1001, Max: 4096},
	}
	merged := tracker.Plan("m", bucketServeTestNow.Add(time.Second), disjoint)
	require.True(t, merged.Events.Refreshed)
	assert.Equal(t, 1, merged.Events.Merges)
	assert.Zero(t, merged.Events.Splits)
	_, ok := merged.GroupFor(500)
	assert.False(t, ok)
}

func TestBucketServeSingleGroupHasNoAffinity(t *testing.T) {
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 1000, 100, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, []BucketGroup{{Name: "solo", Min: 0, Max: 4096}})

	require.Len(t, plan.Bands, 1)
	assert.Empty(t, plan.Bands[0].Group)
	_, ok := plan.GroupFor(1000)
	assert.False(t, ok)
}

func TestBucketServeSegmentsRespectDeclaredRanges(t *testing.T) {
	groups := []BucketGroup{
		{Name: "a", Min: 0, Max: 4096},
		{Name: "b", Min: 0, Max: 8192},
	}
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 100, 200, bucketServeTestNow)
	observeCount(tracker, "m", 4000, 200, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, groups)

	// [0, 4096] is shared and split; [4097, 8192] is served by b alone and
	// carries no assignment.
	require.Len(t, plan.Bands, 3)
	assert.Equal(t, 0, plan.Bands[0].Min)
	assert.LessOrEqual(t, plan.Bands[0].Max, 4096)
	assert.Equal(t, plan.Bands[0].Max+1, plan.Bands[1].Min)
	assert.Equal(t, 4096, plan.Bands[1].Max)
	assert.Equal(t, 4097, plan.Bands[2].Min)
	assert.Equal(t, 8192, plan.Bands[2].Max)
	assert.Empty(t, plan.Bands[2].Group)

	group, ok := plan.GroupFor(100)
	require.True(t, ok)
	assert.Equal(t, "a", group)
	group, ok = plan.GroupFor(4000)
	require.True(t, ok)
	assert.Equal(t, "b", group)
}

func TestBucketServeObservationsDecay(t *testing.T) {
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	tracker.Observe("m", 1000, bucketServeTestNow)

	total := func() float64 {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		sum := 0.0
		for _, w := range tracker.models["m"].weights {
			sum += w
		}
		return sum
	}
	assert.InDelta(t, 1.0, total(), 1e-9)

	// One half-life later the first observation carries half its weight, so
	// the model tracks recent traffic rather than all traffic ever seen.
	tracker.Observe("m", 1000, bucketServeTestNow.Add(DefaultBucketServeHalfLife))
	assert.InDelta(t, 1.5, total(), 1e-6)
}

func TestBucketServeNilTrackerIsInert(t *testing.T) {
	var tracker *BucketServeTracker
	tracker.Observe("m", 100, bucketServeTestNow)
	plan := tracker.Plan("m", bucketServeTestNow, []BucketGroup{{Name: "a", Min: 0, Max: 10}})

	assert.Empty(t, plan.Bands)
	assert.False(t, plan.Events.Refreshed)
	assert.Equal(t, DefaultBucketServeConfig(), tracker.Config())
}

func TestBucketServeNoGroupsMeansNoPlan(t *testing.T) {
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	observeCount(tracker, "m", 1000, 10, bucketServeTestNow)

	plan := tracker.Plan("m", bucketServeTestNow, nil)

	assert.Empty(t, plan.Bands)
	_, assigned := plan.GroupFor(1000)
	assert.False(t, assigned)
}

func TestEnvBucketServeConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		enabled bool
		mode    BucketMode
	}{
		{
			name:    "unset keeps the shipped defaults",
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "", "AIBRIX_BUCKET_SERVE_MODE": ""},
			enabled: false,
			mode:    BucketModeThroughput,
		},
		{
			name:    "the switch turns the plan on",
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "true", "AIBRIX_BUCKET_SERVE_MODE": "rps"},
			enabled: true,
			mode:    BucketModeRPS,
		},
		{
			name:    "a differently cased mode name is the mode it names",
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "true", "AIBRIX_BUCKET_SERVE_MODE": "THROUGHPUT"},
			enabled: true,
			mode:    BucketModeThroughput,
		},
		{
			name:    "an unknown mode keeps the default",
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "1", "AIBRIX_BUCKET_SERVE_MODE": "throughput-ish"},
			enabled: true,
			mode:    BucketModeThroughput,
		},
		{
			name:    "an unreadable switch keeps the plan off",
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "yes", "AIBRIX_BUCKET_SERVE_MODE": "rps"},
			enabled: false,
			mode:    BucketModeRPS,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			cfg := EnvBucketServeConfig()
			assert.Equal(t, tt.enabled, cfg.Enabled)
			assert.Equal(t, tt.mode, cfg.Mode)
		})
	}
}

func TestBucketServeConfigureSwitchesTheModePerModel(t *testing.T) {
	tracker := NewBucketServeTracker(testBucketServeConfig(BucketModeRPS))
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	// A uniform count distribution over [200, 4000]: equal request counts per
	// length, so the request-count quantile and the token-mass quantile differ.
	observe := func(model string) {
		for length := 200; length <= 4000; length += 20 {
			tracker.Observe(model, length, bucketServeTestNow)
		}
	}
	observe("m")
	observe("other")

	rpsPlan := tracker.Plan("m", bucketServeTestNow, groups)
	require.Len(t, rpsPlan.Bands, 2)

	// A request that resolves another mode re-plans its model: the cached plan
	// is dropped and the cut moves to the token-mass quantile.
	tracker.Configure("m", testBucketServeConfig(BucketModeThroughput))
	throughputPlan := tracker.Plan("m", bucketServeTestNow.Add(time.Second), groups)
	require.True(t, throughputPlan.Events.Refreshed, "the mode change drops the cached plan")
	require.Len(t, throughputPlan.Bands, 2)
	assert.Less(t, rpsPlan.Bands[0].Max, throughputPlan.Bands[0].Max)

	// The other model keeps the tracker's own configuration.
	otherPlan := tracker.Plan("other", bucketServeTestNow, groups)
	require.Len(t, otherPlan.Bands, 2)
	assert.Equal(t, rpsPlan.Bands[0].Max, otherPlan.Bands[0].Max,
		"another model still plans in the tracker's request-count mode")

	// Re-configuring with the same configuration keeps the cached plan.
	same := tracker.Plan("other", bucketServeTestNow.Add(time.Second), groups)
	assert.False(t, same.Events.Refreshed)
	tracker.Configure("other", testBucketServeConfig(BucketModeRPS))
	stillCached := tracker.Plan("other", bucketServeTestNow.Add(time.Second), groups)
	assert.False(t, stillCached.Events.Refreshed, "an unchanged configuration keeps the cached plan")
}

func TestBucketServeConfigureIsInertOnNilTrackerAndEmptyModel(t *testing.T) {
	var nilTracker *BucketServeTracker
	nilTracker.Configure("m", testBucketServeConfig(BucketModeRPS))

	cfg := testBucketServeConfig(BucketModeRPS)
	tracker := NewBucketServeTracker(cfg)
	tracker.Configure("", testBucketServeConfig(BucketModeThroughput))

	assert.Equal(t, cfg, tracker.Config(), "Configure never touches the tracker configuration")
	assert.Empty(t, tracker.models, "an empty model does not even create state")
}
