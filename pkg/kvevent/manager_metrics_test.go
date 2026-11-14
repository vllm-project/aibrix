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

// TestStartRegistersKVCacheMetrics checks that starting the manager with KV
// event sync on registers the aibrix_kvcache_zmq_* metrics, so a ZMQ client's
// updates reach the default registry that the gateway's /metrics serves.
func TestStartRegistersKVCacheMetrics(t *testing.T) {
	t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
	t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")

	podProvider := &mockPodProvider{pods: make(map[string]*kvevent.PodInfo)}
	syncProvider := &mockSyncIndexProvider{indexer: &mockSyncIndexer{}}
	manager := kvevent.NewManager(podProvider, syncProvider)
	if err := manager.Start(); err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Stop()

	// A pod key no other test uses, so series left by other tests in the
	// shared default registry cannot satisfy the check.
	const podKey = "default/metrics-registration-pod"
	metrics := kvcache.NewZMQClientMetrics(podKey)
	defer metrics.Delete()
	metrics.IncrementConnectionCount()

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
					if got := m.GetCounter().GetValue(); got != 1 {
						t.Fatalf("aibrix_kvcache_zmq_connections_total{pod_key=%q} = %v, want 1", podKey, got)
					}
					return
				}
			}
		}
	}
	t.Fatalf("aibrix_kvcache_zmq_connections_total{pod_key=%q} is not registered", podKey)
}
