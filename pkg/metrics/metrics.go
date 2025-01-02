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
	NumRequestsRunning                   = "num_requests_running"
	NumRequestsWaiting                   = "num_requests_waiting"
	NumRequestsSwapped                   = "num_requests_swapped"
	AvgPromptThroughputToksPerS          = "avg_prompt_throughput_toks_per_s"
	AvgGenerationThroughputToksPerS      = "avg_generation_throughput_toks_per_s"
	IterationTokensTotal                 = "iteration_tokens_total"
	TimeToFirstTokenSeconds              = "time_to_first_token_seconds"
	TimePerOutputTokenSeconds            = "time_per_output_token_seconds"
	E2ERequestLatencySeconds             = "e2e_request_latency_seconds"
	RequestQueueTimeSeconds              = "request_queue_time_seconds"
	RequestInferenceTimeSeconds          = "request_inference_time_seconds"
	RequestDecodeTimeSeconds             = "request_decode_time_seconds"
	RequestPrefillTimeSeconds            = "request_prefill_time_seconds"
	P95TTFT5m                            = "p95_ttft_5m"
	P95TTFT5mPod                         = "p95_ttft_5m_pod"
	AvgTTFT5mPod                         = "avg_ttft_5m_pod"
	P95TPOT5mPod                         = "p95_tpot_5m_pod"
	AvgTPOT5mPod                         = "avg_tpot_pod_5m"
	AvgPromptToksPerReq                  = "avg_prompt_toks_per_req"
	AvgGenerationToksPerReq              = "avg_generation_toks_per_req"
	GPUCacheUsagePerc                    = "gpu_cache_usage_perc"
	CPUCacheUsagePerc                    = "cpu_cache_usage_perc"
	AvgE2ELatencyPod                     = "avg_e2e_latency_pod"
	AvgRequestsPerMinPod                 = "avg_requests_per_min_pod"
	AvgPromptThroughputToksPerMinPod     = "avg_prompt_throughput_toks_per_min_pod"
	AvgGenerationThroughputToksPerMinPod = "avg_generation_throughput_toks_per_min_pod"
	MaxLora                              = "max_lora"
	WaitingLoraAdapters                  = "waiting_lora_adapters"
	RunningLoraAdapters                  = "running_lora_adapters"
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
			PromQL:      `histogram_quantile(0.95, sum by(le) (rate(vllm:time_to_first_token_seconds_bucket{instance="${instance}", model_name="${model_name}", job="pods"}[5m])))`,
			Description: "95th ttft in last 5 mins",
		},
		P95TTFT5mPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `histogram_quantile(0.95, sum by(le) (rate(vllm:time_to_first_token_seconds_bucket{instance="${instance}", job="pods"}[5m])))`,
			Description: "95th ttft in last 5 mins",
		},
		AvgTTFT5mPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:time_to_first_token_seconds_sum{instance="${instance}", job="pods"}[5m]) / increase(vllm:time_to_first_token_seconds_count{instance="${instance}", job="pods"}[5m])`,
			Description: "Average ttft in last 5 mins",
		},
		P95TPOT5mPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `histogram_quantile(0.95, sum by(le) (rate(vllm:time_per_output_token_seconds_bucket{instance="${instance}", job="pods"}[5m])))`,
			Description: "95th tpot in last 5 mins",
		},
		AvgTPOT5mPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:time_per_output_token_seconds_sum{instance="${instance}", job="pods"}[5m]) / increase(vllm:time_per_output_token_seconds_sum{instance="${instance}", job="pods"}[5m])`,
			Description: "Average tpot in last 5 mins",
		},
		AvgPromptToksPerReq: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:request_prompt_tokens_sum{instance="${instance}", model_name="${model_name}", job="pods"}[1d]) / increase(vllm:request_prompt_tokens_count{instance="${instance}", model_name="${model_name}", job="pods"}[1d])`,
			Description: "Average prompt tokens per request in last day",
		},
		AvgGenerationToksPerReq: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:request_generation_tokens_sum{instance="${instance}", model_name="${model_name}", job="pods"}[1d]) / increase(vllm:request_generation_tokens_count{instance="${instance}", model_name="${model_name}", job="pods"}[1d])`,
			Description: "Average generation tokens per request in last day",
		},
		GPUCacheUsagePerc: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "GPU cache usage percentage",
		},
		CPUCacheUsagePerc: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "CPU cache usage percentage",
		},
		AvgE2ELatencyPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:e2e_request_latency_seconds_sum{instance="${instance}", job="pods"}[5m]) / increase(vllm:e2e_request_latency_seconds_count{instance="${instance}", job="pods"}[5m])`,
			Description: "Average End-to-end latency in last 5 mins",
		},
		AvgRequestsPerMinPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:request_success_total{instance="${instance}", job="pods"}[5m]) / 5`,
			Description: "Average requests throughput per minute in last 5 mins",
		},
		AvgPromptThroughputToksPerMinPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:prompt_tokens_total{instance="${instance}", job="pods"}[5m]) / 5`,
			Description: "Average prompt throughput in tokens per minute in last 5 mins",
		},
		AvgGenerationThroughputToksPerMinPod: {
			MetricScope:  PodMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			PromQL:      `increase(vllm:generation_tokens_total{instance="${instance}", job="pods"}[5m]) / 5`,
			Description: "Average generation throughput in tokens per minute in last 5 mins",
		},
		MaxLora: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Query: QueryLabel,
			},
			RawMetricName: "lora_requests_info",
			Description:   "Max count of Lora Adapters",
		},
		RunningLoraAdapters: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Query: QueryLabel,
			},
			RawMetricName: "lora_requests_info",
			Description:   "Count of running Lora Adapters",
		},
		WaitingLoraAdapters: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Query: QueryLabel,
			},
			RawMetricName: "lora_requests_info",
			Description:   "Count of waiting Lora Adapters",
		},
	}
)
