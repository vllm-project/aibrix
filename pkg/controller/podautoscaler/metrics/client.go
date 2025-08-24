/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	apitypes "k8s.io/apimachinery/pkg/types"
)

// AggregatorMetricsClient interface defines what aggregators need from metrics storage
// This interface should be consumed by aggregators but defined where it's implemented
type AggregatorMetricsClient interface {
	UpdateMetrics(now time.Time, metricKey interface{}, metricValues ...float64) error
	StableAndPanicMetrics(metricKey interface{}, now time.Time) (float64, float64, error)
	GetMetricValue(metricKey interface{}, now time.Time) (float64, error)
	GetTrendAnalysis(metricKey interface{}, now time.Time) (direction float64, velocity float64, confidence float64)
	CalculatePodAwareConfidence(metricKey interface{}, podCount int, now time.Time) float64
}

// NamespaceNameMetric contains the namespace, name and the metric name
type NamespaceNameMetric struct {
	apitypes.NamespacedName // target deployment namespace and name
	MetricName              string
	PaNamespace             string
	PaName                  string
}

// NewNamespaceNameMetric creates a NamespaceNameMetric based on the PodAutoscaler's metrics source.
// For consistency, it will return the corresponding MetricSource.
// Currently, it supports only a single metric source. In the future, this could be extended to handle multiple metric sources.
func NewNamespaceNameMetric(pa *autoscalingv1alpha1.PodAutoscaler) (NamespaceNameMetric, autoscalingv1alpha1.MetricSource, error) {
	if len(pa.Spec.MetricsSources) != 1 {
		return NamespaceNameMetric{}, autoscalingv1alpha1.MetricSource{}, fmt.Errorf("metrics sources must be 1, but got %d", len(pa.Spec.MetricsSources))
	}
	metricSource := pa.Spec.MetricsSources[0]
	return NamespaceNameMetric{
		NamespacedName: apitypes.NamespacedName{
			Namespace: pa.Namespace,
			Name:      pa.Spec.ScaleTargetRef.Name,
		},
		MetricName:  metricSource.TargetMetric,
		PaNamespace: pa.Namespace,
		PaName:      pa.Name,
	}, metricSource, nil
}

// PodMetric contains pod metric value (the metric values are expected to be the metric as a milli-value)
type PodMetric struct {
	Timestamp time.Time
	// kubernetes metrics return this value.
	Window          time.Duration
	Value           int64
	MetricsName     string
	containerPort   int32
	ScaleObjectName string
}

// PodMetricsInfo contains pod metrics as a map from pod names to PodMetricsInfo
type PodMetricsInfo map[string]PodMetric

// MetricsClient provides a single metrics client for all scaling strategies
// It can be configured with multiple time windows as needed
type MetricsClient struct {
	fetcher MetricFetcher

	// Protects access to windows map
	mu sync.RWMutex

	// Named windows for different purposes (e.g., "stable", "panic", "default")
	windows map[string]*types.TimeWindow

	// Historical tracking for trend analysis
	history map[string]*types.MetricHistory

	// Default granularity for time windows
	granularity time.Duration
}

// NewMetricsClient creates a new metrics client
func NewMetricsClient(fetcher MetricFetcher, granularity time.Duration) *MetricsClient {
	return &MetricsClient{
		fetcher:     fetcher,
		windows:     make(map[string]*types.TimeWindow),
		history:     make(map[string]*types.MetricHistory),
		granularity: granularity,
	}
}

// AddWindow adds a named time window with specified duration
func (c *MetricsClient) AddWindow(name string, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows[name] = types.NewTimeWindow(duration, c.granularity)
}

// ConfigureForStrategy sets up the client for a specific scaling strategy
func (c *MetricsClient) ConfigureForStrategy(strategy autoscalingv1alpha1.ScalingStrategyType, config MetricsConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Clear existing windows and history
	c.windows = make(map[string]*types.TimeWindow)
	c.history = make(map[string]*types.MetricHistory)

	switch strategy {
	case autoscalingv1alpha1.KPA:
		// KPA needs both stable and panic windows
		c.windows["stable"] = types.NewTimeWindow(config.StableWindow, c.granularity)
		c.windows["panic"] = types.NewTimeWindow(config.PanicWindow, c.granularity)
		// Create longer history for trend analysis (10x window duration)
		c.history["stable"] = types.NewMetricHistory(config.StableWindow * 10)
		c.history["panic"] = types.NewMetricHistory(config.PanicWindow * 10)
	case autoscalingv1alpha1.APA:
		// APA needs a single window
		c.windows["default"] = types.NewTimeWindow(config.Window, c.granularity)
		c.history["default"] = types.NewMetricHistory(config.Window * 10)
	default:
		// Default configuration
		c.windows["default"] = types.NewTimeWindow(60*time.Second, c.granularity)
		c.history["default"] = types.NewMetricHistory(600 * time.Second)
	}
}

// UpdateMetrics records metrics to all configured windows
func (c *MetricsClient) UpdateMetrics(now time.Time, metricKey interface{}, metricValues ...float64) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.windows) == 0 {
		return fmt.Errorf("no windows configured")
	}

	if len(metricValues) == 0 {
		return nil
	}

	// Calculate average of metric values
	var sum float64
	for _, v := range metricValues {
		sum += v
	}
	avg := sum / float64(len(metricValues))

	// Record in all windows and history
	for name, window := range c.windows {
		window.Record(now, avg)
		klog.V(4).InfoS("Recorded metric", "window", name, "value", avg, "time", now)

		// Also record in corresponding history for trend analysis
		if history, exists := c.history[name]; exists {
			history.Add(avg, now)
		}
	}

	return nil
}

// GetWindowValue returns the current value for a named window
func (c *MetricsClient) GetWindowValue(name string, now time.Time) (float64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	window, exists := c.windows[name]
	if !exists {
		return 0, fmt.Errorf("window %s not found", name)
	}

	value, err := window.Avg()
	if err != nil {
		return 0, err
	}
	return value, nil
}

// GetPodContainerMetric implements MetricClient interface
func (c *MetricsClient) GetPodContainerMetric(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (PodMetricsInfo, time.Time, error) {
	// Delegate to fetcher
	value, err := c.fetcher.FetchPodMetrics(ctx, pod, source)
	if err != nil {
		return nil, time.Time{}, err
	}

	info := PodMetricsInfo{
		pod.Name: PodMetric{
			Timestamp:   time.Now(),
			Value:       int64(value * 1000), // Convert to milli-value
			MetricsName: source.TargetMetric,
		},
	}

	return info, time.Now(), nil
}

// GetMetricsFromPods implements MetricClient interface
func (c *MetricsClient) GetMetricsFromPods(ctx context.Context, pods []corev1.Pod, source autoscalingv1alpha1.MetricSource) ([]float64, error) {
	values := make([]float64, 0, len(pods))
	for _, pod := range pods {
		value, err := c.fetcher.FetchPodMetrics(ctx, pod, source)
		if err != nil {
			klog.V(4).InfoS("Failed to fetch pod metrics", "pod", pod.Name, "error", err)
			continue
		}
		values = append(values, value)
	}
	return values, nil
}

// GetMetricFromSource implements MetricClient interface
func (c *MetricsClient) GetMetricFromSource(ctx context.Context, source autoscalingv1alpha1.MetricSource) (float64, error) {
	// For external metrics, we need to create a dummy pod since the interface requires one
	// This is acceptable since external metrics don't actually use pod information
	dummyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-source",
			Namespace: "default",
		},
	}
	return c.fetcher.FetchPodMetrics(ctx, dummyPod, source)
}

// UpdatePodListMetric implements MetricClient interface (deprecated)
func (c *MetricsClient) UpdatePodListMetric(metricValues []float64, metricKey NamespaceNameMetric, now time.Time) error {
	return c.UpdateMetrics(now, metricKey, metricValues...)
}

// StableAndPanicMetrics returns stable and panic window metrics for KPA
func (c *MetricsClient) StableAndPanicMetrics(metricKey interface{}, now time.Time) (float64, float64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stableWindow, hasStable := c.windows["stable"]
	panicWindow, hasPanic := c.windows["panic"]

	if !hasStable || !hasPanic {
		return 0, 0, fmt.Errorf("stable and panic windows not configured")
	}

	stableValue, err := stableWindow.Avg()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get stable value: %w", err)
	}
	panicValue, err := panicWindow.Avg()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get panic value: %w", err)
	}

	return stableValue, panicValue, nil
}

// GetMetricValue returns the metric value from the default or first window
func (c *MetricsClient) GetMetricValue(metricKey interface{}, now time.Time) (float64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try default window first
	if window, exists := c.windows["default"]; exists {
		value, err := window.Avg()
		if err != nil {
			return 0, err
		}
		return value, nil
	}

	// Fall back to stable window for KPA
	if window, exists := c.windows["stable"]; exists {
		value, err := window.Avg()
		if err != nil {
			return 0, err
		}
		return value, nil
	}

	// Use any available window
	for _, window := range c.windows {
		value, err := window.Avg()
		if err != nil {
			return 0, err
		}
		return value, nil
	}

	return 0, fmt.Errorf("no windows configured")
}

// MetricsConfig holds configuration for metrics collection
type MetricsConfig struct {
	StableWindow time.Duration // For KPA
	PanicWindow  time.Duration // For KPA
	Window       time.Duration // For APA
}

// GetUnifiedStats returns stats for both stable and panic windows
// APA can use just the stable stats, KPA can use both
func (c *MetricsClient) GetUnifiedStats(metricKey interface{}, now time.Time) (stableStats, panicStats types.WindowStats, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// For KPA: return both stable and panic stats
	if stableWindow, exists := c.windows["stable"]; exists {
		stableStats = c.getStatsFromWindow(stableWindow, "stable")
	}
	if panicWindow, exists := c.windows["panic"]; exists {
		panicStats = c.getStatsFromWindow(panicWindow, "panic")
	}

	// For APA: use default window as stable, panic is empty
	if defaultWindow, exists := c.windows["default"]; exists && stableStats.DataPoints == 0 {
		stableStats = c.getStatsFromWindow(defaultWindow, "default")
		// Panic stats remain empty for APA
	}

	if stableStats.DataPoints == 0 {
		return stableStats, panicStats, fmt.Errorf("no metrics available")
	}

	return stableStats, panicStats, nil
}

// getStatsFromWindow extracts stats from a time window
func (c *MetricsClient) getStatsFromWindow(window *types.TimeWindow, name string) types.WindowStats {
	var stats types.WindowStats
	stats.DataPoints = window.Size()

	if stats.DataPoints == 0 {
		return stats
	}

	avg, _ := window.Avg()
	min, _ := window.Min()
	max, _ := window.Max()

	stats.Mean = avg
	stats.Min = min
	stats.Max = max

	stats.LastUpdate = time.Now()

	return stats
}

// GetEnhancedStats returns both window and historical statistics
func (c *MetricsClient) GetEnhancedStats(metricKey interface{}, now time.Time) (windowStats, historyStats types.WindowStats, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Get current window stats (for immediate scaling decisions)
	stableStats, panicStats, err := c.GetUnifiedStats(metricKey, now)
	if err != nil {
		return types.WindowStats{}, types.WindowStats{}, err
	}

	// For KPA: use stable window stats
	windowStats = stableStats
	if panicStats.DataPoints > 0 {
		windowStats = panicStats // Prefer panic if available
	}

	// Get historical stats for trend analysis
	for _, windowName := range []string{"stable", "default"} {
		if history, exists := c.history[windowName]; exists {
			historyStats = history.GetStats(now)
			break
		}
	}

	return windowStats, historyStats, nil
}

// GetTrendAnalysis calculates trend direction and velocity
func (c *MetricsClient) GetTrendAnalysis(metricKey interface{}, now time.Time) (direction float64, velocity float64, confidence float64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try to get history from stable window first, then default
	for _, windowName := range []string{"stable", "default"} {
		if history, exists := c.history[windowName]; exists {
			stats := history.GetStats(now)
			if stats.DataPoints < 3 {
				continue // Need at least 3 points for trend
			}

			// Calculate trend direction: positive = increasing load
			if stats.Variance > 0 {
				direction = (stats.Max - stats.Min) / stats.Mean
				velocity = stats.StdDev / stats.Mean                       // Rate of change relative to mean
				confidence = minFloat(1.0, float64(stats.DataPoints)/10.0) // More points = higher confidence
			}
			break
		}
	}

	return direction, velocity, confidence
}

// CalculatePodAwareConfidence combines pod count with statistical confidence
func (c *MetricsClient) CalculatePodAwareConfidence(metricKey interface{}, podCount int, now time.Time) float64 {
	_, historyStats, err := c.GetEnhancedStats(metricKey, now)
	if err != nil {
		return 0.5 // Default confidence
	}

	// Base confidence from data quality
	baseConfidence := 0.5
	if historyStats.DataPoints > 0 {
		// More data points = higher confidence
		dataConfidence := minFloat(1.0, float64(historyStats.DataPoints)/20.0)

		// Low variance = higher confidence
		varianceConfidence := 1.0
		if historyStats.Mean > 0 && historyStats.Variance > 0 {
			coefficientOfVariation := historyStats.StdDev / historyStats.Mean
			varianceConfidence = maxFloat(0.1, 1.0-coefficientOfVariation)
		}

		baseConfidence = (dataConfidence + varianceConfidence) / 2.0
	}

	// Pod count factor: more pods = higher confidence (diminishing returns)
	podConfidence := minFloat(1.0, float64(podCount)/5.0) // Confidence plateaus at 5+ pods

	// Combined confidence
	return minFloat(1.0, (baseConfidence*0.7)+(podConfidence*0.3))
}

// Compile-time interface verification
var _ AggregatorMetricsClient = (*MetricsClient)(nil)
