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
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"k8s.io/klog/v2"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"

	"time"
)

const (
	metricServerDefaultMetricWindow = time.Minute
	paGranularity                   = time.Second
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
	panicWindow  *aggregation.TimeWindow
	stableWindow *aggregation.TimeWindow
}

var _ MetricClient = (*KPAMetricsClient)(nil)

// NewKPAMetricsClient initializes and returns a KPAMetricsClient with specified durations.
func NewKPAMetricsClient(fetcher MetricFetcher, stableDuration time.Duration, panicDuration time.Duration) *KPAMetricsClient {
	client := &KPAMetricsClient{
		fetcher:        fetcher,
		stableDuration: stableDuration,
		panicDuration:  panicDuration,
		granularity:    paGranularity,
		panicWindow:    aggregation.NewTimeWindow(panicDuration, paGranularity),
		stableWindow:   aggregation.NewTimeWindow(stableDuration, paGranularity),
	}
	return client
}

func (c *KPAMetricsClient) UpdateMetricIntoWindow(now time.Time, metricValue float64) error {
	c.panicWindow.Record(now, metricValue)
	c.stableWindow.Record(now, metricValue)
	return nil
}

func (c *KPAMetricsClient) UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error {
	return c.UpdateMetrics(now, metricKey, metricValues...)
}

func (c *KPAMetricsClient) UpdateMetrics(now time.Time, metricKey NamespaceNameMetric, metricValues ...float64) error {
	if len(metricValues) == 0 {
		return nil
	}

	// Calculate the total value from the retrieved metrics
	var sumMetricValue float64
	for _, metricValue := range metricValues {
		sumMetricValue += metricValue
	}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	// Update metrics into the window for tracking
	err := c.UpdateMetricIntoWindow(now, sumMetricValue)
	if err != nil {
		return err
	}
	klog.InfoS("Update pod list metrics", "metricKey", metricKey, "valueNum", len(metricValues), "timestamp", now, "metricValue", sumMetricValue)
	return nil
}

func (c *KPAMetricsClient) StableAndPanicMetrics(
	metricKey NamespaceNameMetric, now time.Time) (float64, float64, error) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	panicValue, err := c.panicWindow.Avg()
	if err != nil {
		return -1, -1, err
	}

	klog.InfoS("Get panicWindow", "metricKey", metricKey, "panicValue", panicValue, "panicWindow", c.panicWindow)

	stableValue, err := c.stableWindow.Avg()
	if err != nil {
		return -1, -1, err
	}

	klog.Infof("Get stableWindow: metricKey=%s, stableValue=%.2f, stableWindow=%v", metricKey, stableValue, c.stableWindow)
	return stableValue, panicValue, nil
}

func (c *KPAMetricsClient) GetPodContainerMetric(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (PodMetricsInfo, time.Time, error) {
	return GetPodContainerMetric(ctx, c.fetcher, pod, source)
}

func (c *KPAMetricsClient) GetMetricsFromPods(ctx context.Context, pods []corev1.Pod, source autoscalingv1alpha1.MetricSource) ([]float64, error) {
	return GetMetricsFromPods(ctx, c.fetcher, pods, source)
}

func (c *KPAMetricsClient) GetMetricFromSource(ctx context.Context, source autoscalingv1alpha1.MetricSource) (float64, error) {
	return GetMetricFromSource(ctx, c.fetcher, source)
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
	window *aggregation.TimeWindow
}

var _ MetricClient = (*APAMetricsClient)(nil)

// NewAPAMetricsClient initializes and returns a KPAMetricsClient with specified durations.
func NewAPAMetricsClient(fetcher MetricFetcher, duration time.Duration) *APAMetricsClient {
	client := &APAMetricsClient{
		fetcher:     fetcher,
		duration:    duration,
		granularity: paGranularity,
		window:      aggregation.NewTimeWindow(duration, paGranularity),
	}
	return client
}

func (c *APAMetricsClient) UpdateMetricIntoWindow(now time.Time, metricValue float64) error {
	c.window.Record(now, metricValue)
	return nil
}

func (c *APAMetricsClient) UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error {
	return c.UpdateMetrics(now, metricKey, metricValues...)
}

func (c *APAMetricsClient) UpdateMetrics(now time.Time, metricKey NamespaceNameMetric, metricValues ...float64) error {
	// Calculate the total value from the retrieved metrics
	var sumMetricValue float64
	for _, metricValue := range metricValues {
		sumMetricValue += metricValue
	}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	// Update metrics into the window for tracking
	err := c.UpdateMetricIntoWindow(now, sumMetricValue)
	if err != nil {
		return err
	}
	klog.InfoS("Update pod list metrics", "metricKey", metricKey, "valueNum", len(metricValues), "timestamp", now, "metricValue", sumMetricValue)
	return nil
}

func (c *APAMetricsClient) GetMetricValue(
	metricKey NamespaceNameMetric, now time.Time) (float64, error) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	metricValue, err := c.window.Avg()
	if err != nil {
		return -1, err
	}
	klog.InfoS("Get APA Window", "metricKey", metricKey, "value", metricValue)
	klog.V(4).InfoS("APA Window Details", "metricKey", metricKey, "value", metricValue, "window", c.window.String())

	return metricValue, nil
}

func (c *APAMetricsClient) GetPodContainerMetric(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (PodMetricsInfo, time.Time, error) {
	return GetPodContainerMetric(ctx, c.fetcher, pod, source)
}

func (c *APAMetricsClient) GetMetricsFromPods(ctx context.Context, pods []corev1.Pod, source autoscalingv1alpha1.MetricSource) ([]float64, error) {
	return GetMetricsFromPods(ctx, c.fetcher, pods, source)
}

func (c *APAMetricsClient) GetMetricFromSource(ctx context.Context, source autoscalingv1alpha1.MetricSource) (float64, error) {
	return GetMetricFromSource(ctx, c.fetcher, source)
}
