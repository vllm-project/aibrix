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
	"reflect"
	"testing"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

func TestNormalizeCircuitBreaker(t *testing.T) {
	tests := []struct {
		name string
		in   *autoscalingv1alpha1.CircuitBreakerConfig
		want Config
	}{
		{
			name: "nil",
			want: Config{Enabled: false},
		},
		{
			name: "disabled",
			in:   &autoscalingv1alpha1.CircuitBreakerConfig{},
			want: Config{Enabled: false, Action: autoscalingv1alpha1.CircuitBreakerActionFreeze, FailureThreshold: 3, RecoveryThreshold: 3},
		},
		{
			name: "enabled defaults",
			in:   &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true},
			want: Config{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionFreeze, FailureThreshold: 3, RecoveryThreshold: 3},
		},
		{
			name: "explicit max",
			in: &autoscalingv1alpha1.CircuitBreakerConfig{
				Enabled:           true,
				Action:            autoscalingv1alpha1.CircuitBreakerActionMax,
				FailureThreshold:  1,
				RecoveryThreshold: 5,
			},
			want: Config{Enabled: true, Action: autoscalingv1alpha1.CircuitBreakerActionMax, FailureThreshold: 1, RecoveryThreshold: 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := autoscalingv1alpha1.CircuitBreakerConfig{}
			if tt.in != nil {
				original = *tt.in
			}

			if got := NormalizeCircuitBreaker(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeCircuitBreaker() = %#v, want %#v", got, tt.want)
			}
			if tt.in != nil && !reflect.DeepEqual(*tt.in, original) {
				t.Fatalf("NormalizeCircuitBreaker() modified input: got %#v, want %#v", *tt.in, original)
			}
		})
	}
}

func TestValidateCircuitBreaker(t *testing.T) {
	tests := []struct {
		name      string
		strategy  autoscalingv1alpha1.ScalingStrategyType
		cfg       *autoscalingv1alpha1.CircuitBreakerConfig
		wantField string
	}{
		{name: "nil", strategy: autoscalingv1alpha1.HPA},
		{
			name:     "disabled HPA ignores invalid fields",
			strategy: autoscalingv1alpha1.HPA,
			cfg: &autoscalingv1alpha1.CircuitBreakerConfig{
				Action:            "panic",
				FailureThreshold:  -1,
				RecoveryThreshold: -1,
			},
		},
		{name: "KPA enabled with defaults", strategy: autoscalingv1alpha1.KPA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}},
		{name: "APA enabled with defaults", strategy: autoscalingv1alpha1.APA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}},
		{name: "HPA enabled", strategy: autoscalingv1alpha1.HPA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}, wantField: "spec.circuitBreaker"},
		{name: "unknown strategy enabled", strategy: "unknown", cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}, wantField: "spec.circuitBreaker"},
		{name: "invalid action", strategy: autoscalingv1alpha1.KPA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: "panic"}, wantField: "spec.circuitBreaker.action"},
		{name: "negative failure threshold", strategy: autoscalingv1alpha1.APA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, FailureThreshold: -1}, wantField: "spec.circuitBreaker.failureThreshold"},
		{name: "negative recovery threshold", strategy: autoscalingv1alpha1.KPA, cfg: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, RecoveryThreshold: -1}, wantField: "spec.circuitBreaker.recoveryThreshold"},
		{
			name:     "explicit values",
			strategy: autoscalingv1alpha1.APA,
			cfg: &autoscalingv1alpha1.CircuitBreakerConfig{
				Enabled:           true,
				Action:            autoscalingv1alpha1.CircuitBreakerActionMax,
				FailureThreshold:  1,
				RecoveryThreshold: 5,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCircuitBreaker(tt.strategy, tt.cfg)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("ValidateCircuitBreaker() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCircuitBreaker() error = nil, want field %q", tt.wantField)
			}
			if err.Field != tt.wantField {
				t.Fatalf("ValidateCircuitBreaker() field = %q, want %q", err.Field, tt.wantField)
			}
		})
	}
}
