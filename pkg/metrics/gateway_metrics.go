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
	GatewayRequestTotal             = "gateway_request_total"
	GatewayRequestModelSuccessTotal = "gateway_request_model_success_total"
	GatewayRequestModelFailTotal    = "gateway_request_model_fail_total"

	GatewayPrefillRequestSuccessTotal = "gateway_prefill_request_success_total"
	GatewayPrefillRequestFailTotal    = "gateway_prefill_request_fail_total"

	GatewayPrefillOutstandingRequests = "gateway_prefill_outstanding_requests"
	GatewayDecodeOutstandingRequests  = "gateway_decode_outstanding_requests"

	GatewayE2EDuration = "gateway_e2e_duration_seconds"
	GatewayInFlight    = "gateway_in_flight_requests"
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
	}
)
