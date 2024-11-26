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

import "github.com/prometheus/common/model"

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
	Value            float64          // For simple metrics (e.g., gauge or counter)
	Histogram        *HistogramMetric // For histogram metrics
	PrometheusResult *model.Value     // For prometheus metrics
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

const (
	NumRequestsRunning              = "num_requests_running"
	NumRequestsWaiting              = "num_requests_waiting"
	NumRequestsSwapped              = "num_requests_swapped"
	AvgPromptThroughputToksPerS     = "avg_prompt_throughput_toks_per_s"
	AvgGenerationThroughputToksPerS = "avg_generation_throughput_toks_per_s"
	IterationTokensTotal            = "iteration_tokens_total"
	TimeToFirstTokenSeconds         = "time_to_first_token_seconds"
	TimePerOutputTokenSeconds       = "time_per_output_token_seconds"
	E2ERequestLatencySeconds        = "e2e_request_latency_seconds"
	RequestQueueTimeSeconds         = "request_queue_time_seconds"
	RequestInferenceTimeSeconds     = "request_inference_time_seconds"
	RequestDecodeTimeSeconds        = "request_decode_time_seconds"
	RequestPrefillTimeSeconds       = "request_prefill_time_seconds"
	P95TTFT5m                       = "p95_ttft_5m"
)

var (
	Metrics = map[string]Metric{
		// Pods metrics
		// Counter metrics
		NumRequestsRunning:              {PodMetrics, Counter, "", "Number of running requests"},
		NumRequestsWaiting:              {PodMetrics, Counter, "", "Number of waiting requests"},
		NumRequestsSwapped:              {PodMetrics, Counter, "", "Number of swapped requests"},
		AvgPromptThroughputToksPerS:     {PodMetrics, Gauge, "", "Average prompt throughput in tokens per second"},
		AvgGenerationThroughputToksPerS: {PodMetrics, Gauge, "", "Average generation throughput in tokens per second"},
		// Histogram metrics
		IterationTokensTotal:        {PodMetrics, Histogram, "", "Total iteration tokens"},
		TimeToFirstTokenSeconds:     {PodMetrics, Histogram, "", "Time to first token in seconds"},
		TimePerOutputTokenSeconds:   {PodMetrics, Histogram, "", "Time per output token in seconds"},
		E2ERequestLatencySeconds:    {PodMetrics, Histogram, "", "End-to-end request latency in seconds"},
		RequestQueueTimeSeconds:     {PodMetrics, Histogram, "", "Request queue time in seconds"},
		RequestInferenceTimeSeconds: {PodMetrics, Histogram, "", "Request inference time in seconds"},
		RequestDecodeTimeSeconds:    {PodMetrics, Histogram, "", "Request decode time in seconds"},
		RequestPrefillTimeSeconds:   {PodMetrics, Histogram, "", "Request prefill time in seconds"},

		// Aggregated Prometheus metrics
		// Please use label_key=${label_key} for dynamic injection
		P95TTFT5m: {PrometheusEndpoint, PromQL, `histogram_quantile(0.95, sum by(le) (rate(vllm:time_to_first_token_seconds_bucket{model_name="${model_name}", job="pods"}[5m])))`, "95th ttft in last 5 mins"},
	}
)
