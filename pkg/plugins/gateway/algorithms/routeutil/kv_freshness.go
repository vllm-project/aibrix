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

package routeutil

import (
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var kvMetricRejections = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "aibrix_routing_kv_metric_rejections_total",
	Help: "KV metric reads rejected by routing freshness checks (not distinct pods or requests).",
}, []string{"reason", "metric"})

var rejectionCounters = func() map[string]prometheus.Counter {
	counters := make(map[string]prometheus.Counter)
	for _, reason := range []string{"stale", "missing", "invalid", "lookup_error"} {
		for _, metric := range []string{"gpu", "cpu", "other"} {
			counters[reason+":"+metric] = kvMetricRejections.WithLabelValues(reason, metric)
		}
	}
	return counters
}()

var legacyKVCacheWarning sync.Once

// FreshKVUsage reads a KV metric with collection-age validation. Only a missing
// CPU metric contributes zero: engines without CPU offload never report it.
// Callers decide whether an unavailable metric excludes a score or is neutral.
// A collection completed after now was captured is fresh, not invalid.
func FreshKVUsage(c cache.Cache, pod *v1.Pod, model, metric string, now time.Time, threshold time.Duration) (float64, bool) {
	value, ok, _ := ReadKVUsage(c, pod, model, metric, now, threshold)
	return value, ok
}

// ReadKVUsage also returns the bounded rejection reason for routing policy decisions.
func ReadKVUsage(c cache.Cache, pod *v1.Pod, model, metric string, now time.Time, threshold time.Duration) (float64, bool, string) {
	if records, ok := c.(cache.MetricRecordCache); ok {
		record, err := records.GetMetricRecordByPodModel(pod.Name, pod.Namespace, model, metric)
		reason := ""
		switch {
		case err != nil:
			var missing *cache.MetricNotFoundError
			if errors.As(err, &missing) {
				if metric == metrics.CPUCacheUsagePerc {
					return 0, true, ""
				}
				reason = "missing"
			} else {
				reason = "lookup_error"
			}
		case record.Value == nil || record.Timestamp.IsZero():
			reason = "invalid"
		case now.Sub(record.Timestamp) > threshold:
			reason = "stale"
		default:
			return record.Value.GetSimpleValue(), true, ""
		}
		// Bound label cardinality even if a caller passes a custom metric name.
		label := "other"
		switch metric {
		case metrics.KVCacheUsagePerc:
			label = "gpu"
		case metrics.CPUCacheUsagePerc:
			label = "cpu"
		}
		rejectionCounters[reason+":"+label].Inc()
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("KV routing metric unavailable", "pod", klog.KObj(pod),
				"metric", metric, "reason", reason, "age", now.Sub(record.Timestamp),
				"threshold", threshold, "error", err)
		}
		return 0, false, reason
	}
	// External value-only adapters retain historical semantics, but cannot
	// enforce freshness. Warn once per process so this degradation is visible.
	legacyKVCacheWarning.Do(func() {
		klog.Warning("KV routing cache does not implement MetricRecordCache; freshness checks are unavailable")
	})
	value, err := c.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metric)
	if err != nil {
		var missing *cache.MetricNotFoundError
		if metric == metrics.CPUCacheUsagePerc && errors.As(err, &missing) {
			return 0, true, ""
		}
		return 0, false, "lookup_error"
	}
	if value == nil {
		return 0, false, "invalid"
	}
	return value.GetSimpleValue(), true, ""
}
