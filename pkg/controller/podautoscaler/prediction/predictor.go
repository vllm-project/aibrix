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

// Package prediction projects the observed metric trend forward in time.
//
// The package works on metric samples only: it reports the observed value and
// the value projected at the horizon. Turning a projected value back into a
// replica count stays with the scaling pipelines, so a projection always
// follows the same formula as the reactive path of the strategy that uses it.
package prediction

import (
	"sort"
	"time"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

const (
	// DefaultHorizon is used when a PodAutoscaler does not configure one.
	DefaultHorizon = 2 * time.Minute

	// MinDataPoints is the smallest sample count a projection may be built from.
	MinDataPoints = 3

	// MinCoverage is the fraction of the observation window the samples must
	// span before a projection is trusted. It stops a projection from being
	// extrapolated out of the few samples recorded right after startup.
	MinCoverage = 0.5

	// MaxGrowthRatio caps how far the projection may rise above the observed
	// value, so one noisy spike cannot ask for an unbounded replica count.
	MaxGrowthRatio = 4.0
)

// Response is a completed projection over one observation window.
type Response struct {
	// ObservedValue is the mean sample value inside the observation window.
	ObservedValue float64
	// PredictedValue is the projected value at the horizon.
	PredictedValue float64
	// Slope is the fitted change of the metric value per second.
	Slope float64
	// DataPoints is the number of samples the fit used.
	DataPoints int
	// Window is the observation window the fit used.
	Window time.Duration
	// Horizon is how far ahead PredictedValue is projected.
	Horizon time.Duration
}

// Request carries the samples and the time ranges of one projection.
type Request struct {
	// Series holds the recorded samples in any order.
	Series []types.MetricPoint
	// Now is the evaluation time. Samples after it are ignored.
	Now time.Time
	// Window is the observation window the fit is limited to.
	Window time.Duration
	// Horizon is how far ahead the trend is projected.
	Horizon time.Duration
}

// Predictor projects a metric series into the future.
type Predictor interface {
	// Predict returns the projection and whether it could be computed.
	Predict(Request) (Response, bool)
}

// Linear projects the least-squares slope of the observation window forward.
// It is stateless and safe for concurrent use.
type Linear struct {
	// MinDataPoints overrides the package default when set.
	MinDataPoints int
	// MinCoverage overrides the package default when set.
	MinCoverage float64
	// MaxGrowthRatio overrides the package default when set.
	MaxGrowthRatio float64
}

var _ Predictor = (*Linear)(nil)

// NewLinear returns a Linear predictor with the package defaults.
func NewLinear() *Linear {
	return &Linear{
		MinDataPoints:  MinDataPoints,
		MinCoverage:    MinCoverage,
		MaxGrowthRatio: MaxGrowthRatio,
	}
}

// Predict fits a straight line to the samples inside the observation window and
// evaluates it at the horizon.
func (l *Linear) Predict(request Request) (Response, bool) {
	minPoints := l.MinDataPoints
	if minPoints <= 0 {
		minPoints = MinDataPoints
	}
	minCoverage := l.MinCoverage
	if minCoverage <= 0 {
		minCoverage = MinCoverage
	}
	maxGrowthRatio := l.MaxGrowthRatio
	if maxGrowthRatio <= 0 {
		maxGrowthRatio = MaxGrowthRatio
	}

	horizon := request.Horizon
	if horizon <= 0 {
		horizon = DefaultHorizon
	}
	if request.Window <= 0 {
		return Response{}, false
	}

	samples := samplesInWindow(request.Series, request.Now, request.Window)
	if len(samples) < minPoints {
		return Response{}, false
	}
	if samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp) <
		time.Duration(minCoverage*float64(request.Window)) {
		return Response{}, false
	}

	slope, observed, ok := leastSquares(samples)
	if !ok {
		return Response{}, false
	}

	predicted := observed + slope*horizon.Seconds()
	if predicted < 0 {
		predicted = 0
	}
	if observed > 0 && predicted > observed*maxGrowthRatio {
		predicted = observed * maxGrowthRatio
	}

	return Response{
		ObservedValue:  observed,
		PredictedValue: predicted,
		Slope:          slope,
		DataPoints:     len(samples),
		Window:         request.Window,
		Horizon:        horizon,
	}, true
}

// samplesInWindow returns the samples inside (now-window, now], oldest first.
func samplesInWindow(series []types.MetricPoint, now time.Time, window time.Duration) []types.MetricPoint {
	cutoff := now.Add(-window)
	samples := make([]types.MetricPoint, 0, len(series))
	for _, point := range series {
		if point.Timestamp.After(cutoff) && !point.Timestamp.After(now) {
			samples = append(samples, point)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		return samples[i].Timestamp.Before(samples[j].Timestamp)
	})
	return samples
}

// leastSquares returns the slope per second, the mean value of the samples and
// whether the fit has a time spread to work with. The timestamp offsets are
// recomputed instead of buffered so the fit stays allocation free.
func leastSquares(samples []types.MetricPoint) (float64, float64, bool) {
	origin := samples[0].Timestamp
	var sumOffset, sumValue float64
	for _, sample := range samples {
		sumOffset += sample.Timestamp.Sub(origin).Seconds()
		sumValue += sample.Value
	}

	count := float64(len(samples))
	meanOffset := sumOffset / count
	meanValue := sumValue / count

	var covariance, variance float64
	for _, sample := range samples {
		delta := sample.Timestamp.Sub(origin).Seconds() - meanOffset
		covariance += delta * (sample.Value - meanValue)
		variance += delta * delta
	}
	if variance == 0 {
		return 0, 0, false
	}
	return covariance / variance, meanValue, true
}
