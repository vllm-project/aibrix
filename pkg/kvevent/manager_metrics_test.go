// Copyright 2025 AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build zmq
// +build zmq

package kvevent_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
)

// TestSyncOnRegistersKVCacheMetrics checks that a manager built with KV event
// sync on registers the aibrix_kvcache_zmq_* metrics before Start, so a ZMQ
// client created for a pod event that arrives before Start still reaches the
// default registry that the gateway's /metrics serves.
func TestSyncOnRegistersKVCacheMetrics(t *testing.T) {
	kvcache.ResetMetricsForTest()
	t.Cleanup(kvcache.ResetMetricsForTest)
	t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
	t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")

	podProvider := &mockPodProvider{pods: make(map[string]*kvevent.PodInfo)}
	syncProvider := &mockSyncIndexProvider{indexer: &mockSyncIndexer{}}
	manager := kvevent.NewManager(podProvider, syncProvider)

	const podKey = "default/metrics-registration-pod"
	metrics := kvcache.NewZMQClientMetrics(podKey)
	defer metrics.Delete()
	metrics.IncrementConnectionCount()

	if err := manager.Start(); err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Stop()

	if got, ok := connectionCount(t, podKey); !ok || got != 1 {
		t.Fatalf("aibrix_kvcache_zmq_connections_total{pod_key=%q} = %v (registered: %v), want 1", podKey, got, ok)
	}
}

// TestSyncOffRegistersNoKVCacheMetrics checks the other half of the contract:
// with KV event sync off, neither building nor starting the manager registers
// the metrics, so a client's updates leave no series.
func TestSyncOffRegistersNoKVCacheMetrics(t *testing.T) {
	kvcache.ResetMetricsForTest()
	t.Cleanup(kvcache.ResetMetricsForTest)
	t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "false")

	podProvider := &mockPodProvider{pods: make(map[string]*kvevent.PodInfo)}
	syncProvider := &mockSyncIndexProvider{indexer: &mockSyncIndexer{}}
	manager := kvevent.NewManager(podProvider, syncProvider)
	if err := manager.Start(); err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Stop()

	if kvcache.MetricsInitialized() {
		t.Fatal("KV cache metrics are registered with KV event sync off")
	}
	const podKey = "default/sync-off-pod"
	kvcache.NewZMQClientMetrics(podKey).IncrementConnectionCount()
	if _, ok := connectionCount(t, podKey); ok {
		t.Fatalf("aibrix_kvcache_zmq_connections_total{pod_key=%q} exists with KV event sync off", podKey)
	}
}

// connectionCount returns aibrix_kvcache_zmq_connections_total for podKey from
// the default registry, and whether that series exists.
func connectionCount(t *testing.T, podKey string) (float64, bool) {
	t.Helper()
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}
	for _, mf := range metricFamilies {
		if mf.GetName() != "aibrix_kvcache_zmq_connections_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == kvcache.LabelPodKey && label.GetValue() == podKey {
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}
