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
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"k8s.io/klog/v2"

	"time"
)

const (
	metricServerDefaultMetricWindow = time.Minute
)

type KPAMetricsClient struct {
	fetcher MetricFetcher

	// collectionsMutex protects access to both panicWindowDict and stableWindowDict,
	// ensuring thread-safe read and write operations. It uses a read-write mutex to
	// allow multiple concurrent reads while preventing race conditions during write
	// operations on the window dictionaries.
	collectionsMutex sync.RWMutex
	// the time range of stable metrics
	stableDuration time.Duration
	// the time range of panic metrics
	panicDuration time.Duration
	// granularity represents the time interval at which metrics are aggregated.
	// It determines the frequency of data points being added to the sliding window
	// for both stable and panic metrics. Each data point is recorded at a
	// specific timestamp, and the granularity defines how often these points
	// are collected and processed within the sliding window.
	granularity time.Duration
	// the difference between stable and panic metrics is the time window range
	panicWindowDict  map[NamespaceNameMetric]*aggregation.TimeWindow
	stableWindowDict map[NamespaceNameMetric]*aggregation.TimeWindow
}

var _ MetricClient = (*KPAMetricsClient)(nil)

// NewKPAMetricsClient initializes and returns a KPAMetricsClient with specified durations.
func NewKPAMetricsClient(fetcher MetricFetcher) *KPAMetricsClient {
	client := &KPAMetricsClient{
		fetcher:          fetcher,
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

func (c *KPAMetricsClient) UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error {
	// Calculate the total value from the retrieved metrics
	var sumMetricValue float64
	for _, metricValue := range metricValues {
		sumMetricValue += metricValue
	}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	// Update metrics into the window for tracking
	err := c.UpdateMetricIntoWindow(metricKey, now, sumMetricValue)
	if err != nil {
		return err
	}
	klog.InfoS("Update pod list metrics", "metricKey", metricKey, "podListNum", len(metricValues), "timestamp", now, "metricValue", sumMetricValue)
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

func (c *KPAMetricsClient) GetPodContainerMetric(ctx context.Context, pod corev1.Pod, metricName string, metricPort int) (PodMetricsInfo, time.Time, error) {
	return GetPodContainerMetric(ctx, c.fetcher, pod, metricName, metricPort)
}

func (c *KPAMetricsClient) GetMetricsFromPods(ctx context.Context, pods []corev1.Pod, metricName string, metricPort int) ([]float64, error) {
	return GetMetricsFromPods(ctx, c.fetcher, pods, metricName, metricPort)
}

type APAMetricsClient struct {
	fetcher MetricFetcher
	// collectionsMutex protects access to both panicWindowDict and stableWindowDict,
	// ensuring thread-safe read and write operations. It uses a read-write mutex to
	// allow multiple concurrent reads while preventing race conditions during write
	// operations on the window dictionaries.
	collectionsMutex sync.RWMutex
	// the time range of metrics
	duration time.Duration
	// granularity represents the time interval at which metrics are aggregated.
	// It determines the frequency of data points being added to the sliding window
	// for both stable and panic metrics. Each data point is recorded at a
	// specific timestamp, and the granularity defines how often these points
	// are collected and processed within the sliding window.
	granularity time.Duration
	// stable time window
	windowDict map[NamespaceNameMetric]*aggregation.TimeWindow
}

var _ MetricClient = (*APAMetricsClient)(nil)

// NewAPAMetricsClient initializes and returns a KPAMetricsClient with specified durations.
func NewAPAMetricsClient(fetcher MetricFetcher) *APAMetricsClient {
	client := &APAMetricsClient{
		fetcher:     fetcher,
		duration:    60 * time.Second,
		granularity: time.Second,
		windowDict:  make(map[NamespaceNameMetric]*aggregation.TimeWindow),
	}
	return client
}

func (c *APAMetricsClient) UpdateMetricIntoWindow(metricKey NamespaceNameMetric, now time.Time, metricValue float64) error {
	// Add to metric window; create a new window if not present in the map
	// Ensure that windowDict maps are checked and updated
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

	// Update metrics windows
	updateWindow(c.windowDict, c.duration)
	return nil
}

func (c *APAMetricsClient) UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error {
	// Calculate the total value from the retrieved metrics
	var sumMetricValue float64
	for _, metricValue := range metricValues {
		sumMetricValue += metricValue
	}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	// Update metrics into the window for tracking
	err := c.UpdateMetricIntoWindow(metricKey, now, sumMetricValue)
	if err != nil {
		return err
	}
	klog.InfoS("Update pod list metrics", "metricKey", metricKey, "podListNum", len(metricValues), "timestamp", now, "metricValue", sumMetricValue)
	return nil
}

func (c *APAMetricsClient) GetMetricValue(
	metricKey NamespaceNameMetric, now time.Time) (float64, error) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	window, exists := c.windowDict[metricKey]
	if !exists {
		return -1, fmt.Errorf("metrics %s not found", metricKey)
	}

	metricValue, err := window.Avg()
	if err != nil {
		return -1, err
	}

	return metricValue, nil
}

func (c *APAMetricsClient) GetPodContainerMetric(ctx context.Context, pod corev1.Pod, metricName string, metricPort int) (PodMetricsInfo, time.Time, error) {
	return GetPodContainerMetric(ctx, c.fetcher, pod, metricName, metricPort)
}

func (c *APAMetricsClient) GetMetricsFromPods(ctx context.Context, pods []corev1.Pod, metricName string, metricPort int) ([]float64, error) {
	return GetMetricsFromPods(ctx, c.fetcher, pods, metricName, metricPort)
}
