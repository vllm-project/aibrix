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

	"github.com/stretchr/testify/assert"
)

func TestAPAAlgorithm_ComputeTargetReplicas(t *testing.T) {
	algorithm := &APAAlgorithm{}

	tests := []struct {
		name            string
		currentPodCount float64
		context         *mockScalingContext
		expected        int32
	}{
		// scale up tests
		{
			name:            "basic_scale_up",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         45.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 3,
		},
		{
			name:            "scale_up_upto_ceiling",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         35.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 3,
		},
		{
			name:            "scale_up_upto_max_scale_up",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         90.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 4,
		},
		{
			name:            "scale_up_upto_max_scale_up_ceiling",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         90.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2.5,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 5,
		},
		// scale down tests
		{
			name:            "basic_scale_down",
			currentPodCount: 6.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         20.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 4,
		},
		{
			name:            "scale_down_upto_ceiling",
			currentPodCount: 6.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         18.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 4,
		},
		{
			name:            "scale_down_upto_max_scale_down",
			currentPodCount: 6.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         10.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 3,
		},
		{
			name:            "scale_down_upto_max_scale_down_floor",
			currentPodCount: 6.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         5.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2.5,
			},
			expected: 2,
		},
		// no scaling tests
		{
			name:            "no_scaling_as_within_up_tolerance",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         33.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 2,
		},
		{
			name:            "no_scaling_as_within_down_tolerance",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				TargetValue:              30.0,
				CurrentUsePerPod:         27.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := algorithm.computeTargetReplicas(tt.currentPodCount, tt.context)
			assert.Equal(t, tt.expected, result)
		})
	}
}
