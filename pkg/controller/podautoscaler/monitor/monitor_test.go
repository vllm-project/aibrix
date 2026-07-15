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

package monitor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMonitor_RecordScaleAction(t *testing.T) {
	// Reset the metric before the test to ensure a clean state and avoid interference between test runs.
	autoscalerScaleAction.Reset()

	m := New()

	// Define test cases
	testCases := []struct {
		namespace       string
		name            string
		algorithm       string
		reason          string
		desiredReplicas int32
	}{
		{"test-namespace", "test-name", "test-algorithm", "test-reason", 5},
		{"prod-namespace", "prod-name", "prod-algorithm", "prod-reason", 10},
	}

	// Record all scale actions
	for _, tc := range testCases {
		m.RecordScaleAction(tc.namespace, tc.name, tc.algorithm, tc.reason, tc.desiredReplicas)
	}
	// Check that all metrics were recorded correctly
	for _, tc := range testCases {
		expectedValue := float64(tc.desiredReplicas)
		actualValue := testutil.ToFloat64(autoscalerScaleAction.WithLabelValues(tc.namespace, tc.name, tc.algorithm, tc.reason))
		assert.Equal(t, expectedValue, actualValue, "Expected metric for %s/%s to be %v", tc.namespace, tc.name, tc.desiredReplicas)
	}
}

func TestMonitor_RecordCircuitBreakerMetrics(t *testing.T) {
	circuitBreakerState.Reset()
	circuitBreakerTransitions.Reset()
	circuitBreakerActionFailures.Reset()

	m := New()

	m.RecordCircuitBreakerState("ns-a", "pa-a", true)
	assert.Equal(t, float64(1), testutil.ToFloat64(circuitBreakerState.WithLabelValues("ns-a", "pa-a")))

	m.RecordCircuitBreakerState("ns-a", "pa-a", false)
	assert.Equal(t, float64(0), testutil.ToFloat64(circuitBreakerState.WithLabelValues("ns-a", "pa-a")))

	m.RecordCircuitBreakerTransition("ns-a", "pa-a", "freeze", "Opened")
	m.RecordCircuitBreakerTransition("ns-a", "pa-a", "freeze", "Opened")
	assert.Equal(t, float64(2), testutil.ToFloat64(circuitBreakerTransitions.WithLabelValues("ns-a", "pa-a", "freeze", "Opened")))

	m.RecordCircuitBreakerActionFailure("ns-a", "pa-a", "freeze")
	assert.Equal(t, float64(1), testutil.ToFloat64(circuitBreakerActionFailures.WithLabelValues("ns-a", "pa-a", "freeze")))
}
