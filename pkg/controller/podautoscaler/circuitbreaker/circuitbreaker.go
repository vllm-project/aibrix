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

// Package circuitbreaker provides protection against bad metrics in the autoscaler pipeline.
// When enabled, it detects invalid metric values (NaN, Inf, negative) and applies a protective
// action (freeze current replicas or scale to maximum) to prevent erratic scaling behavior.
package circuitbreaker

import (
	"fmt"
	"math"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"k8s.io/klog/v2"
)

// MetricsValidationResult contains the result of validating metric values.
type MetricsValidationResult struct {
	// Valid indicates whether all metric values are valid.
	Valid bool
	// Reason describes why the metrics are invalid.
	Reason string
	// InvalidValues contains the invalid metric values found.
	InvalidValues []float64
}

// ValidateMetricValues checks a slice of metric values for invalid entries.
// Invalid values include: NaN, +Inf, -Inf, and negative values.
func ValidateMetricValues(values []float64) MetricsValidationResult {
	if len(values) == 0 {
		return MetricsValidationResult{
			Valid:  false,
			Reason: "no metric values provided",
		}
	}

	var invalidValues []float64
	var reason string

	for _, v := range values {
		if math.IsNaN(v) {
			invalidValues = append(invalidValues, v)
			reason = "NaN metric value detected"
		} else if math.IsInf(v, 0) {
			invalidValues = append(invalidValues, v)
			reason = "Inf metric value detected"
		} else if v < 0 {
			invalidValues = append(invalidValues, v)
			reason = "negative metric value detected"
		}
	}

	if len(invalidValues) > 0 {
		return MetricsValidationResult{
			Valid:         false,
			Reason:        fmt.Sprintf("%s (%d invalid out of %d values)", reason, len(invalidValues), len(values)),
			InvalidValues: invalidValues,
		}
	}

	return MetricsValidationResult{Valid: true}
}

// ValidateAggregatedValues checks aggregated metric values (stable, panic) for invalid entries.
func ValidateAggregatedValues(stableValue, panicValue float64) MetricsValidationResult {
	var invalidValues []float64
	var reasons []string

	if math.IsNaN(stableValue) || math.IsInf(stableValue, 0) || stableValue < 0 {
		invalidValues = append(invalidValues, stableValue)
		reasons = append(reasons, fmt.Sprintf("invalid stable value: %v", stableValue))
	}

	if math.IsNaN(panicValue) || math.IsInf(panicValue, 0) || panicValue < 0 {
		invalidValues = append(invalidValues, panicValue)
		reasons = append(reasons, fmt.Sprintf("invalid panic value: %v", panicValue))
	}

	if len(invalidValues) > 0 {
		return MetricsValidationResult{
			Valid:         false,
			Reason:        fmt.Sprintf("invalid aggregated metrics: %v", reasons),
			InvalidValues: invalidValues,
		}
	}

	return MetricsValidationResult{Valid: true}
}

// EvaluateCircuitBreaker determines the circuit breaker action for a PodAutoscaler
// when invalid metrics are detected. It returns the desired replica count and whether
// the circuit breaker was triggered.
func EvaluateCircuitBreaker(
	config *autoscalingv1alpha1.CircuitBreakerConfig,
	currentReplicas int32,
	maxReplicas int32,
	validationResult MetricsValidationResult,
) (desiredReplicas int32, triggered bool, reason string) {
	// If circuit breaker is not configured or not enabled, don't intervene
	if config == nil || !config.Enabled {
		return currentReplicas, false, ""
	}

	// If metrics are valid, circuit breaker is not triggered
	if validationResult.Valid {
		return currentReplicas, false, ""
	}

	// Circuit breaker triggered - apply action
	action := config.Action
	if action == "" {
		action = autoscalingv1alpha1.CircuitBreakerActionFreeze // default to freeze
	}

	switch action {
	case autoscalingv1alpha1.CircuitBreakerActionFreeze:
		klog.InfoS("Circuit breaker triggered: freezing at current replicas",
			"currentReplicas", currentReplicas,
			"reason", validationResult.Reason)
		return currentReplicas, true,
			fmt.Sprintf("circuit breaker triggered (freeze): %s", validationResult.Reason)

	case autoscalingv1alpha1.CircuitBreakerActionMax:
		klog.InfoS("Circuit breaker triggered: scaling to maximum replicas",
			"currentReplicas", currentReplicas,
			"maxReplicas", maxReplicas,
			"reason", validationResult.Reason)
		return maxReplicas, true,
			fmt.Sprintf("circuit breaker triggered (max): %s", validationResult.Reason)

	default:
		klog.Warningf("Unknown circuit breaker action %q, defaulting to freeze", action)
		return currentReplicas, true,
			fmt.Sprintf("circuit breaker triggered (freeze/default): %s", validationResult.Reason)
	}
}
