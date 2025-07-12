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

package stormservice

import (
	"testing"

	"github.com/stretchr/testify/assert"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestSetDefaultStormServiceValues(t *testing.T) {
	reconciler := &StormServiceReconciler{}

	tests := []struct {
		name     string
		storm    *orchestrationv1alpha1.StormService
		expected orchestrationv1alpha1.StormServiceUpdateStrategyType
	}{
		{
			name: "empty update strategy type",
			storm: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
						Type: "",
					},
				},
			},
			expected: orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
		},
		{
			name: "existing update strategy type",
			storm: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
						Type: orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType,
					},
				},
			},
			expected: orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler.setDefaultStormServiceValues(tt.storm)
			assert.Equal(t, tt.expected, tt.storm.Spec.UpdateStrategy.Type)
		})
	}
}
