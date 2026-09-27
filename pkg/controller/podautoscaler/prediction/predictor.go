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
// The package works on metric samples only: it reports the observed level and
// the level projected for the configured horizon. Turning a projected value
// back into a replica count stays with the scaling pipelines, so a projection
// always follows the same formula as the reactive path of the strategy that
// uses it.
package prediction

import (
	"math"
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

	// MaxGrowthRatio caps how far the projection may rise above the fitted
	// level at now, so one noisy spike cannot ask for an unbounded replica
	// count.
	MaxGrowthRatio = 4.0
)

// Response is a completed projection over one observation window.
type Response struct {
	// ObservedValue is the mean sample value inside the observation window.
	ObservedValue float64
	// PredictedValue is the fitted line evaluated at now + horizon.
	PredictedValue float64
	// Slope is the fitted change of the metric value per second.
	Slope float64
	// DataPoints is the number of samples the fit used.
	DataPoints int
}

// Project fits a straight line to the samples inside (now-window, now] and
// evaluates it at now + horizon.
//
// The fitted line is anchored at the centroid of the samples, so the value at
// a point in time is the mean plus the slope times the distance from the
// centroid. A horizon shorter than half the window therefore still projects a
// point after now, and a full window projects the configured lead time past
// now instead of half a window behind it.
//
// Project reports false when the window is not positive, when fewer than
// MinDataPoints usable samples fall inside it, when the samples span less than
// MinCoverage of the window, when the fit has no time spread to work with, or
// when the fit does not produce a finite value. Otherwise the projection is
// clamped at zero and at MaxGrowthRatio times the fitted level at now.
func Project(series []types.MetricPoint, now time.Time, window, horizon time.Duration) (Response, bool) {
	if window <= 0 {
		return Response{}, false
	}
	if horizon <= 0 {
		horizon = DefaultHorizon
	}

	samples := samplesInWindow(series, now, window)
	if len(samples) < MinDataPoints {
		return Response{}, false
	}
	if samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp) <
		time.Duration(MinCoverage*float64(window)) {
		return Response{}, false
	}

	slope, observed, meanOffset, ok := leastSquares(samples)
	if !ok {
		return Response{}, false
	}

	// Evaluate the fitted line at now and at now+horizon, both measured from
	// the centroid of the samples the fit was built from.
	nowOffset := now.Sub(samples[0].Timestamp).Seconds()
	levelAtNow := observed + slope*(nowOffset-meanOffset)
	predicted := levelAtNow + slope*horizon.Seconds()
	if math.IsNaN(predicted) || math.IsInf(predicted, 0) {
		return Response{}, false
	}
	if predicted < 0 {
		predicted = 0
	}
	if levelAtNow > 0 && predicted > levelAtNow*MaxGrowthRatio {
		predicted = levelAtNow * MaxGrowthRatio
	}

	return Response{
		ObservedValue:  observed,
		PredictedValue: predicted,
		Slope:          slope,
		DataPoints:     len(samples),
	}, true
}

// samplesInWindow returns the finite samples inside (now-window, now], oldest
// first. Non-finite samples are dropped so one bad scrape cannot poison the
// fit.
func samplesInWindow(series []types.MetricPoint, now time.Time, window time.Duration) []types.MetricPoint {
	cutoff := now.Add(-window)
	samples := make([]types.MetricPoint, 0, len(series))
	for _, point := range series {
		if math.IsNaN(point.Value) || math.IsInf(point.Value, 0) {
			continue
		}
		if point.Timestamp.After(cutoff) && !point.Timestamp.After(now) {
			samples = append(samples, point)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		return samples[i].Timestamp.Before(samples[j].Timestamp)
	})
	return samples
}

// leastSquares returns the slope per second, the mean value, the mean sample
// offset in seconds from the first sample, and whether the fit has a time
// spread to work with. The timestamp offsets are recomputed instead of
// buffered so the fit stays allocation free.
func leastSquares(samples []types.MetricPoint) (slope, mean, meanOffset float64, ok bool) {
	origin := samples[0].Timestamp
	var sumOffset, sumValue float64
	for _, sample := range samples {
		sumOffset += sample.Timestamp.Sub(origin).Seconds()
		sumValue += sample.Value
	}

	count := float64(len(samples))
	meanOffset = sumOffset / count
	mean = sumValue / count

	var covariance, variance float64
	for _, sample := range samples {
		delta := sample.Timestamp.Sub(origin).Seconds() - meanOffset
		covariance += delta * (sample.Value - mean)
		variance += delta * delta
	}
	if variance == 0 {
		return 0, 0, 0, false
	}
	return covariance / variance, mean, meanOffset, true
}
