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

package routingalgorithms

import (
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// calculatePodScoreBasedOffRequestRate calculates the score of a pod based off the ratio of requests waiting vs requests draining.
func calculatePodScoreBasedOffRequestRate(routingCtx *types.RoutingContext, cache cache.Cache, pod *v1.Pod) float64 {
	modelName := pod.Labels[constants.ModelLabelName]
	waitingReqs := GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumRequestsWaiting)
	prefillPreallocQueue := GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumPrefillPreallocQueueReqs)
	decodePreallocQueue := GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumDecodePreallocQueueReqs)
	drainRate1m := GetPodModelMetricsSimplePrometheusValue(cache, pod.Name, pod.Namespace, modelName, metrics.DrainRate1m)

	return (waitingReqs + prefillPreallocQueue + decodePreallocQueue) / drainRate1m
}

func GetPodModelMetricsSimpleValue(cache cache.Cache, podname, namespace, modelname, metricname string) float64 {
	metricValue, err := cache.GetMetricValueByPodModel(podname, namespace, modelname, metricname)
	if err != nil {
		metricValue = &metrics.SimpleMetricValue{Value: 0}
	}

	return metricValue.GetSimpleValue()
}

func GetPodModelMetricsSimplePrometheusValue(cache cache.Cache, podname, namespace, modelname, metricname string) float64 {
	metricValue, err := cache.GetMetricValueByPodModel(podname, namespace, modelname, metricname)
	if err != nil {
		metricValue = &metrics.PrometheusMetricValue{Result: nil}
	}

	metricValueFloat, err := metrics.ExtractNumericFromPromResult(metricValue.GetPrometheusResult())
	if err != nil {
		metricValueFloat = 0
	}

	return metricValueFloat
}
