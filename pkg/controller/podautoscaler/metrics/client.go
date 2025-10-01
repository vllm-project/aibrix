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
	"fmt"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/klog/v2"
)

// AggregatorMetricsClient interface defines what aggregators need from metrics storage
// This interface should be consumed by aggregators but defined where it's implemented
type AggregatorMetricsClient interface {
	UpdateMetrics(now time.Time, metricKey types.MetricKey, config MetricsConfig, metricValues ...float64) error
	GetMetricValue(metricKey types.MetricKey, now time.Time) (float64, float64, error)
	GetTrendAnalysis(metricKey types.MetricKey, now time.Time) (direction float64, velocity float64, confidence float64)
	CalculatePodAwareConfidence(metricKey types.MetricKey, podCount int, now time.Time) float64
}

// NewNamespaceNameMetric creates a MetricKey based on the PodAutoscaler's metrics source.
// For consistency, it will return the corresponding MetricSource.
// Currently, it supports only a single metric source. In the future, this could be extended to handle multiple metric sources.
func NewNamespaceNameMetric(pa *autoscalingv1alpha1.PodAutoscaler) (types.MetricKey, autoscalingv1alpha1.MetricSource, error) {
	if len(pa.Spec.MetricsSources) != 1 {
		return types.MetricKey{}, autoscalingv1alpha1.MetricSource{}, fmt.Errorf("metrics sources must be 1, but got %d", len(pa.Spec.MetricsSources))
	}
	metricSource := pa.Spec.MetricsSources[0]
	return types.MetricKey{
		Namespace:   pa.Namespace,
		Name:        pa.Spec.ScaleTargetRef.Name,
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

// MetricsClient provides metric data storage (windows and history) for all scaling strategies
// It does NOT fetch metrics - fetching is done separately via MetricFetcherFactory
// IMPORTANT: This client is shared across multiple PodAutoscalers, so we need proper isolation
type MetricsClient struct {
	// Protects access to windows
	mu sync.RWMutex

	// Simplified structure: direct fields for stable and panic windows
	// metricKeyStr format: "paNamespace/paName/metricName"
	stableWindows map[string]*types.TimeWindow
	panicWindows  map[string]*types.TimeWindow // Optional, only used by KPA

	// Historical tracking for trend analysis
	stableHistory map[string]*types.MetricHistory
	panicHistory  map[string]*types.MetricHistory

	// Default granularity for time windows
	granularity time.Duration
}

// NewMetricsClient creates a new metrics client for storing metric windows and history
func NewMetricsClient(granularity time.Duration) *MetricsClient {
	return &MetricsClient{
		stableWindows: make(map[string]*types.TimeWindow),
		panicWindows:  make(map[string]*types.TimeWindow),
		stableHistory: make(map[string]*types.MetricHistory),
		panicHistory:  make(map[string]*types.MetricHistory),
		granularity:   granularity,
	}
}

// ensureWindowsForKey ensures windows exist for a specific metricKey
// This does NOT wipe existing data, only creates if missing
func (c *MetricsClient) ensureWindowsForKey(metricKeyStr string, config MetricsConfig) {
	// Check if stable window already exists
	if _, exists := c.stableWindows[metricKeyStr]; exists {
		// Windows already configured, don't recreate
		return
	}

	if _, exists := c.panicWindows[metricKeyStr]; exists {
		// Windows already configured, don't recreate
		return
	}

	// Determine window durations
	stableWindowDuration := config.StableWindow
	if stableWindowDuration == 0 {
		stableWindowDuration = 120 * time.Second // Default
	}

	panicWindowDuration := config.PanicWindow
	if panicWindowDuration == 0 {
		panicWindowDuration = 15 * time.Second // Default
	}

	// Always create stable window and panic window (KPA and APA will decide whether to use it or not)
	c.stableWindows[metricKeyStr] = types.NewTimeWindow(stableWindowDuration, c.granularity)
	c.stableHistory[metricKeyStr] = types.NewMetricHistory(stableWindowDuration * 10)
	c.panicWindows[metricKeyStr] = types.NewTimeWindow(panicWindowDuration, c.granularity)
	c.panicHistory[metricKeyStr] = types.NewMetricHistory(panicWindowDuration * 10)
}

// UpdateMetrics records metrics to all configured windows for the given metricKey
// If windows don't exist for this metricKey, they are auto-initialized with the provided config
func (c *MetricsClient) UpdateMetrics(now time.Time, metricKey types.MetricKey, config MetricsConfig, metricValues ...float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	metricKeyStr := metricKey.String()

	// Auto-initialize if needed, using the provided config from PodAutoscaler
	if _, exists := c.stableWindows[metricKeyStr]; !exists {
		klog.V(4).InfoS("Auto-initializing window for metricKey with config",
			"metricKey", metricKeyStr,
			"stableWindow", config.StableWindow,
			"panicWindow", config.PanicWindow)
		c.ensureWindowsForKey(metricKeyStr, config)
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

	// Record in stable window (always present)
	if stableWindow := c.stableWindows[metricKeyStr]; stableWindow != nil {
		stableWindow.Record(now, avg)
		if stableHist := c.stableHistory[metricKeyStr]; stableHist != nil {
			stableHist.Add(avg, now)
		}
	}

	// Record in panic window if it exists (KPA only)
	if panicWindow := c.panicWindows[metricKeyStr]; panicWindow != nil {
		panicWindow.Record(now, avg)
		if panicHist := c.panicHistory[metricKeyStr]; panicHist != nil {
			panicHist.Add(avg, now)
		}
	}

	klog.V(4).InfoS("Recorded metric", "metricKey", metricKeyStr, "value", avg, "time", now,
		"stableWindowSize", c.stableWindows[metricKeyStr].Size(),
		"panicWindowSize", func() int {
			if c.panicWindows[metricKeyStr] != nil {
				return c.panicWindows[metricKeyStr].Size()
			}
			return 0
		}())

	return nil
}

// GetMetricValue returns the metric value from the stable window for a specific metricKey
// Both KPA and APA use the stable window (APA doesn't use panic window)
func (c *MetricsClient) GetMetricValue(metricKey types.MetricKey, now time.Time) (float64, float64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metricKeyStr := metricKey.String()

	stableWindow := c.stableWindows[metricKeyStr]
	if stableWindow == nil {
		return 0, 0, fmt.Errorf("no stable window configured for metricKey: %s", metricKeyStr)
	}

	panicWindow := c.panicWindows[metricKeyStr]
	if panicWindow == nil {
		return 0, 0, fmt.Errorf("no panic window configured for metricKey: %s", metricKeyStr)
	}

	stableValue, err := stableWindow.Avg()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get stable value: %w", err)
	}
	panicValue, err := panicWindow.Avg()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get panic value: %w", err)
	}

	klog.V(4).InfoS("[MetricClient] metrics window", "metricKey", metricKeyStr,
		"size", stableWindow.Size(), "stableValue", stableValue, "panicValue", panicValue)

	return stableValue, panicValue, nil
}

// MetricsConfig holds configuration for metrics collection
type MetricsConfig struct {
	StableWindow time.Duration // Shared by APA and KPA
	PanicWindow  time.Duration // For KPA
	Window       time.Duration // For APA
}

// GetUnifiedStats returns stats for both stable and panic windows for a specific metricKey
func (c *MetricsClient) GetUnifiedStats(metricKey types.MetricKey, now time.Time) (stableStats, panicStats types.WindowStats, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metricKeyStr := metricKey.String()

	// Get stable window stats (always present for both KPA and APA)
	if stableWindow := c.stableWindows[metricKeyStr]; stableWindow != nil {
		stableStats = c.getStatsFromWindow(stableWindow)
	}

	// Get panic window stats (only present for KPA)
	if panicWindow := c.panicWindows[metricKeyStr]; panicWindow != nil {
		panicStats = c.getStatsFromWindow(panicWindow)
	}

	if stableStats.DataPoints == 0 {
		return stableStats, panicStats, fmt.Errorf("no metrics available for metricKey: %s", metricKeyStr)
	}

	return stableStats, panicStats, nil
}

// getStatsFromWindow extracts stats from a time window
func (c *MetricsClient) getStatsFromWindow(window *types.TimeWindow) types.WindowStats {
	var stats types.WindowStats
	stats.DataPoints = window.Size()

	if stats.DataPoints == 0 {
		return stats
	}

	avg, _ := window.Avg()
	minVal, _ := window.Min()
	maxVal, _ := window.Max()

	stats.Mean = avg
	stats.Min = minVal
	stats.Max = maxVal

	stats.LastUpdate = time.Now()

	return stats
}

// GetEnhancedStats returns both window and historical statistics for a specific metricKey
func (c *MetricsClient) GetEnhancedStats(metricKey types.MetricKey, now time.Time) (windowStats, historyStats types.WindowStats, err error) {
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

	metricKeyStr := metricKey.String()

	// Get historical stats from stable history (used by both KPA and APA)
	if history := c.stableHistory[metricKeyStr]; history != nil {
		historyStats = history.GetStats(now)
	}

	return windowStats, historyStats, nil
}

// TODO(Jeffwan): support tend and condidence later

// GetTrendAnalysis calculates trend direction and velocity (stubbed - returns zeros)
func (c *MetricsClient) GetTrendAnalysis(metricKey types.MetricKey, now time.Time) (direction float64, velocity float64, confidence float64) {
	// Stubbed out - trend analysis not needed for current implementation
	return 0, 0, 0
}

// CalculatePodAwareConfidence combines pod count with statistical confidence (stubbed - returns 0)
func (c *MetricsClient) CalculatePodAwareConfidence(metricKey types.MetricKey, podCount int, now time.Time) float64 {
	// Stubbed out - confidence calculation not needed for current implementation
	return 0
}

// Compile-time interface verification
var _ AggregatorMetricsClient = (*MetricsClient)(nil)
