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
	"context"
	"testing"
)

func TestHPAAlgorithm_ComputeRecommendation(t *testing.T) {
	// HPA scaling is delegated to the Kubernetes HPA controller, so the
	// recommendation must echo the current replica count unchanged.
	tests := []struct {
		name            string
		currentReplicas int32
	}{
		{"zero replicas", 0},
		{"single replica", 1},
		{"multiple replicas", 7},
	}

	a := &HPAAlgorithm{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.ComputeRecommendation(context.Background(), ScalingRequest{
				CurrentReplicas: tt.currentReplicas,
			})
			if err != nil {
				t.Fatalf("ComputeRecommendation() returned error: %v", err)
			}

			if got.DesiredReplicas != tt.currentReplicas {
				t.Errorf("DesiredReplicas = %d, want %d", got.DesiredReplicas, tt.currentReplicas)
			}
			if got.Algorithm != "hpa" {
				t.Errorf("Algorithm = %q, want %q", got.Algorithm, "hpa")
			}
			if !got.ScaleValid {
				t.Error("ScaleValid = false, want true")
			}
			if got.Confidence != 1.0 {
				t.Errorf("Confidence = %v, want 1.0", got.Confidence)
			}
		})
	}
}

func TestHPAAlgorithm_GetAlgorithmType(t *testing.T) {
	a := &HPAAlgorithm{}
	if got := a.GetAlgorithmType(); got != "hpa" {
		t.Errorf("GetAlgorithmType() = %q, want %q", got, "hpa")
	}
}
