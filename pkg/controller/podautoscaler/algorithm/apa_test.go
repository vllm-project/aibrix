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

package algorithm

import (
	"testing"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"
)

func TestApaScalingAlgorithm_ComputeTargetReplicas(t *testing.T) {
	algorithm := &ApaScalingAlgorithm{}

	tests := []struct {
		name            string
		currentPodCount float64
		context         *common.MockScalingContext
		expected        int32
		description     string
	}{
		{
			name:            "no_scaling_within_tolerance",
			currentPodCount: 3.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,   // Can scale up by 100%
				MaxScaleDownRate:         2.0,   // Can scale down by 50%
				CurrentUsePerPod:         50.0,  // Exactly at target
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    3,
			description: "Should not scale when current use per pod equals target value",
		},
		{
			name:            "no_scaling_within_up_tolerance",
			currentPodCount: 3.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				CurrentUsePerPod:         54.0,  // 54/50 = 1.08, within 1+0.1=1.1 tolerance
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    3,
			description: "Should not scale when within up tolerance (8% above target)",
		},
		{
			name:            "no_scaling_within_down_tolerance",
			currentPodCount: 3.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				CurrentUsePerPod:         42.0,  // 42/50 = 0.84, within 1-0.2=0.8 tolerance
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    3,
			description: "Should not scale when within down tolerance (16% below target)",
		},
		{
			name:            "scale_up_beyond_tolerance",
			currentPodCount: 3.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,   // Can scale up by 100%
				MaxScaleDownRate:         2.0,
				CurrentUsePerPod:         60.0,  // 60/50 = 1.2, exceeds 1+0.1=1.1 tolerance
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    4,  // ceil(3 * (60/50)) = ceil(3.6) = 4
			description: "Should scale up when current use exceeds up tolerance threshold",
		},
		{
			name:            "scale_down_beyond_tolerance",
			currentPodCount: 5.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,   // Can scale down by 50%
				CurrentUsePerPod:         30.0,  // 30/50 = 0.6, below 1-0.2=0.8 tolerance
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    3,  // ceil(5 * (30/50)) = ceil(3.0) = 3
			description: "Should scale down when current use falls below down tolerance threshold",
		},
		{
			name:            "scale_up_limited_by_max_scale_up_rate",
			currentPodCount: 3.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           1.5,   // Can only scale up by 50%
				MaxScaleDownRate:         2.0,
				CurrentUsePerPod:         100.0, // 100/50 = 2.0, would need 6 pods but limited
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    5,  // ceil(1.5 * 3) = ceil(4.5) = 5 (limited by max scale up rate)
			description: "Should be limited by max scale up rate",
		},
		{
			name:            "scale_down_limited_by_max_scale_down_rate",
			currentPodCount: 6.0,
			context: &common.MockScalingContext{
				TargetValue:              50.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         3.0,   // Can scale down to 1/3 = 33% of current (6/3=2 minimum)
				CurrentUsePerPod:         10.0,  // 10/50 = 0.2, would need 1.2 pods but limited
				MinReplicas:              1,
				MaxReplicas:              10,
			},
			expected:    2,  // floor(6/3) = floor(2) = 2
			description: "Should be limited by max scale down rate",
		},
		{
			name:            "extreme_scale_up_case",
			currentPodCount: 2.0,
			context: &common.MockScalingContext{
				TargetValue:              10.0,
				UpFluctuationTolerance:   0.1,   // 10% tolerance
				DownFluctuationTolerance: 0.2,   // 20% tolerance
				MaxScaleUpRate:           3.0,   // Can scale up by 200%
				MaxScaleDownRate:         2.0,
				CurrentUsePerPod:         50.0,  // 50/10 = 5.0, would need 10 pods
				MinReplicas:              1,
				MaxReplicas:              20,
			},
			expected:    6,  // ceil(3.0 * 2) = 6 (limited by max scale up rate)
			description: "Should handle extreme scale up scenarios with rate limiting",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := algorithm.ComputeTargetReplicas(tt.currentPodCount, tt.context)
			if result != tt.expected {
				t.Errorf("ComputeTargetReplicas() = %d, expected %d. %s", result, tt.expected, tt.description)
				t.Logf("Current pod count: %.1f", tt.currentPodCount)
				t.Logf("Current use per pod: %.1f", tt.context.CurrentUsePerPod)
				t.Logf("Target value: %.1f", tt.context.TargetValue)
				t.Logf("Up tolerance: %.3f", tt.context.UpFluctuationTolerance)
				t.Logf("Down tolerance: %.3f", tt.context.DownFluctuationTolerance)
				t.Logf("Ratio (current/expected): %.6f", tt.context.CurrentUsePerPod/tt.context.TargetValue)
				t.Logf("Up threshold: %.6f", 1+tt.context.UpFluctuationTolerance)
				t.Logf("Down threshold: %.6f", 1-tt.context.DownFluctuationTolerance)
			}
		})
	}
}