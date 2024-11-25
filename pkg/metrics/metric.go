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
	PromQL    MetricType = "PromQL"
)

type MetricValue struct {
	Value     float64          // For simple metrics (e.g., gauge or counter)
	Histogram *HistogramMetric // For histogram metrics
}

// Metric defines a unique metrics
type Metric struct {
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

var (
	Metrics = map[string]Metric{
		// Counter metrics
		"num_requests_running":                 {PodMetrics, Counter, "", "Number of running requests"},
		"num_requests_waiting":                 {PodMetrics, Counter, "", "Number of waiting requests"},
		"num_requests_swapped":                 {PodMetrics, Counter, "", "Number of swapped requests"},
		"avg_prompt_throughput_toks_per_s":     {PodMetrics, Gauge, "", "Average prompt throughput in tokens per second"},
		"avg_generation_throughput_toks_per_s": {PodMetrics, Gauge, "", "Average generation throughput in tokens per second"},

		// Histogram metrics
		"iteration_tokens_total":         {PodMetrics, Histogram, "", "Total iteration tokens"},
		"time_to_first_token_seconds":    {PodMetrics, Histogram, "", "Time to first token in seconds"},
		"time_per_output_token_seconds":  {PodMetrics, Histogram, "", "Time per output token in seconds"},
		"e2e_request_latency_seconds":    {PodMetrics, Histogram, "", "End-to-end request latency in seconds"},
		"request_queue_time_seconds":     {PodMetrics, Histogram, "", "Request queue time in seconds"},
		"request_inference_time_seconds": {PodMetrics, Histogram, "", "Request inference time in seconds"},
		"request_decode_time_seconds":    {PodMetrics, Histogram, "", "Request decode time in seconds"},
		"request_prefill_time_seconds":   {PodMetrics, Histogram, "", "Request prefill time in seconds"},

		// Prometheus metrics
		// TODO: what's the result.Value here. depends on the query type. We need to build an abstraction then.
		"p95_ttft_5m": {PrometheusEndpoint, Gauge, `histogram_quantile(0.95, sum by(le) (rate(vllm:time_to_first_token_seconds_bucket{model_name="${model_name}", job="pods"}[5m])))`, "95th ttft in last 5 mins"},
	}
)
