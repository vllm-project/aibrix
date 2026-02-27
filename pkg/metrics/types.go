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
	"math"
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
	PromQL     QueryType = "PromQL"     // PromQL represents a Prometheus query language expression.
	QueryLabel QueryType = "QueryLabel" // Query Label value from raw metrics.
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

// MetricScope defines the scope of a metric (e.g., model or pod or podmodel).
type MetricScope string

const (
	ModelMetricScope    MetricScope = "Model"
	PodMetricScope      MetricScope = "Pod"
	PodModelMetricScope MetricScope = "PodModel" // model in pod
)

// Metric defines a unique metric with metadata.
type Metric struct {
	MetricSource             MetricSource
	MetricType               MetricType
	PromQL                   string            // Optional: Only applicable for PromQL-based metrics
	LabelKey                 string            // Optional: Only applicable for QueryLabel-based metrics
	EngineMetricsNameMapping map[string]string // Optional: Mapping from engine type to raw metric name.
	Description              string
	MetricScope              MetricScope
}

// MetricValue is the interface for all metric values.
type MetricValue interface {
	GetSimpleValue() float64
	GetHistogramValue() *HistogramMetricValue
	GetPrometheusResult() *model.Value
	GetLabelValues() map[string]string
}

var _ MetricValue = (*SimpleMetricValue)(nil)
var _ MetricValue = (*HistogramMetricValue)(nil)
var _ MetricValue = (*PrometheusMetricValue)(nil)
var _ MetricValue = (*LabelValueMetricValue)(nil)

// SimpleMetricValue represents simple metrics (e.g., gauge or counter).
type SimpleMetricValue struct {
	Value  float64
	Labels map[string]string // Optional: Additional labels for the metric.
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

func (s *SimpleMetricValue) GetLabelValues() map[string]string {
	return s.Labels
}

// HistogramMetricValue represents a detailed histogram metric.
type HistogramMetricValue struct {
	Sum     float64
	Count   float64
	Buckets map[string]float64 // e.g., {"0.1": 5, "0.5": 3, "1.0": 2}
	Labels  map[string]string  // Optional: Additional labels for the histogram.
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
	if percentile <= 0 || percentile > 100 {
		return 0, fmt.Errorf("percentile must be between 0 and 100, got: %f", percentile)
	}

	type bucket struct {
		upper float64
		count float64
		isInf bool
	}

	var buckets []bucket
	var maxCount float64
	for k, v := range h.Buckets {
		if k == "+Inf" {
			buckets = append(buckets, bucket{upper: math.Inf(1), count: v, isInf: true})
		} else {
			b, err := strconv.ParseFloat(k, 64)
			if err != nil {
				return 0, err
			}
			buckets = append(buckets, bucket{upper: b, count: v})
		}
		if v > maxCount {
			maxCount = v
		}
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].upper < buckets[j].upper
	})

	// Use bucket-derived total if h.Count is missing or smaller than buckets.
	totalCount := h.Count
	if totalCount <= 0 || totalCount < maxCount {
		totalCount = maxCount
	}

	target := percentile / 100.0 * totalCount

	var prevCount float64
	var prevUpper float64

	for _, b := range buckets {
		if b.count >= target {
			if b.isInf {
				return prevUpper, nil
			}
			bucketCount := b.count - prevCount
			if bucketCount == 0 {
				return b.upper, nil
			}
			fraction := (target - prevCount) / bucketCount
			return prevUpper + fraction*(b.upper-prevUpper), nil
		}
		prevCount = b.count
		prevUpper = b.upper
	}

	return 0, fmt.Errorf("percentile not found")
}

func (h *HistogramMetricValue) GetLabelValues() map[string]string {
	return h.Labels
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

func (s *PrometheusMetricValue) GetLabelValues() map[string]string {
	return map[string]string{}
}

// PrometheusMetricValue represents Prometheus query results.
type LabelValueMetricValue struct {
	Value string
}

func (l *LabelValueMetricValue) GetSimpleValue() float64 {
	return 0.0
}

func (l *LabelValueMetricValue) GetHistogramValue() *HistogramMetricValue {
	return nil
}

func (l *LabelValueMetricValue) GetPrometheusResult() *model.Value {
	return nil
}

func (l *LabelValueMetricValue) GetLabelValues() map[string]string {
	return map[string]string{"value": l.Value}
}

func ExtractNumericFromPromResult(r *model.Value) (float64, error) {
	if r == nil {
		return 0, fmt.Errorf("nil Prometheus result")
	}
	switch (*r).Type() {
	case model.ValVector:
		vec := (*r).(model.Vector)
		if len(vec) == 0 {
			return 0, fmt.Errorf("empty vector")
		}
		return float64(vec[0].Value), nil
	case model.ValScalar:
		scalar := (*r).(*model.Scalar)
		return float64(scalar.Value), nil
	case model.ValMatrix:
		matrix := (*r).(model.Matrix)
		if len(matrix) == 0 || len(matrix[0].Values) == 0 {
			return 0, fmt.Errorf("empty matrix")
		}
		return float64(matrix[0].Values[0].Value), nil
	default:
		return 0, fmt.Errorf("unsupported Prometheus result type: %s", (*r).Type().String())
	}
}
