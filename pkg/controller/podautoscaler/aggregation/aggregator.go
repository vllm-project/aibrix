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
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	types2 "k8s.io/apimachinery/pkg/types"
)

// MetricAggregator processes and aggregates metrics over time windows
type MetricAggregator interface {
	// ProcessSnapshot adds new metrics to aggregation windows
	ProcessSnapshot(snapshot *types.MetricSnapshot) error

	// GetAggregatedMetrics returns processed metrics for scaling decisions
	GetAggregatedMetrics(key metrics.NamespaceNameMetric, now time.Time) (*AggregatedMetrics, error)

	// UpdateConfiguration changes aggregation parameters
	UpdateConfiguration(config AggregationConfig) error
}

// AggregatedMetrics contains processed metric values
type AggregatedMetrics struct {
	MetricKey    metrics.NamespaceNameMetric
	CurrentValue float64
	StableValue  float64 // KPA only
	PanicValue   float64 // KPA only
	Trend        float64
	Confidence   float64
	LastUpdated  time.Time
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
	*metrics.KPAMetricsClient // Embed existing client to reuse logic
	config                    AggregationConfig
}

func NewKPAMetricAggregator(fetcher metrics.MetricFetcher, config AggregationConfig) *KPAMetricAggregator {
	// Reuse existing KPAMetricsClient constructor
	client := metrics.NewKPAMetricsClient(fetcher, config.StableWindow, config.PanicWindow)

	return &KPAMetricAggregator{
		KPAMetricsClient: client,
		config:           config,
	}
}

func (a *KPAMetricAggregator) ProcessSnapshot(snapshot *types.MetricSnapshot) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}

	// Convert to NamespaceNameMetric for compatibility with existing logic
	metricKey := metrics.NamespaceNameMetric{
		NamespacedName: types2.NamespacedName{
			Namespace: snapshot.Namespace,
			Name:      snapshot.TargetName,
		},
		MetricName: snapshot.MetricName,
	}

	// Reuse existing UpdateMetrics logic
	return a.UpdateMetrics(snapshot.Timestamp, metricKey, snapshot.Values...)
}

func (a *KPAMetricAggregator) GetAggregatedMetrics(key metrics.NamespaceNameMetric, now time.Time) (*AggregatedMetrics, error) {
	// Reuse existing StableAndPanicMetrics logic
	stable, panic, err := a.StableAndPanicMetrics(key, now)
	if err != nil {
		return nil, err
	}

	// Calculate simple trend and confidence
	trend := 0.0
	confidence := 1.0
	if stable > 0 {
		trend = (panic - stable) / stable
		confidence = 0.8 // Default confidence
	}

	return &AggregatedMetrics{
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
	*metrics.APAMetricsClient // Embed existing client
	config                    AggregationConfig
}

func NewAPAMetricAggregator(fetcher metrics.MetricFetcher, config AggregationConfig) *APAMetricAggregator {
	client := metrics.NewAPAMetricsClient(fetcher, config.Window)

	return &APAMetricAggregator{
		APAMetricsClient: client,
		config:           config,
	}
}

func (a *APAMetricAggregator) ProcessSnapshot(snapshot *types.MetricSnapshot) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}

	// Convert to NamespaceNameMetric for compatibility with existing logic
	metricKey := metrics.NamespaceNameMetric{
		NamespacedName: types2.NamespacedName{
			Namespace: snapshot.Namespace,
			Name:      snapshot.TargetName,
		},
		MetricName: snapshot.MetricName,
	}

	// Reuse existing UpdateMetrics logic
	return a.UpdateMetrics(snapshot.Timestamp, metricKey, snapshot.Values...)
}

func (a *APAMetricAggregator) GetAggregatedMetrics(key metrics.NamespaceNameMetric, now time.Time) (*AggregatedMetrics, error) {
	// Reuse existing GetMetricValue logic
	value, err := a.GetMetricValue(key, now)
	if err != nil {
		return nil, err
	}

	return &AggregatedMetrics{
		MetricKey:    key,
		CurrentValue: value,
		StableValue:  value,
		PanicValue:   value,
		Trend:        0.0,
		Confidence:   1.0,
		LastUpdated:  now,
	}, nil
}

func (a *APAMetricAggregator) UpdateConfiguration(config AggregationConfig) error {
	a.config = config
	return nil
}

// NewMetricAggregator creates aggregators based on scaling strategy
func NewMetricAggregator(strategy autoscalingv1alpha1.ScalingStrategyType, fetcher metrics.MetricFetcher, config AggregationConfig) MetricAggregator {
	switch strategy {
	case autoscalingv1alpha1.KPA:
		return NewKPAMetricAggregator(fetcher, config)
	case autoscalingv1alpha1.APA:
		return NewAPAMetricAggregator(fetcher, config)
	default:
		return NewKPAMetricAggregator(fetcher, config) // Default to KPA
	}
}
