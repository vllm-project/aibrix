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

package types

import (
	"math"
	"sync"
	"time"
)

// AggregatedMetrics contains processed metrics data
type AggregatedMetrics struct {
	MetricKey    MetricKey
	CurrentValue float64
	StableValue  float64
	PanicValue   float64
	Trend        float64
	Confidence   float64
	LastUpdated  time.Time
}

// WindowStats represents statistics about a time window
type WindowStats struct {
	DataPoints  int
	Mean        float64
	Variance    float64
	StdDev      float64
	Min         float64
	Max         float64
	LastUpdate  time.Time
	WindowStart time.Time
	WindowEnd   time.Time
}

// MetricPoint represents a single metric measurement
type MetricPoint struct {
	Timestamp time.Time
	Value     float64
}

// MetricHistory tracks historical metrics for trend analysis
type MetricHistory struct {
	mu      sync.RWMutex
	history []MetricPoint
	maxAge  time.Duration
}

// NewMetricHistory creates a new metric history tracker
func NewMetricHistory(maxAge time.Duration) *MetricHistory {
	return &MetricHistory{
		history: make([]MetricPoint, 0, 100),
		maxAge:  maxAge,
	}
}

// Add adds a new metric point to history
func (h *MetricHistory) Add(value float64, timestamp time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Add new point
	h.history = append(h.history, MetricPoint{
		Timestamp: timestamp,
		Value:     value,
	})

	// Clean old points
	cutoff := timestamp.Add(-h.maxAge)
	validStart := 0
	for i, point := range h.history {
		if point.Timestamp.After(cutoff) {
			validStart = i
			break
		}
	}
	if validStart > 0 {
		h.history = h.history[validStart:]
	}
}

// GetStats calculates statistics for the history
func (h *MetricHistory) GetStats(now time.Time) WindowStats {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.history) == 0 {
		return WindowStats{
			DataPoints:  0,
			LastUpdate:  now,
			WindowStart: now.Add(-h.maxAge),
			WindowEnd:   now,
		}
	}

	// Calculate statistics
	var sum, min, max float64
	min = math.MaxFloat64
	max = -math.MaxFloat64

	for _, point := range h.history {
		sum += point.Value
		if point.Value < min {
			min = point.Value
		}
		if point.Value > max {
			max = point.Value
		}
	}

	mean := sum / float64(len(h.history))

	// Calculate variance
	var varianceSum float64
	for _, point := range h.history {
		diff := point.Value - mean
		varianceSum += diff * diff
	}
	variance := varianceSum / float64(len(h.history))
	stdDev := math.Sqrt(variance)

	return WindowStats{
		DataPoints:  len(h.history),
		Mean:        mean,
		Variance:    variance,
		StdDev:      stdDev,
		Min:         min,
		Max:         max,
		LastUpdate:  now,
		WindowStart: h.history[0].Timestamp,
		WindowEnd:   h.history[len(h.history)-1].Timestamp,
	}
}

// TimeWindow manages a sliding window of metric values
type TimeWindow struct {
	mu          sync.RWMutex
	duration    time.Duration
	granularity time.Duration
	buckets     map[int64]float64
	values      []float64
}

// NewTimeWindow creates a new time window
func NewTimeWindow(duration, granularity time.Duration) *TimeWindow {
	return &TimeWindow{
		duration:    duration,
		granularity: granularity,
		buckets:     make(map[int64]float64),
		values:      make([]float64, 0),
	}
}

// Record adds a value to the time window
func (tw *TimeWindow) Record(timestamp time.Time, value float64) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	// Bucket the timestamp
	bucket := timestamp.UnixNano() / int64(tw.granularity)
	tw.buckets[bucket] = value

	// Clean old buckets
	cutoff := timestamp.Add(-tw.duration).UnixNano() / int64(tw.granularity)
	// TODO: improve performance to O(1)
	for b := range tw.buckets {
		if b < cutoff {
			delete(tw.buckets, b)
		}
	}

	// Update values slice for easy access
	tw.values = make([]float64, 0, len(tw.buckets))
	for _, v := range tw.buckets {
		tw.values = append(tw.values, v)
	}
}

// Size returns the number of data points in the window
func (tw *TimeWindow) Size() int {
	tw.mu.RLock()
	defer tw.mu.RUnlock()
	return len(tw.values)
}

// Avg returns the average value in the window
func (tw *TimeWindow) Avg() (float64, error) {
	tw.mu.RLock()
	defer tw.mu.RUnlock()

	if len(tw.values) == 0 {
		return 0, nil
	}

	var sum float64
	for _, v := range tw.values {
		sum += v
	}
	return sum / float64(len(tw.values)), nil
}

// Min returns the minimum value in the window
func (tw *TimeWindow) Min() (float64, error) {
	tw.mu.RLock()
	defer tw.mu.RUnlock()

	if len(tw.values) == 0 {
		return 0, nil
	}

	min := tw.values[0]
	for _, v := range tw.values[1:] {
		if v < min {
			min = v
		}
	}
	return min, nil
}

// Max returns the maximum value in the window
func (tw *TimeWindow) Max() (float64, error) {
	tw.mu.RLock()
	defer tw.mu.RUnlock()

	if len(tw.values) == 0 {
		return 0, nil
	}

	max := tw.values[0]
	for _, v := range tw.values[1:] {
		if v > max {
			max = v
		}
	}
	return max, nil
}

// String returns a string representation of the time window
func (tw *TimeWindow) String() string {
	return "TimeWindow"
}
