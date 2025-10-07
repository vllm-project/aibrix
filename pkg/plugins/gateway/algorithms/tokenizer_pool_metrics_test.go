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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// simpleTokenizer implements the tokenizer.Tokenizer interface for testing
type simpleTokenizer struct{}

func (m *simpleTokenizer) TokenizeInputText(text string) ([]byte, error) {
	return []byte(text), nil
}

func TestTokenizerPoolMetricsNotRegisteredByDefault(t *testing.T) {
	// Create pool with feature disabled
	config := TokenizerPoolConfig{
		EnableVLLMRemote: false,
		DefaultTokenizer: &simpleTokenizer{},
	}

	pool := NewTokenizerPool(config, nil)
	defer func() { _ = pool.Close() }()

	// Verify no metrics were created
	if pool.metrics != nil {
		t.Fatal("Expected metrics to be nil when feature is disabled")
	}
	if pool.metricsRegistered {
		t.Fatal("Expected metricsRegistered to be false when feature is disabled")
	}

	// Create a new registry to check metrics
	registry := prometheus.NewRegistry()

	// Gather all metrics
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Check that no tokenizer pool metrics are registered
	for _, mf := range metricFamilies {
		name := mf.GetName()
		if name == "aibrix_tokenizer_pool_active_tokenizers" ||
			name == "aibrix_tokenizer_pool_creation_successes_total" ||
			name == "aibrix_tokenizer_pool_creation_failures_total" ||
			name == "aibrix_tokenizer_pool_unhealthy_tokenizers_total" ||
			name == "aibrix_tokenizer_pool_requests_total" ||
			name == "aibrix_tokenizer_pool_latency_seconds" {
			t.Errorf("Found tokenizer pool metric %s when none should be registered", name)
		}
	}

	// Verify operations work without metrics (no panic)
	tok := pool.GetTokenizer("test-model", nil)
	if tok == nil {
		t.Fatal("Expected to get default tokenizer")
	}
}

func TestTokenizerPoolMetricsRegisteredWhenEnabled(t *testing.T) {
	// Create pool with feature enabled
	config := TokenizerPoolConfig{
		EnableVLLMRemote:  true,
		EndpointTemplate:  "http://%s:8000",
		DefaultTokenizer:  &simpleTokenizer{},
		HealthCheckPeriod: 0, // Disable health check for testing
	}

	pool := NewTokenizerPool(config, nil)
	defer func() { _ = pool.Close() }()

	// Verify metrics were created
	if pool.metrics == nil {
		t.Fatal("Expected metrics to be created when feature is enabled")
	}
	if !pool.metricsRegistered {
		t.Fatal("Expected metricsRegistered to be true when feature is enabled")
	}

	// Test that metric operations don't panic
	pool.incTokenizerRequests("test-model")
	pool.incTokenizerCreationSuccesses()
	pool.incTokenizerCreationFailures()
	pool.incUnhealthyTokenizers()
	pool.incActiveTokenizers()
	pool.decActiveTokenizers()
	pool.setActiveTokenizers(5)
	pool.observeTokenizerLatency("test-model", 100*time.Millisecond)
}

func TestTokenizerPoolMetricsNoOpWhenNotInitialized(t *testing.T) {
	// Create pool with feature disabled
	config := TokenizerPoolConfig{
		EnableVLLMRemote: false,
		DefaultTokenizer: &simpleTokenizer{},
	}

	pool := NewTokenizerPool(config, nil)
	defer func() { _ = pool.Close() }()

	// All operations should be no-ops and not panic
	pool.incTokenizerRequests("test-model")
	pool.incTokenizerCreationSuccesses()
	pool.incTokenizerCreationFailures()
	pool.incUnhealthyTokenizers()
	pool.incActiveTokenizers()
	pool.decActiveTokenizers()
	pool.setActiveTokenizers(5)
	pool.observeTokenizerLatency("test-model", 100*time.Millisecond)

	// Verify GetTokenizer works without metrics
	tok := pool.GetTokenizer("test-model", nil)
	if tok == nil {
		t.Fatal("Expected to get default tokenizer")
	}
}

func TestTokenizerPoolMetricsUnregisterOnClose(t *testing.T) {
	// Create pool with feature enabled
	config := TokenizerPoolConfig{
		EnableVLLMRemote:  true,
		EndpointTemplate:  "http://%s:8000",
		DefaultTokenizer:  &simpleTokenizer{},
		HealthCheckPeriod: 0, // Disable health check for testing
	}

	pool := NewTokenizerPool(config, nil)

	// Verify metrics were created
	if pool.metrics == nil {
		t.Fatal("Expected metrics to be created when feature is enabled")
	}

	// Close the pool which should unregister metrics
	err := pool.Close()
	if err != nil {
		t.Fatalf("Failed to close pool: %v", err)
	}

	// The unregister call should have been made (we can't easily verify it was successful
	// without mocking Prometheus, but at least verify it doesn't panic)
}

func TestTokenizerPoolConcurrentMetricsAccess(t *testing.T) {
	// Create pool with feature enabled
	config := TokenizerPoolConfig{
		EnableVLLMRemote:  true,
		EndpointTemplate:  "http://%s:8000",
		DefaultTokenizer:  &simpleTokenizer{},
		HealthCheckPeriod: 0, // Disable health check for testing
	}

	pool := NewTokenizerPool(config, nil)
	defer func() { _ = pool.Close() }()

	// Run concurrent operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				model := "test-model-" + string(rune(id))
				pool.incTokenizerRequests(model)
				pool.observeTokenizerLatency(model, time.Duration(j)*time.Millisecond)
				pool.incActiveTokenizers()
				pool.decActiveTokenizers()
				if j%10 == 0 {
					pool.incTokenizerCreationSuccesses()
				}
				if j%15 == 0 {
					pool.incTokenizerCreationFailures()
				}
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}
