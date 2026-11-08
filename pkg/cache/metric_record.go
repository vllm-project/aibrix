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
	"math"
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
// Keeping it separate from Cache preserves compatibility with external cache implementations.
type MetricRecordCache interface {
	GetMetricRecordByPod(podName, podNamespace, metricName string) (MetricRecord, error)
	GetMetricRecordByPodModel(podName, podNamespace, modelName, metricName string) (MetricRecord, error)
}

var routingMetricStaleThreshold = loadRoutingMetricStaleThreshold()

// RoutingMetricStaleThreshold returns the gateway-local KV scoring age limit,
// configured at process startup, like the collection refresh interval.
func RoutingMetricStaleThreshold() time.Duration { return routingMetricStaleThreshold }

func loadRoutingMetricStaleThreshold() time.Duration {
	// Validate in milliseconds before either multiplication can overflow Duration.
	const maxMilliseconds = math.MaxInt64 / int64(time.Millisecond)
	refreshMS := utils.LoadEnvInt("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", defaultPodMetricRefreshIntervalInMS)
	fallback := 5 * time.Second
	if int64(refreshMS) <= maxMilliseconds/3 {
		fallback = max(fallback, 3*time.Duration(refreshMS)*time.Millisecond)
	}
	// LoadEnvInt already rejects zero, negative and malformed overrides.
	ms := utils.LoadEnvInt("AIBRIX_ROUTING_METRIC_STALE_THRESHOLD_MS", int(fallback/time.Millisecond))
	if int64(ms) > maxMilliseconds {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}
