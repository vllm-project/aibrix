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
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
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

// shouldSkipMetric reports whether a PD pod exposes a metric that only makes
// sense for the other role. The role comes from pdRole, the same detection
// the throughput rates use.
func shouldSkipMetric(pod *v1.Pod, metricName string) bool {
	switch pdRole(pod) {
	case "prefill":
		return isDecodeOnlyMetric(metricName)
	case "decode":
		return isPrefillOnlyMetric(metricName)
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
	key := rateHistoryKey(pod, modelName, metricName)
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
// Snapshots are throttled to ~5-second intervals so the metric refresh loop
// does not flood the history buffer. The baseline is the snapshot closest to
// 1 minute ago; if less than 1 minute of history exists, the oldest available
// snapshot is used. Returns -1 when insufficient data is available.
func (c *Store) calculateRate1m(pod *Pod, metricName string, currentValue float64) float64 {
	key := rateHistoryKey(pod, "", metricName)
	now := time.Now()

	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()

	history := appendSnapshot1m(rateCalculator.history[key], MetricSnapshot{Value: currentValue, Timestamp: now})
	rateCalculator.history[key] = history

	if len(history) < 2 {
		return -1
	}

	base := baseSnapshot1m(history, now)
	elapsed := now.Sub(base.Timestamp).Seconds()
	if elapsed < window1mMinElapsed {
		return -1
	}

	delta := currentValue - base.Value
	if delta < 0 {
		// Counter should never decrease; treat as a reset and use current value as delta.
		delta = currentValue
	}

	return delta / elapsed
}

// calculateDeltas1m returns how much each of several monotonically increasing counters grew over
// the same approximate 1-minute window as calculateRate1m. ok is false when there is not enough
// history yet.
//
// The counters are snapshotted together, at the same instants and under one lock, and every delta
// is taken against the same baseline snapshot. Independent calculateRate1m histories could each
// admit their throttled snapshot on a different refresh tick, and values derived from two of them
// (a ratio, a count that gates it) would then describe different windows. names identify the
// series; their histories are only written here, as one group. names and values must be non-empty
// and of the same length; otherwise nothing is recorded and ok is false.
func (c *Store) calculateDeltas1m(pod *Pod, names []string, values []float64) (deltas []float64, ok bool) {
	if len(names) == 0 || len(names) != len(values) {
		return nil, false
	}
	keys := make([]string, len(names))
	for i, name := range names {
		keys[i] = rateHistoryKey(pod, "", name)
	}
	now := time.Now()

	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()

	hists := make([][]MetricSnapshot, len(keys))
	for i, key := range keys {
		hists[i] = rateCalculator.history[key]
		if len(hists[i]) != len(hists[0]) {
			// Never written apart, so this should not happen. Start the group over rather than
			// pair snapshots of different instants.
			for j := range hists {
				hists[j] = nil
			}
			break
		}
	}
	if len(hists[0]) == 0 || now.Sub(hists[0][len(hists[0])-1].Timestamp) >= window1mSnapshotInterval {
		// The histories only change when a snapshot is admitted, so that is the only time they are
		// pruned and stored: cleanupOldSnapshots copies the history, and between two snapshots
		// (at most window1mSnapshotInterval) nothing new can age out of a 3-minute history that
		// matters to a 1-minute baseline.
		for i, key := range keys {
			hists[i] = append(hists[i], MetricSnapshot{Value: values[i], Timestamp: now})
			// Same timestamps in every series, so the same snapshots are dropped from each.
			hists[i] = cleanupOldSnapshots(hists[i], now, window1mHistoryMaxAge, window1mHistoryMaxCount)
			rateCalculator.history[key] = hists[i]
		}
	}

	if len(hists[0]) < 2 {
		return nil, false
	}
	base := baseSnapshotIndex1m(hists[0], now)
	if now.Sub(hists[0][base].Timestamp).Seconds() < window1mMinElapsed {
		return nil, false
	}
	deltas = make([]float64, len(values))
	reset := false
	for i := range values {
		deltas[i] = values[i] - hists[i][base].Value
		reset = reset || deltas[i] < 0
	}
	if reset {
		// Counters never decrease; treat it as a reset of the group and use the current values.
		copy(deltas, values)
	}
	return deltas, true
}

// The ~1-minute window shared by calculateRate1m, calculateDeltas1m and calculateMean1m.
const (
	window1mSnapshotInterval = 5 * time.Second // throttle to avoid snapshot flood
	window1mTarget           = 1 * time.Minute // desired look-back window; load-balance's capacity warm-up default (loadBalanceCapacityWarmupDefault) matches it
	window1mHistoryMaxAge    = 3 * time.Minute // keep 3 minutes of history
	window1mHistoryMaxCount  = 36              // 5s × 36 = 3 minutes
	window1mMinElapsed       = 10.0            // seconds; discard results with too little data
)

// appendSnapshot1m appends snapshot unless the last one is more recent than the snapshot
// interval, then drops the snapshots that fall out of the history.
func appendSnapshot1m(history []MetricSnapshot, snapshot MetricSnapshot) []MetricSnapshot {
	if len(history) == 0 || snapshot.Timestamp.Sub(history[len(history)-1].Timestamp) >= window1mSnapshotInterval {
		history = append(history, snapshot)
	}
	return cleanupOldSnapshots(history, snapshot.Timestamp, window1mHistoryMaxAge, window1mHistoryMaxCount)
}

// baseSnapshot1m returns the snapshot whose timestamp is closest to (now - 1 minute).
// If we have less than 1 minute of history, this naturally falls back to the oldest
// snapshot in the buffer. history must hold at least 2 snapshots.
func baseSnapshot1m(history []MetricSnapshot, now time.Time) *MetricSnapshot {
	return &history[baseSnapshotIndex1m(history, now)]
}

// baseSnapshotIndex1m is the index of the snapshot baseSnapshot1m returns. calculateDeltas1m uses
// it to read the same position in several histories that hold snapshots of the same instants.
func baseSnapshotIndex1m(history []MetricSnapshot, now time.Time) int {
	target := now.Add(-window1mTarget)
	base := 0
	for i := 1; i < len(history)-1; i++ {
		if absDuration(history[i].Timestamp.Sub(target)) < absDuration(history[base].Timestamp.Sub(target)) {
			base = i
		}
	}
	return base
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
