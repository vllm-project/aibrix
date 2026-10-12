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

func TestMonitor_ForgetScaleAction(t *testing.T) {
	autoscalerScaleAction.Reset()

	m := New()
	// One PodAutoscaler leaves a series per algorithm/reason pair it has reported.
	m.RecordScaleAction("default", "llm", "kpa", "stable", 2)
	m.RecordScaleAction("default", "llm", "kpa", "kpa above target", 4)
	m.RecordScaleAction("default", "llm-canary", "apa", "stable", 1)
	m.RecordScaleAction("prod", "llm", "kpa", "stable", 3)
	assert.Equal(t, 4, testutil.CollectAndCount(autoscalerScaleAction))

	m.ForgetScaleAction("default", "llm")

	assert.Equal(t, 2, testutil.CollectAndCount(autoscalerScaleAction))
	assert.Equal(t, 1.0, testutil.ToFloat64(autoscalerScaleAction.WithLabelValues("default", "llm-canary", "apa", "stable")))
	assert.Equal(t, 3.0, testutil.ToFloat64(autoscalerScaleAction.WithLabelValues("prod", "llm", "kpa", "stable")))
}
