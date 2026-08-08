/*
Copyright 2026 The Aibrix Team.

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

package config

import (
	"fmt"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Config is the normalized runtime circuit breaker configuration.
type Config struct {
	Enabled           bool
	Action            autoscalingv1alpha1.CircuitBreakerAction
	FailureThreshold  int32
	RecoveryThreshold int32
}

// NormalizeCircuitBreaker returns an independent runtime configuration with defaults applied.
func NormalizeCircuitBreaker(in *autoscalingv1alpha1.CircuitBreakerConfig) Config {
	if in == nil {
		return Config{}
	}

	out := Config{
		Enabled:           in.Enabled,
		Action:            in.Action,
		FailureThreshold:  in.FailureThreshold,
		RecoveryThreshold: in.RecoveryThreshold,
	}
	if out.Action == "" {
		out.Action = autoscalingv1alpha1.CircuitBreakerActionFreeze
	}
	if out.FailureThreshold == 0 {
		out.FailureThreshold = autoscalingv1alpha1.DefaultCircuitBreakerFailureThreshold
	}
	if out.RecoveryThreshold == 0 {
		out.RecoveryThreshold = autoscalingv1alpha1.DefaultCircuitBreakerRecoveryThreshold
	}
	return out
}

// ValidateCircuitBreaker validates an enabled circuit breaker configuration.
func ValidateCircuitBreaker(strategy autoscalingv1alpha1.ScalingStrategyType, in *autoscalingv1alpha1.CircuitBreakerConfig) *field.Error {
	if in == nil || !in.Enabled {
		return nil
	}

	path := field.NewPath("spec", "circuitBreaker")
	if strategy != autoscalingv1alpha1.KPA && strategy != autoscalingv1alpha1.APA {
		return field.Forbidden(path, "circuit breaker is supported only for KPA and APA scaling strategies")
	}

	cfg := NormalizeCircuitBreaker(in)
	if cfg.Action != autoscalingv1alpha1.CircuitBreakerActionFreeze && cfg.Action != autoscalingv1alpha1.CircuitBreakerActionMax {
		return field.NotSupported(path.Child("action"), cfg.Action, []string{
			string(autoscalingv1alpha1.CircuitBreakerActionFreeze),
			string(autoscalingv1alpha1.CircuitBreakerActionMax),
		})
	}
	if cfg.FailureThreshold < 1 {
		return field.Invalid(path.Child("failureThreshold"), cfg.FailureThreshold, fmt.Sprintf("must be at least %d", 1))
	}
	if cfg.RecoveryThreshold < 1 {
		return field.Invalid(path.Child("recoveryThreshold"), cfg.RecoveryThreshold, fmt.Sprintf("must be at least %d", 1))
	}
	return nil
}
