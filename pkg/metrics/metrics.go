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
	NumRequestsRunning          = "num_requests_running"
	NumRequestsWaiting          = "num_requests_waiting"
	EngineSleepState            = "engine_sleep_state"
	HTTPRequestTotal            = "http_requests_total"
	NumPreemptionsTotal         = "num_preemptions_total"
	RequestSuccessTotal         = "request_success_total"
	NumPrefillPreallocQueueReqs = "num_prefill_prealloc_queue_reqs"
	NumDecodePreallocQueueReqs  = "num_decode_prealloc_queue_reqs"

	E2ERequestLatencySeconds        = "e2e_request_latency_seconds"
	RequestQueueTimeSeconds         = "request_queue_time_seconds"
	RequestInferenceTimeSeconds     = "request_inference_time_seconds"
	PerStageReqLatencySeconds       = "per_stage_req_latency_seconds"
	HTTPRequestDurationSeconds      = "http_request_duration_seconds"
	HTTPRequestDurationHighRSeconds = "http_request_duration_highr_seconds"

	TimeToFirstTokenSeconds   = "time_to_first_token_seconds"
	RequestPrefillTimeSeconds = "request_prefill_time_seconds"
	PromptTokenTotal          = "prompt_tokens_total"
	RequestPromptTokens       = "request_prompt_tokens"

	// deprecated (time_per_output_token_seconds), use inter_token_latency_seconds instead
	TimePerOutputTokenSeconds        = "time_per_output_token_seconds"
	InterTokenLatencySeconds         = "inter_token_latency_seconds"
	RequestTimePerOutputTokenSeconds = "request_time_per_output_token_seconds"
	RequestDecodeTimeSeconds         = "request_decode_time_seconds"

	GenerationTokenTotal          = "generation_tokens_total"
	IterationTokensTotal          = "iteration_tokens_total"
	RequestGenerationTokens       = "request_generation_tokens"
	RequestMaxNumGenerationTokens = "request_max_num_generation_tokens"

	KVCacheUsagePerc                = "kv_cache_usage_perc"
	NixlNumFailedTransfers          = "nixl_num_failed_transfers_total"
	NixlNumFailedNotifications      = "nixl_num_failed_notifications_total"
	PrefixCacheHitTotal             = "prefix_cache_hits_total"
	PrefixCacheQueriesTotal         = "prefix_cache_queries_total"
	ExternalPrefixCacheHitsTotal    = "external_prefix_cache_hits_total"
	ExternalPrefixCacheQueriesTotal = "external_prefix_cache_queries_total"

	NixlXferTimeSeconds  = "nixl_xfer_time_seconds"
	NixlPostTimeSeconds  = "nixl_post_time_seconds"
	NixlBytesTransferred = "nixl_bytes_transferred"
	NixlNumDescriptors   = "nixl_num_descriptors"

	DrainRate1m = "drain_rate_1m"

	P95TTFT5m                            = "p95_ttft_5m"
	P95TTFT5mPod                         = "p95_ttft_5m_pod"
	AvgTTFT5mPod                         = "avg_ttft_5m_pod"
	P95TPOT5mPod                         = "p95_tpot_5m_pod"
	AvgTPOT5mPod                         = "avg_tpot_pod_5m"
	AvgPromptToksPerReq                  = "avg_prompt_toks_per_req"
	AvgGenerationToksPerReq              = "avg_generation_toks_per_req"
	GPUCacheUsagePerc                    = "gpu_cache_usage_perc"
	GPUBusyTimeRatio                     = "gpu_busy_time_ratio"
	CPUCacheUsagePerc                    = "cpu_cache_usage_perc"
	EngineUtilization                    = "engine_utilization"
	AvgE2ELatencyPod                     = "avg_e2e_latency_pod"
	AvgRequestsPerMinPod                 = "avg_requests_per_min_pod"
	AvgPromptThroughputToksPerMinPod     = "avg_prompt_throughput_toks_per_min_pod"
	AvgGenerationThroughputToksPerMinPod = "avg_generation_throughput_toks_per_min_pod"
	MaxLora                              = "max_lora"
	WaitingLoraAdapters                  = "waiting_lora_adapters"
	RunningLoraAdapters                  = "running_lora_adapters"
	VTCBucketSizeActive                  = "vtc_bucket_size_active"
	// Realtime metrics
	RealtimeNumRequestsRunning = "realtime_num_requests_running"
	RealtimeNormalizedPendings = "realtime_normalized_pendings"

	// error to read metrics from backend
	PrometheusQueryFail       = "prometheus_query_fail"
	LLMEngineMetricsQueryFail = "llm_engine_metrics_query_fail"

	// Deprecated metrics
	NumRequestsSwapped              = "num_requests_swapped"
	AvgPromptThroughputToksPerS     = "avg_prompt_throughput_toks_per_s"
	AvgGenerationThroughputToksPerS = "avg_generation_throughput_toks_per_s"
)

var (
	// Metrics defines all available metrics, including raw and query-based metrics.
	Metrics = map[string]Metric{
		NumRequestsRunning: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:num_requests_running",
				"sglang": "sglang:num_running_reqs",
			},
			Description: "Number of running requests",
		},
		NumRequestsWaiting: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:num_requests_waiting",
				"sglang": "sglang:num_queue_reqs",
			},
			Description: "Number of waiting requests",
		},
		NumRequestsSwapped: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:num_requests_swapped",
				"sglang": "sglang:num_retracted_reqs",
			},
			Description: "Number of swapped requests",
		},
		EngineSleepState: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:engine_sleep_state",
			},
			Description: "Engine sleep state; awake = 0 means engine is sleeping; awake = 1 means engine is awake; weights_offloaded = 1 means sleep level 1; discard_all = 1 means sleep level 2.",
		},
		HTTPRequestTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:http_requests_total",
			},
			Description: "Total number of requests by method, status and handler.",
		},
		NumPreemptionsTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:num_preemptions_total",
			},
			Description: "Number of preemptions",
		},
		RequestSuccessTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:num_requests_success_total",
				"sglang": "sglang:num_requests_total",
			},
			Description: "Number of successful requests",
		},
		NumPrefillPreallocQueueReqs: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"sglang": "sglang:num_prefill_prealloc_queue_reqs",
			},
			Description: "Number of prefill preallocation queue requests",
		},
		NumDecodePreallocQueueReqs: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"sglang": "sglang:num_decode_prealloc_queue_reqs",
			},
			Description: "Number of decode preallocation queue requests",
		},

		E2ERequestLatencySeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:e2e_request_latency_seconds",
				"sglang": "sglang:e2e_request_latency_seconds",
			},
			Description: "End-to-end request latency in seconds",
		},
		RequestQueueTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_queue_time_seconds",
			},
			Description: "Request queue time in seconds",
		},
		RequestInferenceTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_inference_time_seconds",
			},
			Description: "Request inference time in seconds",
		},
		PerStageReqLatencySeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"sglang": "sglang:per_stage_req_latency_seconds",
			},
			Description: "Per-stage request latency in seconds",
		},
		HTTPRequestDurationSeconds: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "http_request_duration_seconds",
			},
			Description: "Histogram of request duration in seconds",
		},
		HTTPRequestDurationHighRSeconds: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "http_request_duration_highr_seconds",
			},
			Description: "Histogram of request duration in seconds for high priority requests",
		},
		PromptTokenTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:prompt_tokens_total",
			},
			Description: "Total prompt tokens",
		},
		RequestPromptTokens: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_prompt_tokens",
			},
			Description: "Histogram of prompt tokens",
		},
		GenerationTokenTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:generation_tokens_total",
			},
			Description: "Total generation tokens",
		},
		RequestGenerationTokens: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_generation_tokens",
			},
			Description: "Histogram of generation tokens",
		},
		RequestMaxNumGenerationTokens: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_max_num_generation_tokens",
			},
			Description: "Histogram of max number of generation tokens",
		},
		IterationTokensTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:iteration_tokens_total",
			},
			Description: "Total iteration tokens",
		},
		TimeToFirstTokenSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:time_to_first_token_seconds",
				"sglang": "sglang:time_to_first_token_seconds",
			},
			Description: "Time to first token in seconds",
		},
		TimePerOutputTokenSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:time_per_output_token_seconds",
				"sglang": "sglang:inter_token_latency_seconds",
			},
			Description: "Time per output token in seconds",
		},
		InterTokenLatencySeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:inter_token_latency_seconds",
				"sglang": "sglang:inter_token_latency_seconds",
			},
			Description: "Inter-token latency in seconds",
		},
		RequestDecodeTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_decode_time_seconds",
			},
			Description: "Request decode time in seconds",
		},
		RequestPrefillTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_prefill_time_seconds",
			},
			Description: "Request prefill time in seconds",
		},
		RequestTimePerOutputTokenSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:request_time_per_output_token_seconds",
			},
			Description: "Time per output token in seconds",
		},
		GPUCacheUsagePerc: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:gpu_cache_usage_perc",
				"sglang": "sglang:token_usage", // Based on https://github.com/sgl-project/sglang/issues/5979
				"xllm":   "kv_cache_utilization",
			},
			Description: "GPU cache usage percentage",
		},
		EngineUtilization: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"xllm": "engine_utilization",
			},
			Description: "GPU busy time ratio",
		},
		CPUCacheUsagePerc: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:cpu_cache_usage_perc",
			},
			Description: "CPU cache usage percentage",
		},
		KVCacheUsagePerc: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:kv_cache_usage_perc",
				"sglang": "sglang:token_usage", // Based on https://github.com/sgl-project/sglang/issues/5979
				"xllm":   "kv_cache_utilization",
			},
			Description: "KV-cache usage. 1 means 100 percent usage.",
		},
		PrefixCacheQueriesTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:prefix_cache_queries_total",
			},
			Description: "Prefix cache queries, in terms of number of queried tokens..",
		},
		PrefixCacheHitTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:prefix_cache_hits_total",
			},
			Description: "Prefix cache hits, in terms of number of cached tokens.",
		},
		ExternalPrefixCacheQueriesTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:external_prefix_cache_queries_total",
			},
			Description: "External prefix cache queries from KV connector cross-instance cache sharing, in terms of number of queried tokens.",
		},
		ExternalPrefixCacheHitsTotal: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:external_prefix_cache_hits_total",
			},
			Description: "External prefix cache hits from KV connector cross-instance cache sharing, in terms of number of cached tokens.",
		},

		NixlXferTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_xfer_time_seconds",
			},
			Description: "transfer duration for NIXL KV Cache transfers",
		},
		NixlPostTimeSeconds: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_post_time_seconds",
			},
			Description: "transfer post time for NIXL KV Cache transfers",
		},
		NixlBytesTransferred: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_bytes_transferred",
			},
			Description: "number of bytes transferred per NIXL KV Cache transfer",
		},
		NixlNumDescriptors: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_num_descriptors",
			},
			Description: "number of descriptors per NIXL  KV Cache transfers",
		},
		NixlNumFailedTransfers: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_num_failed_transfers",
			},
			Description: "number of failed NIXL KV Cache transfers",
		},
		NixlNumFailedNotifications: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:nixl_num_failed_notifications",
			},
			Description: "number of failed NIXL KV Cache notifications",
		},

		// Query-based metrics
		// TODO: make it agnostic to the engine
		DrainRate1m: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PrometheusEndpoint,
			MetricType: MetricType{
				Query: PromQL,
			},
			// Standard rate for the finished requests counter (clamped to avoid divide-by-zero)
			PromQL: `
			clamp_min(
				rate(
					sglang:num_requests_total{
						instance="${instance}",
						model_name="${model_name}",
						job="pods"
					}[1m]
				),
				0.01
			)`,
			Description: "1-minute average rate of finished requests (Drains), clamped to avoid zero",
		},

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
		AvgPromptThroughputToksPerS: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:avg_prompt_throughput_toks_per_s",
			},
			Description: "Average prompt throughput in tokens per second",
		},
		AvgGenerationThroughputToksPerS: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			EngineMetricsNameMapping: map[string]string{
				"vllm":   "vllm:avg_generation_throughput_toks_per_s",
				"sglang": "sglang:gen_throughput",
			},
			Description: "Average generation throughput in tokens per second",
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
			LabelKey: "max_lora",
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:lora_requests_info",
			},
			Description: "Max count of Lora Adapters",
		},
		RunningLoraAdapters: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Query: QueryLabel,
			},
			LabelKey: "running_lora_adapters",
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:lora_requests_info",
			},
			Description: "Count of running Lora Adapters",
		},
		WaitingLoraAdapters: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Query: QueryLabel,
			},
			LabelKey: "waiting_lora_adapters",
			EngineMetricsNameMapping: map[string]string{
				"vllm": "vllm:lora_requests_info",
			},
			Description: "Count of waiting Lora Adapters",
		},
		VTCBucketSizeActive: {
			MetricScope:  PodModelMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Current adaptive bucket size used by VTC algorithm for token normalization",
		},
		PrometheusQueryFail: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of Prometheus query failures",
		},
		LLMEngineMetricsQueryFail: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of LLM engine metrics query failures",
		},
	}
)
