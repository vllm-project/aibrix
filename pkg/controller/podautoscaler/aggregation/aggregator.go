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

package aggregation

import (
	"math"
	"time"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// MetricAggregator processes and aggregates metrics over time windows
type MetricAggregator interface {
	// ProcessSnapshot adds new metrics to aggregation windows
	ProcessSnapshot(metricKey types.MetricKey, snapshot *types.MetricSnapshot, config metrics.MetricsConfig) error

	// GetAggregatedMetrics returns processed metrics for scaling decisions
	GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error)
}

// AggregationConfig is kept for backward compatibility with config extraction
// but is no longer used by the aggregator itself
type AggregationConfig struct {
	StableWindow time.Duration
	PanicWindow  time.Duration
	Window       time.Duration // For APA
	Granularity  time.Duration
}

// DefaultMetricAggregator is a single implementation for all strategies
// It always returns both stable and panic values - the algorithm layer decides which to use
// This is a stateless aggregator that delegates to MetricsClient
type DefaultMetricAggregator struct {
	client metrics.AggregatorMetricsClient
}

// NewMetricAggregator creates a metric aggregator (same for all strategies)
func NewMetricAggregator(client metrics.AggregatorMetricsClient) *DefaultMetricAggregator {
	return &DefaultMetricAggregator{
		client: client,
	}
}

func (a *DefaultMetricAggregator) ProcessSnapshot(metricKey types.MetricKey, snapshot *types.MetricSnapshot, config metrics.MetricsConfig) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}

	// Use the provided metricKey which has correct PaNamespace/PaName
	// Pass the config so that windows are initialized with user-configured values
	return a.client.UpdateMetrics(snapshot.Timestamp, metricKey, config, snapshot.Values...)
}

func (a *DefaultMetricAggregator) GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error) {
	// Always get stable value (all strategies use this)
	stableVal, panicVal, err := a.client.GetMetricValue(key, now)
	if err != nil {
		return nil, err
	}

	// Return both values - algorithm layer decides which to use
	// For APA: stable == panic (both point to the same stable window)
	// For KPA: stable and panic are different windows
	return &types.AggregatedMetrics{
		MetricKey:   key,
		StableValue: stableVal,
		PanicValue:  panicVal,
		LastUpdated: now,
	}, nil
}

// calculateTrend calculates the trend between stable and panic values with smoothing
func calculateTrend(stable, panic float64) float64 {
	if stable <= 0 {
		return 0
	}

	// Calculate raw trend
	rawTrend := (panic - stable) / stable

	// Apply exponential smoothing to reduce noise
	// Use a smoothing factor (alpha) of 0.3 for gradual changes
	const alpha = 0.3
	smoothedTrend := alpha * rawTrend

	// Cap the trend to reasonable bounds (-1 to 2)
	if smoothedTrend < -1 {
		return -1
	}
	if smoothedTrend > 2 {
		return 2
	}

	return smoothedTrend
}

// calculateKPAConfidence calculates confidence based on stable and panic values
func calculateKPAConfidence(stable, panic float64) float64 {
	// Base confidence
	confidence := 0.5

	// If we have both stable and panic values, confidence is higher
	if stable > 0 && panic > 0 {
		confidence += 0.3
	}

	// If values are close, we have higher confidence
	if stable > 0 && panic > 0 {
		diff := math.Abs(stable - panic)
		ratio := diff / stable
		if ratio < 0.1 { // Less than 10% difference
			confidence += 0.2
		}
	}

	// Ensure confidence is between 0 and 1
	if confidence < 0 {
		return 0
	}
	if confidence > 1 {
		return 1
	}

	return confidence
}
