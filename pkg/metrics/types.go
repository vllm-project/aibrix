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

package metrics

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/prometheus/common/model"
)

// MetricSource defines the metric source
type MetricSource string

const (
	// PrometheusEndpoint indicates metrics are queried from a remote Prometheus server.
	// This source allows querying both raw and aggregated metrics, leveraging PromQL for advanced analytics.
	PrometheusEndpoint MetricSource = "PrometheusEndpoint"
	// PodRawMetrics indicates metrics are collected directly from the metricPort of a Pod.
	PodRawMetrics MetricSource = "PodRawMetrics"
)

// RawMetricType defines the type of raw metrics (e.g., collected directly from a source).
type RawMetricType string

const (
	Gauge     RawMetricType = "Gauge"     // Gauge represents a snapshot value.
	Counter   RawMetricType = "Counter"   // Counter represents a cumulative value.
	Histogram RawMetricType = "Histogram" // Histogram represents a distribution of values.
)

// QueryType defines the type of metric query, such as PromQL.
type QueryType string

const (
	PromQL QueryType = "PromQL" // PromQL represents a Prometheus query language expression.
)

// MetricType defines the type of a metric, including raw metrics and queries.
type MetricType struct {
	Raw   RawMetricType // Optional: Represents the type of raw metric.
	Query QueryType     // Optional: Represents the query type for derived metrics.
}

func (m MetricType) IsRawMetric() bool {
	return m.Raw != ""
}

func (m MetricType) IsQuery() bool {
	return m.Query != ""
}

// Metric defines a unique metric with metadata.
type Metric struct {
	MetricSource MetricSource
	MetricType   MetricType
	PromQL       string // Optional: Only applicable for PromQL-based metrics
	Description  string
}

// MetricValue is the interface for all metric values.
type MetricValue interface {
	GetSimpleValue() float64
	GetHistogramValue() *HistogramMetricValue
	GetPrometheusResult() *model.Value
}

var _ MetricValue = (*SimpleMetricValue)(nil)
var _ MetricValue = (*HistogramMetricValue)(nil)
var _ MetricValue = (*PrometheusMetricValue)(nil)

// SimpleMetricValue represents simple metrics (e.g., gauge or counter).
type SimpleMetricValue struct {
	Value float64
}

func (s *SimpleMetricValue) GetSimpleValue() float64 {
	return s.Value
}

func (s *SimpleMetricValue) GetHistogramValue() *HistogramMetricValue {
	return nil
}

func (s *SimpleMetricValue) GetPrometheusResult() *model.Value {
	return nil
}

// HistogramMetricValue represents a detailed histogram metric.
type HistogramMetricValue struct {
	Sum     float64
	Count   float64
	Buckets map[string]float64 // e.g., {"0.1": 5, "0.5": 3, "1.0": 2}
}

func (h *HistogramMetricValue) GetSimpleValue() float64 {
	return 0.0
}

func (h *HistogramMetricValue) GetHistogramValue() *HistogramMetricValue {
	return h
}

func (h *HistogramMetricValue) GetPrometheusResult() *model.Value {
	return nil
}

func (h *HistogramMetricValue) GetValue() interface{} {
	return h // Return the entire histogram structure
}

// GetSum returns the sum of the histogram values.
func (h *HistogramMetricValue) GetSum() float64 {
	return h.Sum
}

// GetCount returns the total count of values in the histogram.
func (h *HistogramMetricValue) GetCount() float64 {
	return h.Count
}

// GetBucketValue returns the count for a specific bucket.
func (h *HistogramMetricValue) GetBucketValue(bucket string) (float64, bool) {
	value, exists := h.Buckets[bucket]
	return value, exists
}

// GetMean returns the mean value of the histogram (Sum / Count).
func (h *HistogramMetricValue) GetMean() float64 {
	if h.Count == 0 {
		return 0
	}
	return h.Sum / h.Count
}

func (h *HistogramMetricValue) GetPercentile(percentile float64) (float64, error) {
	if percentile < 0 || percentile > 100 {
		return 0, fmt.Errorf("percentile must be between 0 and 100, got: %f", percentile)
	}

	// Collect and sort bucket boundaries, treating +Inf specially
	type bucket struct {
		bound float64
		count float64
		isInf bool
	}
	var buckets []bucket
	for bound, count := range h.Buckets {
		if bound == "+Inf" {
			buckets = append(buckets, bucket{bound: 0, count: count, isInf: true})
		} else {
			parsedBound, err := strconv.ParseFloat(bound, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid bucket boundary: %s", bound)
			}
			buckets = append(buckets, bucket{bound: parsedBound, count: count, isInf: false})
		}
	}

	// Sort buckets by boundary, placing +Inf last
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].isInf {
			return false
		}
		if buckets[j].isInf {
			return true
		}
		return buckets[i].bound < buckets[j].bound
	})

	var lastBound float64
	// Calculate cumulative distribution and find the desired percentile
	for _, b := range buckets {
		if b.count/h.Count*100 >= percentile {
			if b.isInf {
				// instead of return +Inf for infinite bucket, let's return last bucket value
				return lastBound, nil // Return
			}
			return b.bound, nil
		}
		lastBound = b.bound
	}

	return 0, nil
}

// PrometheusMetricValue represents Prometheus query results.
type PrometheusMetricValue struct {
	Result *model.Value
}

func (p *PrometheusMetricValue) GetSimpleValue() float64 {
	return 0.0
}

func (p *PrometheusMetricValue) GetHistogramValue() *HistogramMetricValue {
	return nil
}

func (p *PrometheusMetricValue) GetPrometheusResult() *model.Value {
	return p.Result
}
