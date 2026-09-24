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
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var bucketServeTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serveEven routes count requests with evenly spaced prompt lengths over
// [from, to], so the model's traffic picture is built through the request path.
func serveEven(tracker *BucketServeTracker, model string, mode BucketMode, from, to, count int, now time.Time) {
	for i := 0; i < count; i++ {
		length := from + (to-from)*i/(count-1)
		tracker.Band(model, mode, length, now, nil)
	}
}

// band routes count requests of one prompt length and returns the last result,
// the way a burst of requests of that length would.
func band(tracker *BucketServeTracker, model string, mode BucketMode, length, count int, now time.Time, groups []BucketGroup) BandResult {
	var res BandResult
	for i := 0; i < count; i++ {
		res = tracker.Band(model, mode, length, now, groups)
	}
	return res
}

// assertBandsTile requires the plan to be a sorted, gap-free and overlap-free
// sequence of bands over [min, max].
func assertBandsTile(t *testing.T, plan []BucketBand, min, max int) {
	t.Helper()
	require.NotEmpty(t, plan)
	assert.Equal(t, min, plan[0].Min)
	assert.Equal(t, max, plan[len(plan)-1].Max)
	for i := 1; i < len(plan); i++ {
		assert.Equal(t, plan[i-1].Max+1, plan[i].Min, "band %d must start where band %d ends", i, i-1)
		assert.Less(t, plan[i-1].Max, plan[i].Max, "bands must be sorted by upper bound")
	}
}

func TestBucketServeSplitsSharedRangeInReplicaProportion(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096, Replicas: 1},
		{Name: "dep-b", Min: 0, Max: 4096, Replicas: 3},
	}
	tracker := NewBucketServeTracker()
	// Even request counts over [200, 4000]: the request-count cut of a replica
	// share of three quarters sits three quarters up the range.
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 400, bucketServeTestNow)

	res := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	require.True(t, res.Refreshed)
	require.Len(t, res.Plan, 2)
	assertBandsTile(t, res.Plan, 0, 4096)
	// dep-b carries three quarters of the work, so it takes the lower band and
	// that band holds the larger part of the range.
	assert.Equal(t, "dep-b", res.Plan[0].Group)
	assert.Equal(t, "dep-a", res.Plan[1].Group)
	assert.Greater(t, res.Plan[0].Max, 2400, "the cut sits above the middle of the range")
	assert.Less(t, res.Plan[0].Max, 3600, "the cut sits below the top of the range")

	// The plan answers the request path for the band of its length.
	assert.Equal(t, "dep-b", tracker.Band("m", BucketModeRPS, 200, bucketServeTestNow.Add(time.Second), groups).Roleset)
	assert.Equal(t, "dep-a", tracker.Band("m", BucketModeRPS, 4000, bucketServeTestNow.Add(time.Second), groups).Roleset)
}

func TestBucketServeEqualReplicasSplitEvenly(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 400, bucketServeTestNow)

	res := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	require.Len(t, res.Plan, 2)
	assertBandsTile(t, res.Plan, 0, 4096)
	// A group without a replica count counts as one replica; the tie breaks by
	// name, so the cut lands near the middle of the traffic.
	assert.Equal(t, "dep-a", res.Plan[0].Group)
	assert.Equal(t, "dep-b", res.Plan[1].Group)
	assert.Greater(t, res.Plan[0].Max, 1000)
	assert.Less(t, res.Plan[0].Max, 3200)
}

func TestBucketServeChargesExclusiveTailsToTheWideRoleset(t *testing.T) {
	groups := []BucketGroup{
		{Name: "short", Min: 0, Max: 4096},
		{Name: "general", Min: 0, Max: 8192},
	}
	tracker := NewBucketServeTracker()
	// The shared interval carries fewer requests than the wide roleset's
	// exclusive tail: that roleset already holds more than its share, so the
	// shared interval needs no cut and only the narrow roleset bands it.
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 100, bucketServeTestNow)
	serveEven(tracker, "m", BucketModeRPS, 4200, 7800, 200, bucketServeTestNow)

	res := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	require.Len(t, res.Plan, 1, "only the roleset that still needs traffic holds a band")
	assertBandsTile(t, res.Plan, 0, 4096)
	assert.Equal(t, "short", res.Plan[0].Group)

	// The wide roleset's own tail carries no band: every covering roleset stays
	// a candidate there, which is the same preference as no plan at all.
	tail := tracker.Band("m", BucketModeRPS, 6000, bucketServeTestNow.Add(time.Second), groups)
	assert.Empty(t, tail.Roleset)
}

func TestBucketServeKeepsThinSharedIntervalsWhole(t *testing.T) {
	groups := []BucketGroup{
		{Name: "short-a", Min: 0, Max: 4000},
		{Name: "short-b", Min: 0, Max: 4000},
		{Name: "long-a", Min: 4001, Max: 40000},
		{Name: "long-b", Min: 4001, Max: 40000},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 1000, bucketServeTestNow)
	serveEven(tracker, "m", BucketModeRPS, 5000, 39000, 20, bucketServeTestNow)

	res := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	require.Len(t, res.Plan, 2, "the busy interval is split, the thin one is kept whole")
	assertBandsTile(t, res.Plan, 0, 4000)

	busy := tracker.Band("m", BucketModeRPS, 2000, bucketServeTestNow.Add(time.Second), groups)
	assert.Contains(t, []string{"short-a", "short-b"}, busy.Roleset)
	thin := tracker.Band("m", BucketModeRPS, 30000, bucketServeTestNow.Add(time.Second), groups)
	assert.Empty(t, thin.Roleset, "a thin shared interval keeps every covering roleset a candidate")
}

func TestBucketServeCapsBandsAtTheBudget(t *testing.T) {
	groups := make([]BucketGroup, 0, 17)
	for i := 0; i < 17; i++ {
		groups = append(groups, BucketGroup{Name: fmt.Sprintf("rs-%02d", i), Min: 0, Max: 4096})
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 1000, bucketServeTestNow)

	res := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	require.Len(t, res.Plan, bucketServeMaxBands, "the wide overlap is thinned to the band budget")
	assertBandsTile(t, res.Plan, 0, 4096)
	seen := map[string]bool{}
	for _, b := range res.Plan {
		assert.False(t, seen[b.Group], "every banded roleset holds one band")
		seen[b.Group] = true
	}
}

func TestBucketServeModeMovesTheCut(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 200, bucketServeTestNow)

	rps := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)
	throughput := band(tracker, "m", BucketModeThroughput, 200, 1, bucketServeTestNow, groups)

	require.Len(t, rps.Plan, 2)
	require.Len(t, throughput.Plan, 2)
	// Token mass sits on the longer prompts, so the throughput cut lands
	// further up the length axis than the request-count cut.
	assert.Less(t, rps.Plan[0].Max, throughput.Plan[0].Max)
}

func TestBucketServeNormalizesTheModeAndCachesPerMode(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 200, bucketServeTestNow)

	first := tracker.Band("m", "THROUGHPUT", 200, bucketServeTestNow, groups)
	require.True(t, first.Refreshed)

	// A mode name is normalized before it keys the plan cache, and a name the
	// planner does not know plans in the default mode.
	same := tracker.Band("m", BucketModeThroughput, 200, bucketServeTestNow.Add(time.Second), groups)
	assert.False(t, same.Refreshed)
	unknown := tracker.Band("m", "throughput-ish", 200, bucketServeTestNow.Add(2*time.Second), groups)
	assert.False(t, unknown.Refreshed)

	// The other mode keeps its own cached plan: it does not evict this one.
	rps := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(time.Second), groups)
	require.True(t, rps.Refreshed)
	again := tracker.Band("m", BucketModeThroughput, 200, bucketServeTestNow.Add(2*time.Second), groups)
	assert.False(t, again.Refreshed, "a second mode keeps the first plan cached")
}

func TestBucketServeRefreshesThePlanAfterTheInterval(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 200, bucketServeTestNow)

	require.True(t, band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups).Refreshed)
	assert.False(t, band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(time.Second), groups).Refreshed,
		"requests inside the refresh interval reuse the plan")
	assert.True(t, band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(bucketServeRefreshInterval), groups).Refreshed,
		"past the interval the plan is recomputed")

	// A changed fleet shape is a plan of its own, not the cached one.
	reshaped := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(bucketServeRefreshInterval+time.Second), groups[:1])
	assert.True(t, reshaped.Refreshed)
	assert.Empty(t, reshaped.Plan, "a range only one roleset declares needs no cut")
}

func TestBucketServeReportsRolesetsThatLeaveThePlan(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 200, bucketServeTestNow)

	first := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)
	require.Len(t, first.Plan, 2)
	assert.Empty(t, first.Dropped, "the first plan replaces no earlier plan")

	// The fleet shrinks: the new plan holds no band for either roleset, so the
	// caller is told to drop the gauge series of both.
	shrunk := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(bucketServeRefreshInterval), groups[:1])
	assert.ElementsMatch(t, []string{"dep-a", "dep-b"}, shrunk.Dropped)

	// A roleset is reported once: the next plan replaces one that already
	// holds no band.
	again := band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(2*bucketServeRefreshInterval), groups[:1])
	assert.Empty(t, again.Dropped)
}

func TestBucketServeDecaysTheTrafficPicture(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 100, bucketServeTestNow)
	band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow, groups)

	model := tracker.models["m"]
	require.NotNil(t, model)
	before := 0.0
	for _, weight := range model.weights {
		before += weight
	}
	require.InDelta(t, 101, before, 0.5, "the seeded requests plus the one that planned them")

	// Half a half-life later the picture has halved, and the request that
	// planned it is recorded on top of the faded picture.
	band(tracker, "m", BucketModeRPS, 200, 1, bucketServeTestNow.Add(bucketServeHalfLife), groups)
	after := 0.0
	for _, weight := range model.weights {
		after += weight
	}
	assert.InDelta(t, before*0.5+1, after, 0.5)
}

func TestBucketServeKeepsModelsSeparate(t *testing.T) {
	groups := []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
		{Name: "dep-b", Min: 0, Max: 4096},
	}
	tracker := NewBucketServeTracker()
	serveEven(tracker, "short-model", BucketModeRPS, 200, 2000, 200, bucketServeTestNow)
	serveEven(tracker, "long-model", BucketModeRPS, 3000, 4000, 200, bucketServeTestNow)

	short := band(tracker, "short-model", BucketModeRPS, 200, 1, bucketServeTestNow, groups)
	long := band(tracker, "long-model", BucketModeRPS, 4000, 1, bucketServeTestNow, groups)

	require.Len(t, short.Plan, 2)
	require.Len(t, long.Plan, 2)
	assert.Less(t, short.Plan[0].Max, long.Plan[0].Max, "each model plans against its own traffic")
}

func TestBucketServeSingleRolesetHasNoPlan(t *testing.T) {
	tracker := NewBucketServeTracker()
	res := tracker.Band("m", BucketModeRPS, 1000, bucketServeTestNow, []BucketGroup{
		{Name: "dep-a", Min: 0, Max: 4096},
	})

	assert.Empty(t, res.Roleset, "a range only one roleset declares needs no cut")
	assert.Empty(t, res.Plan)
}

func TestBucketServeWithoutGroupsHasNoPlan(t *testing.T) {
	tracker := NewBucketServeTracker()
	serveEven(tracker, "m", BucketModeRPS, 200, 4000, 100, bucketServeTestNow)

	res := tracker.Band("m", BucketModeRPS, 2000, bucketServeTestNow, nil)

	assert.Empty(t, res.Roleset)
	assert.Empty(t, res.Plan)
}

func TestBucketServeSegmentsClampTheTopOfInt(t *testing.T) {
	segs := bucketServeSegments([]BucketGroup{
		{Name: "open", Min: 0, Max: math.MaxInt},
		{Name: "short", Min: 0, Max: 4096},
	})

	require.Len(t, segs, 2)
	assert.Equal(t, 0, segs[0].min)
	assert.Equal(t, 4096, segs[0].max)
	assert.Len(t, segs[0].covering, 2)
	assert.Equal(t, 4097, segs[1].min)
	assert.Equal(t, math.MaxInt-1, segs[1].max, "an open upper bound moves down one so bounds cannot wrap")
	require.Len(t, segs[1].covering, 1)
	assert.Equal(t, "open", segs[1].covering[0].Name)
}

func TestBucketServeInertInputs(t *testing.T) {
	var nilTracker *BucketServeTracker
	assert.Empty(t, nilTracker.Band("m", BucketModeRPS, 100, bucketServeTestNow, nil).Roleset)

	tracker := NewBucketServeTracker()
	assert.Empty(t, tracker.Band("", BucketModeRPS, 100, bucketServeTestNow, nil).Roleset)
	assert.Empty(t, tracker.Band("m", BucketModeRPS, 0, bucketServeTestNow, nil).Roleset)
	assert.Empty(t, tracker.Band("m", BucketModeRPS, -5, bucketServeTestNow, nil).Roleset)
	assert.Empty(t, tracker.models, "inert input does not even create model state")
}

func TestEnvBucketServe(t *testing.T) {
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
			env:     map[string]string{"AIBRIX_BUCKET_SERVE": "1", "AIBRIX_BUCKET_SERVE_MODE": "THROUGHPUT"},
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
			enabled, mode := EnvBucketServe()
			assert.Equal(t, tt.enabled, enabled)
			assert.Equal(t, tt.mode, mode)
		})
	}
}
