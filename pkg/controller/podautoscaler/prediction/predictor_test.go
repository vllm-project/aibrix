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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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

func TestLinearPredictProjectsObservedTrend(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	response, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
			[]float64{10, 20, 30, 40}),
		Now:     now,
		Window:  60 * time.Second,
		Horizon: 30 * time.Second,
	})

	assert.True(t, ok)
	assert.InDelta(t, 25, response.ObservedValue, 0.0001)
	assert.InDelta(t, 55, response.PredictedValue, 0.0001)
	assert.InDelta(t, 1, response.Slope, 0.0001)
	assert.Equal(t, 4, response.DataPoints)
	assert.Equal(t, 30*time.Second, response.Horizon)
}

func TestLinearPredictRejectsSparseHistory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	_, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-20 * time.Second, -10 * time.Second},
			[]float64{10, 20}),
		Now:     now,
		Window:  60 * time.Second,
		Horizon: 30 * time.Second,
	})

	assert.False(t, ok)
}

func TestLinearPredictRejectsShortCoverage(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	_, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-20 * time.Second, -15 * time.Second, -10 * time.Second},
			[]float64{10, 20, 30}),
		Now:     now,
		Window:  10 * time.Minute,
		Horizon: 30 * time.Second,
	})

	assert.False(t, ok)
}

func TestLinearPredictClampsFallingTrendAtZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	response, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-20 * time.Second, -10 * time.Second, 0},
			[]float64{100, 50, 0}),
		Now:     now,
		Window:  40 * time.Second,
		Horizon: 100 * time.Second,
	})

	assert.True(t, ok)
	assert.InDelta(t, 50, response.ObservedValue, 0.0001)
	assert.Equal(t, 0.0, response.PredictedValue)
}

func TestLinearPredictCapsRunawayGrowth(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	response, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-20 * time.Second, -10 * time.Second, 0},
			[]float64{0, 0, 1000}),
		Now:     now,
		Window:  40 * time.Second,
		Horizon: 30 * time.Second,
	})

	assert.True(t, ok)
	assert.InDelta(t, 1000.0/3.0*MaxGrowthRatio, response.PredictedValue, 0.0001)
}

func TestLinearPredictIgnoresSamplesOutsideWindowAndFuture(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	response, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-10 * time.Minute, -39 * time.Second, -29 * time.Second, -19 * time.Second, 10 * time.Second},
			[]float64{1000, 10, 20, 30, 5000}),
		Now:     now,
		Window:  40 * time.Second,
		Horizon: 30 * time.Second,
	})

	assert.True(t, ok)
	assert.Equal(t, 3, response.DataPoints)
	assert.InDelta(t, 20, response.ObservedValue, 0.0001)
	assert.InDelta(t, 50, response.PredictedValue, 0.0001)
}

func TestLinearPredictDefaultsHorizon(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	response, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
			[]float64{20, 20, 20, 20}),
		Now:    now,
		Window: 60 * time.Second,
	})

	assert.True(t, ok)
	assert.Equal(t, DefaultHorizon, response.Horizon)
	assert.InDelta(t, 20, response.PredictedValue, 0.0001)
}

func TestLinearPredictRejectsNonPositiveWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predictor := NewLinear()

	_, ok := predictor.Predict(Request{
		Series: seriesAt(now,
			[]time.Duration{-30 * time.Second, -20 * time.Second, -10 * time.Second, 0},
			[]float64{10, 20, 30, 40}),
		Now:     now,
		Horizon: 30 * time.Second,
	})

	assert.False(t, ok)
}
