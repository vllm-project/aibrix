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

package podautoscaler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

func TestPredictiveStatusFromEvaluation(t *testing.T) {
	assert.Nil(t, predictiveStatus(nil))

	status := predictiveStatus(&PredictiveResult{
		Mode:              autoscalingv1alpha1.PredictiveModeAuto,
		ObservedValue:     12.3456,
		PredictedValue:    23.4567,
		PredictedReplicas: 3,
	})

	require.NotNil(t, status)
	assert.Equal(t, autoscalingv1alpha1.PredictiveModeAuto, status.Mode)
	assert.Equal(t, "12.346", status.ObservedValue)
	assert.Equal(t, "23.457", status.PredictedValue)
	assert.Equal(t, int32(3), status.PredictedReplicas)
	assert.Nil(t, status.LastUpdated)
}

func TestSetStatusReportsPredictiveAndKeepsObservationTime(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{}
	evaluation := &PredictiveResult{
		Mode:              autoscalingv1alpha1.PredictiveModePreview,
		ObservedValue:     12.3456,
		PredictedValue:    23.4567,
		PredictedReplicas: 3,
	}

	setStatus(pa, 2, 2, false, "stable", false, true, nil, predictiveStatus(evaluation))
	require.NotNil(t, pa.Status.Predictive)
	firstObservation := pa.Status.Predictive.LastUpdated
	require.NotNil(t, firstObservation)

	setStatus(pa, 2, 2, false, "stable", false, true, nil, predictiveStatus(evaluation))
	require.NotNil(t, pa.Status.Predictive.LastUpdated)
	assert.Same(t, firstObservation, pa.Status.Predictive.LastUpdated)

	evaluation.PredictedValue = 30
	setStatus(pa, 2, 2, false, "stable", false, true, nil, predictiveStatus(evaluation))
	require.NotNil(t, pa.Status.Predictive.LastUpdated)
	assert.NotSame(t, firstObservation, pa.Status.Predictive.LastUpdated)
	assert.Equal(t, "30.000", pa.Status.Predictive.PredictedValue)

	setStatus(pa, 2, 2, false, "stable", false, true, nil, nil)
	assert.Nil(t, pa.Status.Predictive)
}
