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
	"sync"
	"testing"
)

func TestPrefixCacheMetricsAlwaysRegistered(t *testing.T) {
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	if prefixCacheMetrics == nil {
		t.Fatal("Expected metrics to be registered")
	}

	metrics := getPrefixCacheMetrics()
	if metrics == nil {
		t.Fatal("Expected to get metrics after initialization")
	}

	recordRoutingDecision("test-model", 50, false)
	recordRoutingSelection("test-model", "prefix_match", false)
	recordRoutingError("test-model", "no_target_pod", false)
}

func TestPrefixCacheMetricsNoOpWhenNotInitialized(t *testing.T) {
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	// All record helpers must be no-ops and not panic when metrics are nil.
	recordRoutingDecision("test-model", 50, false)
	recordRoutingSelection("test-model", "prefix_match", false)
	recordRoutingError("test-model", "no_target_pod", false)

	// Confirm that no helper silently initialised the global.
	if prefixCacheMetrics != nil {
		t.Fatal("record helpers must not initialize metrics")
	}
}

func TestRecordRoutingDecisionBuckets(t *testing.T) {
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

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
	}
}

func TestConcurrentPrefixCacheMetricsAccess(t *testing.T) {
	prefixCacheMetrics = nil
	prefixCacheMetricsOnce = sync.Once{}

	err := initializePrefixCacheMetrics()
	if err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				recordRoutingDecision("test-model", j%101, j%2 == 0)
				recordRoutingSelection("test-model", "prefix_match", j%2 == 0)
				recordRoutingError("test-model", "no_target_pod", j%2 == 0)
			}
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
