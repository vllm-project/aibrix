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

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func ParseMetricFromBody(body []byte, metricName string) (float64, error) {
	lines := strings.Split(string(body), "\n")

	for _, line := range lines {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, metricName) {
			// format is `http_requests_total 1234.56`
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("unexpected format for metric %s", metricName)
			}

			// parse to float64
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse metric value for %s: %v", metricName, err)
			}

			return value, nil
		}
	}
	return 0, fmt.Errorf("metrics %s not found", metricName)
}

// GetResourceUtilizationRatio takes in a set of metrics, a set of matching requests,
// and a target utilization percentage, and calculates the ratio of
// desired to actual utilization (returning that, the actual utilization, and the raw average value)
func GetResourceUtilizationRatio(metrics PodMetricsInfo, requests map[string]int64, targetUtilization int32) (utilizationRatio float64, currentUtilization int32, rawAverageValue int64, err error) {
	metricsTotal := int64(0)
	requestsTotal := int64(0)
	numEntries := 0

	for podName, metric := range metrics {
		request, hasRequest := requests[podName]
		if !hasRequest {
			// we check for missing requests elsewhere, so assuming missing requests == extraneous metrics
			continue
		}

		metricsTotal += metric.Value
		requestsTotal += request
		numEntries++
	}

	// if the set of requests is completely disjoint from the set of metrics,
	// then we could have an issue where the requests total is zero
	if requestsTotal == 0 {
		return 0, 0, 0, fmt.Errorf("no metrics returned matched known pods")
	}

	currentUtilization = int32((metricsTotal * 100) / requestsTotal)

	return float64(currentUtilization) / float64(targetUtilization), currentUtilization, metricsTotal / int64(numEntries), nil
}

// GetMetricUsageRatio takes in a set of metrics and a target usage value,
// and calculates the ratio of desired to actual usage
// (returning that and the actual usage)
func GetMetricUsageRatio(metrics PodMetricsInfo, targetUsage int64) (usageRatio float64, currentUsage int64) {
	metricsTotal := int64(0)
	for _, metric := range metrics {
		metricsTotal += metric.Value
	}

	currentUsage = metricsTotal / int64(len(metrics))

	return float64(currentUsage) / float64(targetUsage), currentUsage
}

func GetPodContainerMetric(ctx context.Context, fetcher MetricFetcher, pod corev1.Pod, metricName string, metricPort int) (PodMetricsInfo, time.Time, error) {
	_, err := fetcher.FetchPodMetrics(ctx, pod, metricPort, metricName)
	currentTimestamp := time.Now()
	if err != nil {
		return nil, currentTimestamp, err
	}

	// TODO(jiaxin.shan): convert this raw metric to PodMetrics
	return nil, currentTimestamp, nil
}

func GetMetricsFromPods(ctx context.Context, fetcher MetricFetcher, pods []corev1.Pod, metricName string, metricPort int) ([]float64, error) {
	metrics := make([]float64, 0, len(pods))
	for _, pod := range pods {
		// TODO: Let's optimize the performance for multi-metrics later.
		metric, err := fetcher.FetchPodMetrics(ctx, pod, metricPort, metricName)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}
