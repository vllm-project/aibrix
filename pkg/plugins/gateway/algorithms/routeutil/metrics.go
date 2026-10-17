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

package routeutil

import (
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// GetPodModelMetricsSimpleValue returns the pod's simple value for metricname, or 0 when the cache
// has no such metric.
func GetPodModelMetricsSimpleValue(cache cache.Cache, podname, namespace, modelname, metricname string) float64 {
	metricValue, err := cache.GetMetricValueByPodModel(podname, namespace, modelname, metricname)
	if err != nil {
		metricValue = &metrics.SimpleMetricValue{Value: 0}
	}

	return metricValue.GetSimpleValue()
}

// GetPodModelMetricsSimplePrometheusValue returns the pod's numeric Prometheus value for metricname,
// or 0 when the cache has none or it is not numeric.
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
