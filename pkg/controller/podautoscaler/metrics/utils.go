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
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Constants for metric validation (duplicated here to avoid import cycle)
const (
	minValidMetricValue = -1e6
	maxValidMetricValue = 1e6
)

// validateMetricValue checks if a metric value is within acceptable bounds
func validateMetricValue(value float64) error {
	if math.IsNaN(value) {
		return fmt.Errorf("metric value is NaN")
	}
	if math.IsInf(value, 0) {
		return fmt.Errorf("metric value is infinite")
	}
	if value < minValidMetricValue || value > maxValidMetricValue {
		return fmt.Errorf("metric value %f is outside valid range [%f, %f]", value, minValidMetricValue, maxValidMetricValue)
	}
	return nil
}

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

func GetPodContainerMetric(ctx context.Context, fetcher MetricFetcher, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (PodMetricsInfo, time.Time, error) {
	endpoint := fmt.Sprintf("%s:%s", pod.Status.PodIP, source.Port)
	_, err := fetcher.FetchMetric(ctx, source.ProtocolType, endpoint, source.Path, source.TargetMetric)
	currentTimestamp := time.Now()
	if err != nil {
		return nil, currentTimestamp, err
	}

	// TODO(jiaxin.shan): convert this raw metric to PodMetrics
	return nil, currentTimestamp, nil
}

func GetMetricsFromPods(ctx context.Context, fetcher MetricFetcher, pods []corev1.Pod, source autoscalingv1alpha1.MetricSource) ([]float64, error) {
	if len(pods) == 0 {
		return []float64{}, nil
	}

	metrics := make([]float64, 0, len(pods))
	var failedPods []string
	var errors []string

	for _, pod := range pods {
		// Skip pods that are not ready or in terminal state
		if pod.Status.Phase != corev1.PodRunning {
			klog.V(4).InfoS("Skipping pod not in running state", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name), "phase", pod.Status.Phase)
			continue
		}

		// Check if pod has valid IP
		if pod.Status.PodIP == "" {
			klog.V(4).InfoS("Skipping pod without IP address", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
			failedPods = append(failedPods, fmt.Sprintf("%s/%s (no IP)", pod.Namespace, pod.Name))
			continue
		}

		endpoint := fmt.Sprintf("%s:%s", pod.Status.PodIP, source.Port)
		metric, err := fetcher.FetchMetric(ctx, source.ProtocolType, endpoint, source.Path, source.TargetMetric)
		if err != nil {
			errors = append(errors, err.Error())
			failedPods = append(failedPods, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
			continue
		}

		// Validate metric value before adding to results
		if err := validateMetricValue(metric); err != nil {
			errors = append(errors, fmt.Sprintf("pod %s/%s: %v", pod.Namespace, pod.Name, err))
			failedPods = append(failedPods, fmt.Sprintf("%s/%s (invalid value)", pod.Namespace, pod.Name))
			continue
		}

		metrics = append(metrics, metric)
	}

	// Log detailed information about failures
	if len(metrics) < len(pods) {
		successRate := float64(len(metrics)) / float64(len(pods)) * 100
		klog.Warningf("Partial metrics collection: got %d/%d metrics (%.1f%% success rate). Failed pods: %v",
			len(metrics), len(pods), successRate, failedPods)
		if len(errors) > 0 {
			klog.V(4).InfoS("Metric collection errors", "errors", errors)
		}
	}

	// Return error only if we couldn't get any metrics at all
	if len(metrics) == 0 && len(pods) > 0 {
		return nil, fmt.Errorf("failed to get metrics from any of the %d pods. Last errors: %v", len(pods), errors[len(errors)-min(len(errors), 3):])
	}

	return metrics, nil
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func GetMetricFromSource(ctx context.Context, fetcher MetricFetcher, source autoscalingv1alpha1.MetricSource) (float64, error) {
	endpoint := source.Endpoint

	// If port specified, try override.
	if source.Port != "" {
		u := url.URL{Host: source.Endpoint}
		if u.Port() == "" {
			endpoint = fmt.Sprintf("%s:%s", u.Hostname(), source.Port)
		}
	}
	return fetcher.FetchMetric(ctx, source.ProtocolType, endpoint, source.Path, source.TargetMetric)
}
