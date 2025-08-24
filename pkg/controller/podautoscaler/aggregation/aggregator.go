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

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// MetricAggregator processes and aggregates metrics over time windows
type MetricAggregator interface {
	// ProcessSnapshot adds new metrics to aggregation windows
	ProcessSnapshot(snapshot *types.MetricSnapshot) error

	// GetAggregatedMetrics returns processed metrics for scaling decisions
	GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error)

	// UpdateConfiguration changes aggregation parameters
	UpdateConfiguration(config AggregationConfig) error
}

// AggregationConfig defines aggregation parameters
type AggregationConfig struct {
	StableWindow time.Duration
	PanicWindow  time.Duration
	Window       time.Duration // For APA
	Granularity  time.Duration
}

// KPAMetricAggregator extends existing KPAMetricsClient
type KPAMetricAggregator struct {
	client metrics.AggregatorMetricsClient // Use interface from metrics package
	config AggregationConfig
}

func NewKPAMetricAggregator(client metrics.AggregatorMetricsClient, config AggregationConfig) *KPAMetricAggregator {
	return &KPAMetricAggregator{
		client: client,
		config: config,
	}
}

func (a *KPAMetricAggregator) ProcessSnapshot(snapshot *types.MetricSnapshot) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}

	// Create metric key for compatibility
	metricKey := types.MetricKey{
		Namespace:  snapshot.Namespace,
		Name:       snapshot.TargetName,
		MetricName: snapshot.MetricName,
	}

	// Reuse existing UpdateMetrics logic
	return a.client.UpdateMetrics(snapshot.Timestamp, metricKey, snapshot.Values...)
}

func (a *KPAMetricAggregator) GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error) {
	// Reuse existing StableAndPanicMetrics logic
	stable, panic, err := a.client.StableAndPanicMetrics(key, now)
	if err != nil {
		return nil, err
	}

	// Get enhanced trend analysis from historical data
	direction, _, trendConfidence := a.client.GetTrendAnalysis(key, now)

	// Use sophisticated trend calculation if available, fallback to simple
	trend := direction
	if trendConfidence < 0.3 {
		trend = calculateTrend(stable, panic) // Fallback to simple calculation
	}

	// Get enhanced confidence from pod-aware calculation
	// Note: We don't have pod count here, but will be enhanced later in the pipeline
	confidence := calculateKPAConfidence(stable, panic)

	return &types.AggregatedMetrics{
		MetricKey:    key,
		StableValue:  stable,
		PanicValue:   panic,
		CurrentValue: panic, // Use panic value as current
		Trend:        trend,
		Confidence:   confidence,
		LastUpdated:  now,
	}, nil
}

func (a *KPAMetricAggregator) UpdateConfiguration(config AggregationConfig) error {
	a.config = config
	return nil
}

// APAMetricAggregator extends existing APAMetricsClient
type APAMetricAggregator struct {
	client metrics.AggregatorMetricsClient // Use interface from metrics package
	config AggregationConfig
}

func NewAPAMetricAggregator(client metrics.AggregatorMetricsClient, config AggregationConfig) *APAMetricAggregator {
	return &APAMetricAggregator{
		client: client,
		config: config,
	}
}

func (a *APAMetricAggregator) ProcessSnapshot(snapshot *types.MetricSnapshot) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}

	// Create metric key for compatibility
	metricKey := types.MetricKey{
		Namespace:  snapshot.Namespace,
		Name:       snapshot.TargetName,
		MetricName: snapshot.MetricName,
	}

	// Reuse existing UpdateMetrics logic
	return a.client.UpdateMetrics(snapshot.Timestamp, metricKey, snapshot.Values...)
}

func (a *APAMetricAggregator) GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error) {
	// Reuse existing GetMetricValue logic
	value, err := a.client.GetMetricValue(key, now)
	if err != nil {
		return nil, err
	}

	// For APA, use simple trend and confidence
	trend := 0.0
	confidence := 0.8 // Default confidence for APA

	return &types.AggregatedMetrics{
		MetricKey:    key,
		CurrentValue: value,
		StableValue:  value,
		PanicValue:   value,
		Trend:        trend,
		Confidence:   confidence,
		LastUpdated:  now,
	}, nil
}

func (a *APAMetricAggregator) UpdateConfiguration(config AggregationConfig) error {
	a.config = config
	return nil
}

// NewMetricAggregator creates aggregators based on scaling strategy
func NewMetricAggregator(strategy autoscalingv1alpha1.ScalingStrategyType, client metrics.AggregatorMetricsClient, config AggregationConfig) MetricAggregator {
	switch strategy {
	case autoscalingv1alpha1.KPA:
		return NewKPAMetricAggregator(client, config)
	case autoscalingv1alpha1.APA:
		return NewAPAMetricAggregator(client, config)
	default:
		return NewKPAMetricAggregator(client, config) // Default to KPA
	}
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
