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

package algorithm

import (
	"testing"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"
)

func TestKpaScalingAlgorithm_ComputeTargetReplicas(t *testing.T) {
	algorithm := &KpaScalingAlgorithm{}

	tests := []struct {
		name            string
		currentPodCount float64
		context         *common.MockScalingContext
		expected        int32
		description     string
	}{
		{
			name:            "stable_mode_basic_scaling",
			currentPodCount: 2.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      20.0, // 20/10 = 2 pods needed
				PanicValue:       20.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      false,
				MaxPanicPods:     0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    2,
			description: "Should scale to 2 pods based on stable value in stable mode",
		},
		{
			name:            "panic_mode_scaling_up",
			currentPodCount: 2.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      30.0, // 30/10 = 3 pods needed
				PanicValue:       40.0, // 40/10 = 4 pods needed
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      true,
				MaxPanicPods:     2,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    4,
			description: "Should scale to 4 pods based on panic value in panic mode",
		},
		{
			name:            "panic_mode_no_scale_down",
			currentPodCount: 5.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      20.0, // 20/10 = 2 pods needed
				PanicValue:       20.0, // 20/10 = 2 pods needed
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      true,
				MaxPanicPods:     5, // Should not scale down below this
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    5,
			description: "Should not scale down below maxPanicPods in panic mode",
		},
		{
			name:            "scale_to_zero",
			currentPodCount: 1.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      0.0, // No load, should scale to 0
				PanicValue:       0.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      false,
				MaxPanicPods:     0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    0,
			description: "Should scale to zero when no load",
		},
		{
			name:            "activation_scale_from_zero",
			currentPodCount: 0.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      15.0, // 15/10 = 2 pods needed, but activation scale should apply
				PanicValue:       15.0,
				ActivationScale:  3, // Should scale to 3 when activating
				PanicThreshold:   2.0,
				InPanicMode:      false,
				MaxPanicPods:     0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    3,
			description: "Should respect activation scale when scaling from zero",
		},
		{
			name:            "max_scale_up_rate_limit",
			currentPodCount: 2.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   1.5, // Can only scale up by 50%
				MaxScaleDownRate: 2.0,
				StableValue:      100.0, // Would need 10 pods, but limited by scale up rate
				PanicValue:       100.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      false,
				MaxPanicPods:     0,
				MinReplicas:      0,
				MaxReplicas:      20,
			},
			expected:    3, // 2 * 1.5 = 3 (ceil)
			description: "Should be limited by max scale up rate",
		},
		{
			name:            "max_scale_down_rate_limit",
			currentPodCount: 4.0,
			context: &common.MockScalingContext{
				TargetValue:      10.0,
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0, // Can scale down by 50% (4/2 = 2 minimum)
				StableValue:      5.0, // Would need 1 pod, but limited by scale down rate
				PanicValue:       5.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				InPanicMode:      false,
				MaxPanicPods:     0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    2, // 4/2 = 2 (floor)
			description: "Should be limited by max scale down rate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := algorithm.ComputeTargetReplicas(tt.currentPodCount, tt.context)
			if result != tt.expected {
				t.Errorf("ComputeTargetReplicas() = %d, expected %d. %s", result, tt.expected, tt.description)
			}
		})
	}
}
