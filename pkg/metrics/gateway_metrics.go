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
	// counter to track #success prefill requests
	GatewayPrefillRequestSuccessTotal = "gateway_prefill_request_success_total"
	// counter to track #fail prefill requests
	GatewayPrefillRequestFailTotal = "gateway_prefill_request_fail_total"

	// counter to track #prefill pods selected by pd
	PDSelectedPrefillPodTotal = "pd_selected_prefill_pod_total"
	// counter to track #decode pods selected by pd
	PDSelectedDecodePodTotal = "pd_selected_decode_pod_total"
)

var (
	GatewayMetrics = map[string]Metric{
		GatewayPrefillRequestSuccessTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "counter to track #success prefill requests",
		},
		GatewayPrefillRequestFailTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "counter to track #fail prefill requests",
		},
		PDSelectedPrefillPodTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "counter to track #prefill pods selected by pd",
		},
		PDSelectedDecodePodTotal: {
			MetricScope:  PodMetricScope,
			MetricSource: PodRawMetrics,
			MetricType: MetricType{
				Raw: Counter,
			},
			Description: "counter to track #decode pods selected by pd",
		},
	}
)
