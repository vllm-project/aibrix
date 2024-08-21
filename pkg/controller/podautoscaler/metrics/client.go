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
	"io"
	"net/http"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"

	"time"
)

const (
	metricServerDefaultMetricWindow = time.Minute
)

// restMetricsClient is a client which supports fetching
// metrics from the pod metrics prometheus API. In future,
// it can fetch from the ai runtime api directly.
type restMetricsClient struct {
}

func (r restMetricsClient) GetPodContainerMetric(ctx context.Context, metricName string, pod corev1.Pod, containerPort int) (PodMetricsInfo, time.Time, error) {
	panic("not implemented")
}

func (r restMetricsClient) GetObjectMetric(ctx context.Context, metricName string, namespace string, objectRef *autoscalingv2.CrossVersionObjectReference, containerPort int) (PodMetricsInfo, time.Time, error) {
	//TODO implement me
	panic("implement me")
}

func GetMetricsFromPods(pods []corev1.Pod, metricName string, metricsPort int) ([]float64, error) {
	metrics := make([]float64, 0, len(pods))

	for _, pod := range pods {
		// We should use the primary container port. In future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, metricsPort)

		// scrape metrics
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch metrics from pod %s: %v", pod.Name, err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				// Handle the error here. For example, log it or take appropriate corrective action.
				klog.InfoS("Error closing response body:", err)
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response from pod %s: %v", pod.Name, err)
		}

		metricValue, err := parseMetricFromBody(body, metricName)
		if err != nil {
			return nil, fmt.Errorf("failed to parse metrics from pod %s: %v", pod.Name, err)
		}

		metrics = append(metrics, metricValue)
	}

	return metrics, nil
}

func parseMetricFromBody(body []byte, metricName string) (float64, error) {
	lines := strings.Split(string(body), "\n")

	for _, line := range lines {
		if strings.Contains(line, metricName) {
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
