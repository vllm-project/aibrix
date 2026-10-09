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

package prediction

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

func seriesAt(now time.Time, offsets []time.Duration, values []float64) []types.MetricPoint {
	points := make([]types.MetricPoint, 0, len(offsets))
	for i, offset := range offsets {
		points = append(points, types.MetricPoint{
			Timestamp: now.Add(offset),
			Value:     values[i],
		})
	}
	return points
}

func TestProjectEvaluatesAtNowPlusHorizon(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	response, ok := Project(seriesAt(now,
		[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
		[]float64{10, 20, 30, 40}),
		now, 60*time.Second, 30*time.Second)

	require.True(t, ok)
	assert.InDelta(t, 25, response.ObservedValue, 0.0001)
	// The samples end at now, so the line at now is 40 and a 30s horizon
	// projects 70. Evaluating the window mean at the horizon would only
	// reach 55, a point 15s before the configured lead time.
	assert.InDelta(t, 70, response.PredictedValue, 0.0001)
	assert.InDelta(t, 1, response.Slope, 0.0001)
	assert.Equal(t, 4, response.DataPoints)
}

func TestProjectKeepsShortHorizonAheadOfNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// The centroid sits 15s before now, so a horizon below half the window
	// used to evaluate the line inside the observed window.
	response, ok := Project(seriesAt(now,
		[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
		[]float64{10, 20, 30, 40}),
		now, 60*time.Second, 10*time.Second)

	require.True(t, ok)
	assert.InDelta(t, 50, response.PredictedValue, 0.0001)
	assert.Greater(t, response.PredictedValue, 40.0)
}

func TestProjectFullWindowProjectsTheLeadTimePastNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Eighteen samples at 10s intervals span 170s of the 180s window of a
	// ramp that rises by 10 every 10s. The level at now is 190, so a 120s
	// horizon projects 310.
	offsets := make([]time.Duration, 0, 18)
	values := make([]float64, 0, 18)
	for i := 0; i < 18; i++ {
		offsets = append(offsets, time.Duration(i-17)*10*time.Second)
		values = append(values, float64(20+i*10))
	}

	response, ok := Project(seriesAt(now, offsets, values), now, 180*time.Second, 120*time.Second)

	require.True(t, ok)
	assert.InDelta(t, 105, response.ObservedValue, 0.0001)
	assert.InDelta(t, 310, response.PredictedValue, 0.0001)
}

func TestProjectRejectsSparseHistory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, ok := Project(seriesAt(now,
		[]time.Duration{-20 * time.Second, -10 * time.Second},
		[]float64{10, 20}),
		now, 60*time.Second, 30*time.Second)

	assert.False(t, ok)
}

func TestProjectRejectsShortCoverage(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, ok := Project(seriesAt(now,
		[]time.Duration{-20 * time.Second, -15 * time.Second, -10 * time.Second},
		[]float64{10, 20, 30}),
		now, 10*time.Minute, 30*time.Second)

	assert.False(t, ok)
}

func TestProjectClampsFallingTrendAtZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	response, ok := Project(seriesAt(now,
		[]time.Duration{-20 * time.Second, -10 * time.Second, 0},
		[]float64{100, 50, 0}),
		now, 40*time.Second, 100*time.Second)

	require.True(t, ok)
	assert.InDelta(t, 50, response.ObservedValue, 0.0001)
	assert.Equal(t, 0.0, response.PredictedValue)
}

func TestProjectCapsGrowthAgainstTheLevelAtNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Slope 1.5/s over samples ending at now puts the level at now at 35 and
	// the cap at 140. The old base, four times the window mean (20), would
	// cap at 80, below the current level of this ramp.
	response, ok := Project(seriesAt(now,
		[]time.Duration{-20 * time.Second, -10 * time.Second, 0},
		[]float64{10, 10, 40}),
		now, 40*time.Second, 200*time.Second)

	require.True(t, ok)
	assert.InDelta(t, 20, response.ObservedValue, 0.0001)
	assert.InDelta(t, 35, response.PredictedValue/4, 0.0001)
	assert.InDelta(t, 140, response.PredictedValue, 0.0001)
	assert.Greater(t, response.PredictedValue, 35.0)
}

func TestProjectIgnoresSamplesOutsideWindowAndFuture(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	response, ok := Project(seriesAt(now,
		[]time.Duration{-10 * time.Minute, -39 * time.Second, -29 * time.Second, -19 * time.Second, 10 * time.Second},
		[]float64{1000, 10, 20, 30, 5000}),
		now, 40*time.Second, 30*time.Second)

	require.True(t, ok)
	assert.Equal(t, 3, response.DataPoints)
	assert.InDelta(t, 20, response.ObservedValue, 0.0001)
	// The kept samples end 19s before now, so the line at now is 49 and a
	// 30s horizon projects 79.
	assert.InDelta(t, 79, response.PredictedValue, 0.0001)
}

func TestProjectDefaultsHorizonAndSortsSamples(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	response, ok := Project(seriesAt(now,
		[]time.Duration{-10 * time.Second, -30 * time.Second, 0, -20 * time.Second},
		[]float64{20, 20, 20, 20}),
		now, 60*time.Second, 0)

	require.True(t, ok)
	assert.Equal(t, 4, response.DataPoints)
	assert.InDelta(t, 20, response.PredictedValue, 0.0001)
}

func TestProjectRejectsNonPositiveWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, ok := Project(seriesAt(now,
		[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
		[]float64{10, 20, 30, 40}),
		now, 0, 30*time.Second)

	assert.False(t, ok)
}

func TestProjectSkipsNonFiniteSamples(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	response, ok := Project(seriesAt(now,
		[]time.Duration{-50 * time.Second, -40 * time.Second, -30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
		[]float64{10, 20, 30, math.NaN(), 40, math.Inf(1)}),
		now, 60*time.Second, 30*time.Second)

	require.True(t, ok)
	assert.Equal(t, 4, response.DataPoints)

	_, ok = Project(seriesAt(now,
		[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
		[]float64{math.NaN(), math.NaN(), math.Inf(-1), math.Inf(1)}),
		now, 60*time.Second, 30*time.Second)
	assert.False(t, ok)
}
