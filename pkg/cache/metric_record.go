/*
Copyright 2026 The Aibrix Team.

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
	"k8s.io/klog/v2"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// MetricRecord keeps a value and its local successful-update time in one cache slot.
// Timestamp measures collection freshness, not the engine's own sampling window.
type MetricRecord struct {
	Value     metrics.MetricValue
	Timestamp time.Time
}

// MetricRecordCache is an optional extension for freshness-sensitive readers.
// Value-only adapters retain legacy behavior, with a one-time warning in the KV reader.
// This does not preserve source compatibility for users of the exported Pod metric maps.
type MetricRecordCache interface {
	GetMetricRecordByPod(podName, podNamespace, metricName string) (MetricRecord, error)
	GetMetricRecordByPodModel(podName, podNamespace, modelName, metricName string) (MetricRecord, error)
}

var _ MetricRecordCache = (*Store)(nil)

var routingMetricStaleThreshold = loadRoutingMetricStaleThreshold()

// RoutingMetricStaleThreshold returns the gateway-local KV scoring age limit,
// configured at process startup, like the collection refresh interval.
func RoutingMetricStaleThreshold() time.Duration { return routingMetricStaleThreshold }

func loadRoutingMetricStaleThreshold() time.Duration {
	fallback := max(5*time.Second, 3*podMetricRefreshInterval, podMetricsFetchTimeout+podMetricRefreshInterval)
	// LoadEnvInt already rejects zero, negative and malformed overrides.
	ms := utils.LoadEnvInt("AIBRIX_ROUTING_METRIC_STALE_THRESHOLD_MS", int(fallback/time.Millisecond))
	source := "default"
	if raw := os.Getenv("AIBRIX_ROUTING_METRIC_STALE_THRESHOLD_MS"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && parsed > 0 && parsed <= math.MaxInt64/int64(time.Millisecond) {
			source = "env"
		}
	}
	value := fallback
	if int64(ms) <= math.MaxInt64/int64(time.Millisecond) {
		value = time.Duration(ms) * time.Millisecond
	}
	if value < fallback {
		klog.Warningf("KV routing freshness threshold %s is below recommended %s (refresh %s, fetch timeout %s)", value, fallback, podMetricRefreshInterval, podMetricsFetchTimeout)
	}
	klog.Infof("KV routing freshness threshold=%s source=%s refresh=%s fetch_timeout=%s", value, source, podMetricRefreshInterval, podMetricsFetchTimeout)
	return value
}
