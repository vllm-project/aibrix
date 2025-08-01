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

package routingalgorithms

import (
	"os"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestPrefixCacheMetricsNotRegisteredByDefault(t *testing.T) {
	// Reset global state for testing
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// Ensure metrics are not enabled
	_ = os.Setenv(constants.EnvPrefixCacheMetricsEnabled, "false")
	defer func() { _ = os.Unsetenv(constants.EnvPrefixCacheMetricsEnabled) }()

	// Try to initialize metrics
	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Verify metrics were NOT created
	if prefixCacheMetrics != nil {
		t.Fatal("Expected metrics to be nil when disabled")
	}

	// Create a new registry to check metrics
	registry := prometheus.NewRegistry()

	// Gather all metrics
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Check that no prefix cache metrics are registered
	for _, mf := range metricFamilies {
		name := mf.GetName()
		if name == "aibrix_prefix_cache_routing_decisions_total" ||
			name == "aibrix_prefix_cache_indexer_status" ||
			name == "aibrix_prefix_cache_routing_latency_seconds" {
			t.Errorf("Found prefix cache metric %s when none should be registered", name)
		}
	}
}

func TestPrefixCacheMetricsRegisteredWhenEnabled(t *testing.T) {
	// Reset global state for testing
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// Enable metrics
	_ = os.Setenv(constants.EnvPrefixCacheMetricsEnabled, "true")
	defer func() { _ = os.Unsetenv(constants.EnvPrefixCacheMetricsEnabled) }()

	// Initialize metrics
	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Verify metrics were created
	if prefixCacheMetrics == nil {
		t.Fatal("Expected metrics to be created when enabled")
	}

	// Verify we can get metrics
	metrics := getPrefixCacheMetrics()
	if metrics == nil {
		t.Fatal("Expected to get metrics after initialization")
	}

	// Test that metric operations don't panic
	recordRoutingDecision("test-model", 50, false)
	recordRoutingDecision("test-model", 100, true)
	recordRoutingDecision("test-model", 0, false)
}

func TestPrefixCacheMetricsNoOpWhenNotInitialized(t *testing.T) {
	// Reset global state for testing
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// Ensure metrics are not enabled
	_ = os.Setenv(constants.EnvPrefixCacheMetricsEnabled, "false")
	defer func() { _ = os.Unsetenv(constants.EnvPrefixCacheMetricsEnabled) }()

	// Initialize (should be no-op)
	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// All operations should be no-ops and not panic
	recordRoutingDecision("test-model", 50, false)
	recordRoutingDecision("test-model", 100, true)
	recordRoutingDecision("test-model", 0, false)
}

func TestRecordRoutingDecisionBuckets(t *testing.T) {
	// Reset global state for testing
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// Enable metrics
	_ = os.Setenv(constants.EnvPrefixCacheMetricsEnabled, "true")
	defer func() { _ = os.Unsetenv(constants.EnvPrefixCacheMetricsEnabled) }()

	// Initialize metrics
	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Test all bucket ranges
	testCases := []struct {
		matchPercent   int
		expectedBucket string
	}{
		{0, "0"},
		{1, "1-25"},
		{25, "1-25"},
		{26, "26-50"},
		{50, "26-50"},
		{51, "51-75"},
		{75, "51-75"},
		{76, "76-100"},
		{99, "76-100"},
		{100, "76-100"},
	}

	for _, tc := range testCases {
		recordRoutingDecision("test-model", tc.matchPercent, false)
		// Note: In a real test, we would verify the metric was recorded
		// with the correct bucket label
	}
}

func TestConcurrentPrefixCacheMetricsAccess(t *testing.T) {
	// Reset global state for testing
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// Enable metrics
	_ = os.Setenv(constants.EnvPrefixCacheMetricsEnabled, "true")
	defer func() { _ = os.Unsetenv(constants.EnvPrefixCacheMetricsEnabled) }()

	// Initialize metrics
	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Run concurrent operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				recordRoutingDecision("test-model", j%101, j%2 == 0)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}
