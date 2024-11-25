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

package cache

// MetricSource defines the metric source
type MetricSource string

const (
	PrometheusEndpoint MetricSource = "Prometheus"
	PodMetrics         MetricSource = "Pod"
)

// MetricType defines the prometheus metrics type
type MetricType string

const (
	Gauge     MetricType = "Gauge"
	Counter   MetricType = "Counter"
	Histogram MetricType = "Histogram"
)

type MetricValue struct {
	Value     float64          // For simple metrics (e.g., gauge or counter)
	Histogram *HistogramMetric // For histogram metrics
}

// Metric defines a unique metrics
type Metric struct {
	Name        string
	Source      MetricSource
	Type        MetricType
	PromQL      string
	Description string
}

type HistogramMetric struct {
	Sum     float64
	Count   float64
	Buckets map[string]float64
}

type MetricSubscriber interface {
	SubscribedMetrics() []string
}
