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
	"sync"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
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
		//// TODO a temp for debugging
		//url := fmt.Sprintf("http://%s:%d/metrics", "127.0.0.1", metricsPort)

		// scrape metrics
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch metrics from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, metricsPort, err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				// Handle the error here. For example, log it or take appropriate corrective action.
				klog.InfoS("Error closing response body:", err)
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, metricsPort, err)
		}

		metricValue, err := parseMetricFromBody(body, metricName)
		if err != nil {
			return nil, fmt.Errorf("failed to parse metrics from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, metricsPort, err)
		}

		metrics = append(metrics, metricValue)

		klog.InfoS("Successfully parsed metrics", "metric", metricName, "PodIP", pod.Status.PodIP, "Port", metricsPort, "metricValue", metricValue)
	}

	return metrics, nil
}

func parseMetricFromBody(body []byte, metricName string) (float64, error) {
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

type KPAMetricsClient struct {
	*restMetricsClient
	collectionsMutex sync.RWMutex

	// the time range of stable metrics
	stableDuration time.Duration
	// the time range of panic metrics
	panicDuration time.Duration

	granularity time.Duration

	// the difference between stable and panic metrics is the time window range
	panicWindowDict  map[NamespaceNameMetric]*aggregation.TimeWindow
	stableWindowDict map[NamespaceNameMetric]*aggregation.TimeWindow
}

// NewKPAMetricsClient initializes and returns a KPAMetricsClient with specified durations.
func NewKPAMetricsClient() *KPAMetricsClient {
	client := &KPAMetricsClient{
		stableDuration:   60 * time.Second,
		panicDuration:    10 * time.Second,
		granularity:      time.Second,
		panicWindowDict:  make(map[NamespaceNameMetric]*aggregation.TimeWindow),
		stableWindowDict: make(map[NamespaceNameMetric]*aggregation.TimeWindow),
	}
	return client
}

func (c *KPAMetricsClient) UpdateMetricIntoWindow(metricKey NamespaceNameMetric, now time.Time, metricValue float64) error {
	// Add to panic and stable windows; create a new window if not present in the map
	// Ensure that panicWindowDict and stableWindowDict maps are checked and updated
	updateWindow := func(windowDict map[NamespaceNameMetric]*aggregation.TimeWindow, duration time.Duration) {
		window, exists := windowDict[metricKey]
		if !exists {
			// Create a new TimeWindow if it does not exist
			windowDict[metricKey] = aggregation.NewTimeWindow(duration, c.granularity)
			window = windowDict[metricKey]
		}
		// Record the maximum metric value in the TimeWindow
		window.Record(now, metricValue)
	}

	// Update panic and stable windows
	updateWindow(c.panicWindowDict, c.panicDuration)
	updateWindow(c.stableWindowDict, c.stableDuration)
	return nil
}

func (c *KPAMetricsClient) UpdatePodListMetric(ctx context.Context, metricKey NamespaceNameMetric, podList *corev1.PodList, containerPort int, now time.Time) error {
	// Retrieve metrics from a list of pods
	metricValues, err := GetMetricsFromPods(podList.Items, metricKey.MetricName, containerPort)
	if err != nil {
		return err
	}

	// Calculate the total value from the retrieved metrics
	var sumMetricValue float64
	for _, metricValue := range metricValues {
		sumMetricValue += metricValue
	}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	err = c.UpdateMetricIntoWindow(metricKey, now, sumMetricValue)
	if err != nil {
		return err
	}
	klog.InfoS("Update pod list metrics", "metricKey", metricKey, "podListNum", podList.Size(), "timestamp", now, "metricValue", sumMetricValue)
	return nil
}

func (c *KPAMetricsClient) StableAndPanicMetrics(
	metricKey NamespaceNameMetric, now time.Time) (float64, float64, error) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	panicWindow, exists := c.panicWindowDict[metricKey]
	if !exists {
		return -1, -1, fmt.Errorf("panic metrics %s not found", metricKey)
	}

	panicValue, err := panicWindow.Avg()
	if err != nil {
		return -1, -1, err
	}

	stableWindow, exists := c.stableWindowDict[metricKey]
	if !exists {
		return -1, -1, fmt.Errorf("stable metrics %s not found", metricKey)
	}
	stableValue, err := stableWindow.Avg()
	if err != nil {
		return -1, -1, err
	}

	return stableValue, panicValue, nil
}
