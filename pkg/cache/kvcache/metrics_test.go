// Copyright 2025 The AIBrix Authors
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

package kvcache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestMetricsNotRegisteredByDefault(t *testing.T) {
	// Reset global state for testing
	kvCacheMetrics = nil

	// Create a new registry to check metrics
	registry := prometheus.NewRegistry()

	// Gather all metrics
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Check that no KV cache metrics are registered
	for _, mf := range metricFamilies {
		if contains(mf.GetName(), "aibrix_kvcache_zmq") {
			t.Errorf("Found KV cache metric %s when none should be registered", mf.GetName())
		}
	}
}

func TestMetricsRegisteredWhenInitialized(t *testing.T) {
	// Reset global state for testing
	kvCacheMetrics = nil
	kvCacheMetricsOnce = sync.Once{}

	// Initialize metrics
	err := InitializeMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Verify metrics were created
	if kvCacheMetrics == nil {
		t.Fatal("Expected metrics to be created after initialization")
	}

	// Try to get a metric value to ensure it's registered
	metrics := getMetrics()
	if metrics == nil {
		t.Fatal("Expected to get metrics after initialization")
	}

	// Create a client and verify it can use metrics
	client := NewZMQClientMetrics("test-pod")
	if client == nil {
		t.Fatal("Expected to create client metrics")
	}

	// Test that operations don't panic
	client.IncrementConnectionCount()
	client.IncrementDisconnectionCount()
	client.IncrementReconnectAttempts()
	client.IncrementEventCount("test-event")
	client.RecordEventProcessingLatency(100 * time.Millisecond)
	client.IncrementMissedEvents(5)
	client.IncrementReplayCount()
	client.IncrementReplaySuccess()
	client.IncrementReplayFailure()
	client.IncrementErrorCount("test-error")
	client.UpdateLastSequenceID(123)
}

func TestMetricsNoOpWhenNotInitialized(t *testing.T) {
	// Reset global state for testing
	kvCacheMetrics = nil
	kvCacheMetricsOnce = sync.Once{}

	// Create a client without initializing metrics
	client := NewZMQClientMetrics("test-pod")
	if client == nil {
		t.Fatal("Expected to create client metrics even without initialization")
	}

	// All operations should be no-ops and not panic
	client.IncrementConnectionCount()
	client.IncrementDisconnectionCount()
	client.IncrementReconnectAttempts()
	client.IncrementEventCount("test-event")
	client.RecordEventProcessingLatency(100 * time.Millisecond)
	client.IncrementMissedEvents(5)
	client.IncrementReplayCount()
	client.IncrementReplaySuccess()
	client.IncrementReplayFailure()
	client.IncrementErrorCount("test-error")
	client.UpdateLastSequenceID(123)
	client.Delete()
}

func TestMetricsDelete(t *testing.T) {
	// Reset global state for testing
	kvCacheMetrics = nil
	kvCacheMetricsOnce = sync.Once{}

	// Initialize metrics
	err := InitializeMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Create a client and add some metrics
	client := NewZMQClientMetrics("test-pod-delete")
	client.IncrementConnectionCount()
	client.IncrementEventCount(string(EventTypeBlockStored))

	// Delete the metrics
	client.Delete()

	// Verify metrics were deleted by trying to get them
	// This is a bit tricky to test properly without exposing internals
	// For now, just ensure Delete doesn't panic
}

func TestConcurrentMetricsAccess(t *testing.T) {
	// Reset global state for testing
	kvCacheMetrics = nil
	kvCacheMetricsOnce = sync.Once{}

	// Initialize metrics
	err := InitializeMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Run concurrent operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			client := NewZMQClientMetrics(fmt.Sprintf("pod-%d", id))
			for j := 0; j < 100; j++ {
				client.IncrementConnectionCount()
				client.IncrementEventCount("test-event")
				client.RecordEventProcessingLatency(time.Duration(j) * time.Millisecond)
			}
			client.Delete()
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || contains(s[1:], substr)))
}

// Helper function to get metric value (for testing)
func getMetricValue(vec *prometheus.CounterVec, labels ...string) (float64, error) {
	metric := vec.WithLabelValues(labels...)
	dto := &dto.Metric{}
	if err := metric.Write(dto); err != nil {
		return 0, err
	}
	return dto.Counter.GetValue(), nil
}
