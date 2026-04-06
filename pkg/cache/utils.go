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

func mergeLabelPairs(primaryNames, primaryValues, secondaryNames, secondaryValues []string) ([]string, []string) {
	pLen := len(primaryNames)
	if len(primaryValues) < pLen {
		klog.Warningf("primary labels length mismatch: names=%d, values=%d", pLen, len(primaryValues))
		pLen = len(primaryValues)
	}
	sLen := len(secondaryNames)
	if len(secondaryValues) < sLen {
		klog.Warningf("secondary labels length mismatch: names=%d, values=%d", sLen, len(secondaryValues))
		sLen = len(secondaryValues)
	}

	secondaryMap := make(map[string]string, sLen)
	secondaryOrder := make([]string, 0, sLen)
	for i := 0; i < sLen; i++ {
		n := secondaryNames[i]
		if n == "" {
			continue
		}
		if _, exists := secondaryMap[n]; !exists {
			secondaryOrder = append(secondaryOrder, n)
		}
		secondaryMap[n] = secondaryValues[i] // last-wins
	}

	outNames := make([]string, 0, pLen+len(secondaryOrder))
	outValues := make([]string, 0, pLen+len(secondaryOrder))
	seen := make(map[string]struct{}, pLen+len(secondaryOrder))

	// primary first, but allow secondary override
	for i := 0; i < pLen; i++ {
		n := primaryNames[i]
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		v := primaryValues[i]
		if sv, ok := secondaryMap[n]; ok {
			v = sv
		}
		outNames = append(outNames, n)
		outValues = append(outValues, v)
	}

	// then add secondary-only labels (use map value to respect last-wins)
	for _, n := range secondaryOrder {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		outNames = append(outNames, n)
		outValues = append(outValues, secondaryMap[n])
	}

	return outNames, outValues
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

// calculateRate1m calculates the per-second rate of a monotonically increasing counter
// over an approximate 1-minute window. It is designed for gateway-tracked counters
// (e.g. completedRequests) that are available even when engine metrics are not.
//
// Snapshots are throttled to ~5-second intervals so the 50ms metric refresh loop
// does not flood the history buffer. The baseline is the snapshot closest to
// 1 minute ago; if less than 1 minute of history exists, the oldest available
// snapshot is used. Returns -1 when insufficient data is available.
func (c *Store) calculateRate1m(pod *Pod, metricName string, currentValue float64) float64 {
	key := fmt.Sprintf("%s//%s", pod.Name, metricName)
	now := time.Now()

	const (
		snapshotInterval = 5 * time.Second // throttle to avoid snapshot flood
		windowTarget     = 1 * time.Minute // desired look-back window
		historyMaxAge    = 3 * time.Minute // keep 3 minutes of history
		historyMaxCount  = 36              // 5s × 36 = 3 minutes
		minElapsed       = 10.0            // seconds; discard rates with too little data
	)

	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()

	history := rateCalculator.history[key]

	// Only append a new snapshot when enough time has elapsed since the last one.
	if len(history) == 0 || now.Sub(history[len(history)-1].Timestamp) >= snapshotInterval {
		history = append(history, MetricSnapshot{Value: currentValue, Timestamp: now})
	}
	history = cleanupOldSnapshots(history, now, historyMaxAge, historyMaxCount)
	rateCalculator.history[key] = history

	if len(history) < 2 {
		return -1
	}

	// Find the snapshot whose timestamp is closest to (now - 1 minute).
	// If we have less than 1 minute of history, this naturally falls back
	// to the oldest snapshot in the buffer.
	target := now.Add(-windowTarget)
	base := &history[0]
	for i := 1; i < len(history)-1; i++ {
		if absDuration(history[i].Timestamp.Sub(target)) < absDuration(base.Timestamp.Sub(target)) {
			base = &history[i]
		}
	}

	elapsed := now.Sub(base.Timestamp).Seconds()
	if elapsed < minElapsed {
		return -1
	}

	delta := currentValue - base.Value
	if delta < 0 {
		// Counter should never decrease; treat as a reset and use current value as delta.
		delta = currentValue
	}

	return delta / elapsed
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
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
