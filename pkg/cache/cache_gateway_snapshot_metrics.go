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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// States of another gateway's snapshot key at a published tick, the state label of
// aibrix_gateway_snapshot_remote_keys.
const (
	// snapshotKeyFresh: read by this tick.
	snapshotKeyFresh = "fresh"
	// snapshotKeyCarried: not read by this tick, its last value carried over and still within
	// remoteRunningMaxAge, so it still counts. A steady share means ticks are missing keys.
	snapshotKeyCarried = "carried"
	// snapshotKeyStale: carried over but older than remoteRunningMaxAge, so its count is left
	// out of the candidate stage's estimate and the realtime_num_requests_running gauge.
	snapshotKeyStale = "stale"
)

var (
	snapshotMetricsOnce   sync.Once
	snapshotRemoteKeys    *prometheus.GaugeVec
	snapshotOldestReadAge prometheus.Gauge
)

func registerGauge[T prometheus.Collector](c T, name string) T {
	if err := prometheus.Register(c); err != nil {
		registered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			klog.ErrorS(err, "failed to register gateway snapshot metric", "metric", name)
			return c
		}
		return registered.ExistingCollector.(T)
	}
	return c
}

func initSnapshotMetrics() {
	snapshotMetricsOnce.Do(func() {
		snapshotRemoteKeys = registerGauge(prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "gateway_snapshot_remote_keys",
				Help:      "Other gateways' snapshot keys in the last published snapshot, by whether this tick read them (fresh), carried an earlier value still within the age limit (carried), or carried one past it, which is left out of the remote running counts (stale)",
			},
			[]string{"state"},
		), "gateway_snapshot_remote_keys")
		snapshotOldestReadAge = registerGauge(prometheus.NewGauge(
			prometheus.GaugeOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "gateway_snapshot_oldest_read_age_seconds",
				Help:      "Age, at the last published snapshot, of the least recently read of the other gateways' snapshot keys",
			},
		), "gateway_snapshot_oldest_read_age_seconds")
	})
}

// snapshotFreshness counts the other gateways' keys of a published tick (raw, keyed by Redis key)
// by state as of start, the tick's start and the read time of every key it read: read by the
// tick, carried within remoteRunningMaxAge, or carried past it. oldest is the age of the least
// recently read one. Keys of this gateway (self) and keys without pod identity are not counted.
func snapshotFreshness(raw map[string]map[string]string, readAt map[string]time.Time, self string, start time.Time) (fresh, carried, stale int, oldest time.Duration) {
	for key, fields := range raw {
		if fields["namespace"] == "" || fields["pod_name"] == "" || fields["gateway_instance_id"] == self {
			continue
		}
		age := start.Sub(readAt[key])
		switch {
		case age <= 0:
			fresh++
		case age <= remoteRunningMaxAge:
			carried++
		default:
			stale++
		}
		if age > oldest {
			oldest = age
		}
	}
	return fresh, carried, stale, oldest
}

// recordSnapshotFreshness sets the snapshot freshness gauges from a published tick (see
// snapshotFreshness). A tick that publishes nothing (a failed scan, an unread first snapshot)
// leaves the gauges as they were; the rate-limited Error log reports those.
func recordSnapshotFreshness(raw map[string]map[string]string, readAt map[string]time.Time, self string, start time.Time) {
	fresh, carried, stale, oldest := snapshotFreshness(raw, readAt, self, start)
	initSnapshotMetrics()
	if snapshotRemoteKeys == nil || snapshotOldestReadAge == nil {
		return
	}
	snapshotRemoteKeys.WithLabelValues(snapshotKeyFresh).Set(float64(fresh))
	snapshotRemoteKeys.WithLabelValues(snapshotKeyCarried).Set(float64(carried))
	snapshotRemoteKeys.WithLabelValues(snapshotKeyStale).Set(float64(stale))
	snapshotOldestReadAge.Set(oldest.Seconds())
}
