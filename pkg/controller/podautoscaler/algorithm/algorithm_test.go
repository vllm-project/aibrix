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

package algorithm

import (
	"testing"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

func TestApplyConstraints(t *testing.T) {
	tests := []struct {
		name        string
		replicas    int32
		minReplicas int32
		maxReplicas int32
		want        int32
	}{
		// out-of-range values are clamped
		{"below minimum is raised to min", 1, 2, 10, 2},
		{"above maximum is capped to max", 20, 2, 10, 10},
		// in-range and boundary values are returned unchanged
		{"within range is unchanged", 5, 2, 10, 5},
		{"exactly at minimum", 2, 2, 10, 2},
		{"exactly at maximum", 10, 2, 10, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &mockScalingContext{
				MinReplicas: tt.minReplicas,
				MaxReplicas: tt.maxReplicas,
			}

			if got := applyConstraints(tt.replicas, ctx); got != tt.want {
				t.Errorf("applyConstraints(%d) = %d, want %d", tt.replicas, got, tt.want)
			}
		})
	}
}

func TestNewScalingAlgorithm(t *testing.T) {
	tests := []struct {
		name     string
		strategy autoscalingv1alpha1.ScalingStrategyType
		want     string // expected GetAlgorithmType() result
	}{
		{"KPA strategy returns kpa algorithm", autoscalingv1alpha1.KPA, "kpa"},
		{"APA strategy returns apa algorithm", autoscalingv1alpha1.APA, "apa"},
		{"HPA strategy returns hpa algorithm", autoscalingv1alpha1.HPA, "hpa"},
		// unrecognized strategies fall back to KPA
		{"unknown strategy falls back to kpa", autoscalingv1alpha1.ScalingStrategyType("UNKNOWN"), "kpa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			algo := NewScalingAlgorithm(tt.strategy)
			if algo == nil {
				t.Fatalf("NewScalingAlgorithm(%q) returned nil", tt.strategy)
			}

			if got := algo.GetAlgorithmType(); got != tt.want {
				t.Errorf("GetAlgorithmType() = %q, want %q", got, tt.want)
			}
		})
	}
}
