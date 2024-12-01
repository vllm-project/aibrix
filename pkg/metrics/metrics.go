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
	// Metrics defines all available metrics, including raw and query-based metrics.
	Metrics = map[string]Metric{
		// Counter metrics
		NumRequestsRunning: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Number of running requests",
		},
		NumRequestsWaiting: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Number of waiting requests",
		},
		NumRequestsSwapped: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Number of swapped requests",
		},
		// Gauge metrics
		AvgPromptThroughputToksPerS: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Average prompt throughput in tokens per second",
		},
		AvgGenerationThroughputToksPerS: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Average generation throughput in tokens per second",
		},
		// Histogram metrics
		IterationTokensTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Total iteration tokens",
		},
		TimeToFirstTokenSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Time to first token in seconds",
		},
		TimePerOutputTokenSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Time per output token in seconds",
		},
		E2ERequestLatencySeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "End-to-end request latency in seconds",
		},
		RequestQueueTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Request queue time in seconds",
		},
		RequestInferenceTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Request inference time in seconds",
		},
		RequestDecodeTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Request decode time in seconds",
		},
		RequestPrefillTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "Request prefill time in seconds",
		},
		// Query-based metrics
		P95TTFT5m: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `histogram_quantile(0.95, sum by(le) (rate(vllm:time_to_first_token_seconds_bucket{model_name="${model_name}", job="pods"}[5m])))`,
			Description: "95th ttft in last 5 mins",
		},
	}
)
