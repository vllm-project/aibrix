/*
Copyright 2025 The Aibrix Team.

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
	GatewayRequestTotal = "gateway_request_total"
	GatewayE2EDuration  = "gateway_e2e_duration_seconds"
	GatewayInFlight     = "gateway_in_flight_requests"

	// Count of streamed responses where first token delay > 1s
	GatewayFirstTokenDelayOver1sTotal = "gateway_first_token_delay_over_1s_total"

	// counter to track #success & #fail requests
	GatewayRequestSuccessTotal = "gateway_request_success_total"
	GatewayRequestFailTotal    = "gateway_request_fail_total"

	// counter to track #success & #fail requests for each model
	GatewayRequestModelSuccessTotal = "gateway_request_model_success_total"
	GatewayRequestModelFailTotal    = "gateway_request_model_fail_total"

	// counter to track #prompt & #completion tokenss
	GatewayPromptTokenBucketTotal     = "gateway_prompt_token_bucket_total"
	GatewayCompletionTokenBucketTotal = "gateway_completion_token_bucket_total"

	// counter to track #success & #fail prefill requests
	GatewayPrefillRequestSuccessTotal = "gateway_prefill_request_success_total"
	GatewayPrefillRequestFailTotal    = "gateway_prefill_request_fail_total"

	// gauge to track #outstanding prefill & decode requests
	GatewayPrefillOutstandingRequests = "gateway_prefill_outstanding_requests"
	GatewayDecodeOutstandingRequests  = "gateway_decode_outstanding_requests"

	// counter to track #prefill & #decode pods selected by pd
	PDSelectedPrefillPodTotal = "pd_selected_prefill_pod_total"
	PDSelectedDecodePodTotal  = "pd_selected_decode_pod_total"

	// Duration bucket counters for timing breakdowns
	GatewayRoutingTimeBucketTotal    = "gateway_routing_time_bucket_total"
	GatewayPrefillTimeBucketTotal    = "gateway_prefill_time_bucket_total"
	GatewayKVTransferTimeBucketTotal = "gateway_kv_transfer_time_bucket_total"
	GatewayTTFTBucketTotal           = "gateway_ttft_bucket_total"
	GatewayTPOTBucketTotal           = "gateway_tpot_bucket_total"
	GatewayDecodeTimeBucketTotal     = "gateway_decode_time_bucket_total"
	GatewayTotalTimeBucketTotal      = "gateway_total_time_bucket_total"
)

var (
	GatewayMetrics = map[string]Metric{
		GatewayRequestTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of requests received by the gateway",
		},

		GatewayRequestModelSuccessTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of successful requests received by the gateway for each model",
		},
		GatewayRequestModelFailTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of failed requests received by the gateway for each model",
		},
		GatewayPrefillRequestSuccessTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of successful prefill requests received by the gateway",
		},
		GatewayPrefillRequestFailTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total number of failed prefill requests received by the gateway",
		},
		GatewayPrefillOutstandingRequests: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Total number of outstanding prefill requests received by the gateway",
		},
		GatewayDecodeOutstandingRequests: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Total number of outstanding decode requests received by the gateway",
		},
		GatewayE2EDuration: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Histogram,
			},
			Description: "End-to-end latency distribution of requests received by the gateway",
		},
		GatewayInFlight: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Gauge,
			},
			Description: "Current number of requests in flight (i.e., being processed) by the gateway",
		},

		GatewayFirstTokenDelayOver1sTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Count of streamed responses where first token delay > 1s",
		},
		// Bucketized prompt token counters
		GatewayPromptTokenBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Requests counted by prompt token bucket",
		},
		// Bucketized completion token counters
		GatewayCompletionTokenBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Requests counted by completion token bucket",
		},
		PDSelectedPrefillPodTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total selections of prefill pods by the PD router",
		},
		PDSelectedDecodePodTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "Total selections of decode pods by the PD router",
		},
		// Duration bucket counters
		GatewayRoutingTimeBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by routing time bucket",
		},
		GatewayPrefillTimeBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by prefill time bucket",
		},
		GatewayKVTransferTimeBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by KV transfer time bucket",
		},
		GatewayTTFTBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by TTFT bucket",
		},
		GatewayTPOTBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by TPOT bucket",
		},
		GatewayDecodeTimeBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by decode time bucket",
		},
		GatewayTotalTimeBucketTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType:   MetricType{Raw: Counter},
			Description:  "Requests counted by total time bucket",
		},
	}
)
