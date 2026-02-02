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

package cache

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

func getPodMetricPort(pod *Pod) int {
	if pod == nil || pod.Labels == nil {
		return defaultMetricPort
	}
	if v, ok := pod.Labels[MetricPortLabel]; ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		} else {
			klog.Warningf("Invalid value for label %s on pod %s/%s: %q. Using default port %d.", MetricPortLabel, pod.Namespace, pod.Name, v, defaultMetricPort)
		}
	}
	return defaultMetricPort
}

func getPodLabel(pod *Pod, labelName string) (string, error) {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		klog.V(4).Infof("No label %v name for pod %v, default to %v", labelName, pod.Name, defaultEngineLabelValue)
		err := fmt.Errorf("error executing query: no label %v found for pod %v", labelName, pod.Name)
		return "", err
	}
	return labelTarget, nil
}

func buildMetricLabels(pod *Pod, engineType string, model string) ([]string, []string) {
	labelNames := []string{
		"namespace",
		"pod",
		"model",
		"engine_type",
		"roleset",
		"role",
		"role_replica_index",
		"gateway_pod",
	}
	labelValues := []string{
		pod.Namespace,
		pod.Name,
		model,
		engineType,
		utils.GetPodEnv(pod.Pod, "ROLESET_NAME", ""),
		utils.GetPodEnv(pod.Pod, "ROLE_NAME", ""),
		utils.GetPodEnv(pod.Pod, "ROLE_REPLICA_INDEX", ""),
		os.Getenv("POD_NAME"),
	}
	return labelNames, labelValues
}

func shouldSkipMetric(podName string, metricName string) bool {
	if strings.Contains(podName, "prefill") && isDecodeOnlyMetric(metricName) {
		return true
	}
	if strings.Contains(podName, "decode") && isPrefillOnlyMetric(metricName) {
		return true
	}
	return false
}

func isPrefillOnlyMetric(metricName string) bool {
	switch metricName {
	case metrics.TimeToFirstTokenSeconds,
		metrics.RequestPrefillTimeSeconds,
		metrics.RequestPromptTokens:
		return true
	default:
		return false
	}
}

func isDecodeOnlyMetric(metricName string) bool {
	switch metricName {
	case metrics.TimePerOutputTokenSeconds,
		metrics.InterTokenLatencySeconds,
		metrics.RequestTimePerOutputTokenSeconds,
		metrics.RequestDecodeTimeSeconds,
		metrics.IterationTokensTotal,
		metrics.RequestGenerationTokens,
		metrics.RequestMaxNumGenerationTokens:
		return true
	default:
		return false
	}
}

// calculatePerSecondRate calculates the per-second rate for a given metric
// Returns the rate in units per second, or -1 if insufficient data
func (c *Store) calculatePerSecondRate(pod *Pod, modelName, metricName string, currentValue float64) float64 {
	key := fmt.Sprintf("%s/%s/%s", pod.Name, modelName, metricName)
	now := time.Now()

	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()

	// Get or create history for this metric
	history := rateCalculator.history[key]

	// Add current snapshot
	snapshot := MetricSnapshot{
		Value:     currentValue,
		Timestamp: now,
	}
	history = append(history, snapshot)

	// Clean up old snapshots
	history = cleanupOldSnapshots(history, now, rateCalculator.maxAge, rateCalculator.maxCount)
	rateCalculator.history[key] = history

	// Calculate rate if we have enough data
	if len(history) < 2 {
		return -1 // Not enough data points
	}

	// Use the previous snapshot for per-second calculation
	baseSnapshot := &history[len(history)-2]

	// Calculate rate
	timeDiff := now.Sub(baseSnapshot.Timestamp).Seconds()
	if timeDiff <= 0 {
		return -1
	}

	valueDiff := currentValue - baseSnapshot.Value
	if valueDiff < 0 {
		// Handle counter reset - assume it started from 0
		valueDiff = currentValue
	}

	// Return per-second rate
	ratePerSecond := valueDiff / timeDiff

	return ratePerSecond
}

// cleanupOldSnapshots removes snapshots that are too old or exceed the maximum count
func cleanupOldSnapshots(history []MetricSnapshot, now time.Time, maxAge time.Duration, maxCount int) []MetricSnapshot {
	cutoffTime := now.Add(-maxAge)

	// Remove snapshots older than maxAge
	var filtered []MetricSnapshot
	for _, snapshot := range history {
		if snapshot.Timestamp.After(cutoffTime) {
			filtered = append(filtered, snapshot)
		}
	}

	// Keep only the most recent maxCount snapshots
	if len(filtered) > maxCount {
		filtered = filtered[len(filtered)-maxCount:]
	}

	return filtered
}

func printMapTableAligned(message, podname string, m map[string]interface{}) {
	// Collect keys
	keys := make([]string, 0, len(m))
	maxKeyLen := 0
	for k := range m {
		keys = append(keys, k)
		if len(k) > maxKeyLen {
			maxKeyLen = len(k)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "%s pod: %s\n", message, podname)
	fmt.Fprintf(&b, "%-*s | %s\n", maxKeyLen, "metric_name", "value")
	fmt.Fprintln(&b, strings.Repeat("-", maxKeyLen+3+20))

	for _, k := range keys {
		fmt.Fprintf(&b, "%-*s | %12.4f\n", maxKeyLen, k, m[k].(float64))
	}

	klog.Info(b.String())
	fmt.Printf("\n")
}
