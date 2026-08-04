/*
Copyright 2024 The Aibrix Team.

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

package circuitbreaker

import (
	"math"
	"testing"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestValidateMetricValues(t *testing.T) {
	tests := []struct {
		name        string
		values      []float64
		expectValid bool
		expectMsg   string
	}{
		{
			name:        "valid values",
			values:      []float64{0.5, 1.0, 2.5, 0.0},
			expectValid: true,
		},
		{
			name:        "empty values",
			values:      []float64{},
			expectValid: false,
			expectMsg:   "no metric values provided",
		},
		{
			name:        "NaN value",
			values:      []float64{1.0, math.NaN(), 2.0},
			expectValid: false,
			expectMsg:   "NaN",
		},
		{
			name:        "positive infinity",
			values:      []float64{1.0, math.Inf(1), 2.0},
			expectValid: false,
			expectMsg:   "Inf",
		},
		{
			name:        "negative infinity",
			values:      []float64{1.0, math.Inf(-1), 2.0},
			expectValid: false,
			expectMsg:   "Inf",
		},
		{
			name:        "negative value",
			values:      []float64{1.0, -0.5, 2.0},
			expectValid: false,
			expectMsg:   "negative",
		},
		{
			name:        "multiple invalid values",
			values:      []float64{math.NaN(), -1.0, math.Inf(1)},
			expectValid: false,
		},
		{
			name:        "zero is valid",
			values:      []float64{0.0, 0.0, 0.0},
			expectValid: true,
		},
		{
			name:        "single valid value",
			values:      []float64{42.0},
			expectValid: true,
		},
		{
			name:        "single NaN value",
			values:      []float64{math.NaN()},
			expectValid: false,
			expectMsg:   "NaN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateMetricValues(tt.values)
			assert.Equal(t, tt.expectValid, result.Valid)
			if !tt.expectValid && tt.expectMsg != "" {
				assert.Contains(t, result.Reason, tt.expectMsg)
			}
		})
	}
}

func TestValidateAggregatedValues(t *testing.T) {
	tests := []struct {
		name        string
		stableVal   float64
		panicVal    float64
		expectValid bool
	}{
		{
			name:        "both valid",
			stableVal:   1.0,
			panicVal:    2.0,
			expectValid: true,
		},
		{
			name:        "stable NaN",
			stableVal:   math.NaN(),
			panicVal:    2.0,
			expectValid: false,
		},
		{
			name:        "panic NaN",
			stableVal:   1.0,
			panicVal:    math.NaN(),
			expectValid: false,
		},
		{
			name:        "stable negative",
			stableVal:   -1.0,
			panicVal:    2.0,
			expectValid: false,
		},
		{
			name:        "panic Inf",
			stableVal:   1.0,
			panicVal:    math.Inf(1),
			expectValid: false,
		},
		{
			name:        "both zero",
			stableVal:   0.0,
			panicVal:    0.0,
			expectValid: true,
		},
		{
			name:        "both invalid",
			stableVal:   math.NaN(),
			panicVal:    math.Inf(-1),
			expectValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateAggregatedValues(tt.stableVal, tt.panicVal)
			assert.Equal(t, tt.expectValid, result.Valid)
		})
	}
}

func TestEvaluateCircuitBreaker(t *testing.T) {
	tests := []struct {
		name             string
		config           *autoscalingv1alpha1.CircuitBreakerConfig
		currentReplicas  int32
		maxReplicas      int32
		validationResult MetricsValidationResult
		wantReplicas     int32
		wantTriggered    bool
	}{
		{
			name:             "nil config - no intervention",
			config:           nil,
			currentReplicas:  3,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: false, Reason: "NaN detected"},
			wantReplicas:     3,
			wantTriggered:    false,
		},
		{
			name:             "disabled config - no intervention",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: false, Action: autoscalingv1alpha1.CircuitBreakerActionFreeze},
			currentReplicas:  3,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: false, Reason: "NaN detected"},
			wantReplicas:     3,
			wantTriggered:    false,
		},
		{
			name:             "enabled but valid metrics - no intervention",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionFreeze},
			currentReplicas:  3,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: true},
			wantReplicas:     3,
			wantTriggered:    false,
		},
		{
			name:             "freeze action - keeps current replicas",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionFreeze},
			currentReplicas:  5,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: false, Reason: "NaN metric value detected"},
			wantReplicas:     5,
			wantTriggered:    true,
		},
		{
			name:             "max action - scales to maximum",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionMax},
			currentReplicas:  3,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: false, Reason: "Inf metric value detected"},
			wantReplicas:     10,
			wantTriggered:    true,
		},
		{
			name:             "empty action defaults to freeze",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: ""},
			currentReplicas:  4,
			maxReplicas:      8,
			validationResult: MetricsValidationResult{Valid: false, Reason: "negative value"},
			wantReplicas:     4,
			wantTriggered:    true,
		},
		{
			name:             "unknown action defaults to freeze",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: "unknown"},
			currentReplicas:  4,
			maxReplicas:      8,
			validationResult: MetricsValidationResult{Valid: false, Reason: "bad metric"},
			wantReplicas:     4,
			wantTriggered:    true,
		},
		{
			name:             "max action at boundary (current equals max)",
			config:           &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionMax},
			currentReplicas:  10,
			maxReplicas:      10,
			validationResult: MetricsValidationResult{Valid: false, Reason: "bad metric"},
			wantReplicas:     10,
			wantTriggered:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replicas, triggered, _ := EvaluateCircuitBreaker(tt.config, tt.currentReplicas, tt.maxReplicas, tt.validationResult)
			assert.Equal(t, tt.wantReplicas, replicas)
			assert.Equal(t, tt.wantTriggered, triggered)
		})
	}
}
