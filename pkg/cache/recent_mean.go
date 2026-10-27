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
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// recentMeanHistograms are the latency histograms that routers score pods by. Engines
// export them as cumulative histograms, so their Sum / Count is the mean over the pod's
// whole lifetime, which barely moves once a pod has served many requests (#3297).
var recentMeanHistograms = map[string]bool{
	metrics.RequestQueueTimeSeconds:     true,
	metrics.RequestPrefillTimeSeconds:   true,
	metrics.RequestDecodeTimeSeconds:    true,
	metrics.RequestInferenceTimeSeconds: true,
}

// RecentMeanMetricName is the pod-model metric under which the cache stores the mean of
// a latency histogram over about the last minute.
func RecentMeanMetricName(histogramName string) string {
	return histogramName + "_mean_1m"
}

// RecentHistogramMean returns the mean of a pod's latency histogram over about the last
// minute, or its lifetime mean when the pod had no observation in that window (or the
// cache has too little history yet).
func RecentHistogramMean(c MetricCache, podName, podNamespace, modelName, metricName string, lifetime *metrics.HistogramMetricValue) float64 {
	if recent, err := c.GetMetricValueByPodModel(podName, podNamespace, modelName, RecentMeanMetricName(metricName)); err == nil && recent != nil {
		return recent.GetSimpleValue()
	}
	return lifetime.GetMean()
}

// updateRecentMean stores the ~1-minute mean of a latency histogram under
// RecentMeanMetricName, and removes it when there is none, so readers fall back to the
// lifetime mean instead of reading a stale value.
func (c *Store) updateRecentMean(pod *Pod, model, metric string, metricValue metrics.MetricValue) {
	if !recentMeanHistograms[metric] {
		return
	}
	histogram := metricValue.GetHistogramValue()
	if histogram == nil {
		return
	}
	name := RecentMeanMetricName(metric)
	if mean, ok := c.calculateMean1m(pod, model, metric, histogram); ok {
		_ = c.updatePodRecord(pod, model, name, metrics.PodModelMetricScope, &metrics.SimpleMetricValue{Value: mean})
		return
	}
	pod.ModelMetrics.Delete(c.getPodModelMetricName(model, name))
}

// calculateMean1m returns the mean of the observations a cumulative histogram received
// over about the last minute, (Sum - Sum_base) / (Count - Count_base), with the same
// snapshots and window as calculateRate1m. ok is false when there is too little history,
// no observation in the window, or the histogram was reset by an engine restart.
func (c *Store) calculateMean1m(pod *Pod, modelName, metricName string, histogram *metrics.HistogramMetricValue) (mean float64, ok bool) {
	key := rateHistoryKey(pod, modelName, RecentMeanMetricName(metricName))
	now := time.Now()

	rateCalculator.mu.Lock()
	defer rateCalculator.mu.Unlock()

	history := appendSnapshot1m(rateCalculator.history[key],
		MetricSnapshot{Value: histogram.Sum, Count: histogram.Count, Timestamp: now})
	rateCalculator.history[key] = history

	if len(history) < 2 {
		return 0, false
	}
	base := baseSnapshot1m(history, now)
	if now.Sub(base.Timestamp).Seconds() < window1mMinElapsed {
		return 0, false
	}

	sum, count := histogram.Sum-base.Value, histogram.Count-base.Count
	if sum < 0 || count <= 0 {
		// No observation in the window, or the engine restarted and the histogram
		// started again from zero: either way, readers use the lifetime mean.
		return 0, false
	}
	return sum / count, true
}
