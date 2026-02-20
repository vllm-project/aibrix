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

package context

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateByPaTypes_MetricsSources(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-llm-pa",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "kv_cache_usage_perc",
					TargetValue:      "50",
				},
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "30",
				},
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "memory",
					TargetValue:      "128",
				},
			},
		},
	}

	expectedMetricTargets := map[string]MetricTarget{
		"kv_cache_usage_perc": {
			TargetValue:   50,
			TotalValue:    100,
			ScalingMetric: "kv_cache_usage_perc",
			MetricType:    autoscalingv1alpha1.POD,
		},
		"cpu": {
			TargetValue:   30,
			TotalValue:    100,
			ScalingMetric: "cpu",
			MetricType:    autoscalingv1alpha1.RESOURCE,
		},
		"memory": {
			TargetValue:   128,
			TotalValue:    100,
			ScalingMetric: "memory",
			MetricType:    autoscalingv1alpha1.RESOURCE,
		},
	}

	ctx := NewBaseScalingContext()

	err := ctx.UpdateByPaTypes(pa)

	assert.NoError(t, err)
	assert.Len(t, ctx.MetricTargets, 3)
	assert.Equal(t, expectedMetricTargets, ctx.MetricTargets)
}

func TestUpdateByPaTypes_Annotations(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-llm-pa",
			Namespace: "default",
			Annotations: map[string]string{
				types.MaxScaleUpRateLabel:   "3.0",
				types.MaxScaleDownRateLabel: "3.0",

				types.ScaleUpToleranceLabel:   "0.3",
				types.ScaleDownToleranceLabel: "0.3",

				types.PanicThresholdLabel: "3.0",

				types.ScaleUpCooldownWindowLabel:   "30s",
				types.ScaleDownCooldownWindowLabel: "30s",

				types.ScaleToZeroLabel: "true",
			},
		},
	}

	expectedMaxScaleUpRate := 3.0
	expectedMaxScaleDownRate := 3.0
	expectedScaleUpTolerance := 0.3
	expectedScaleDownTolerance := 0.3
	expectedPanicThreshold := 3.0
	expectedScaleUpCooldownWindow := time.Second * 30
	expectedScaleDownCooldownWindow := time.Second * 30

	ctx := NewBaseScalingContext()
	require.NotEqual(t, expectedMaxScaleUpRate, ctx.MaxScaleUpRate)
	require.NotEqual(t, expectedMaxScaleDownRate, ctx.MaxScaleDownRate)
	require.NotEqual(t, expectedScaleUpTolerance, ctx.UpFluctuationTolerance)
	require.NotEqual(t, expectedScaleDownTolerance, ctx.DownFluctuationTolerance)
	require.NotEqual(t, expectedPanicThreshold, ctx.PanicThreshold)
	require.NotEqual(t, expectedScaleUpCooldownWindow, ctx.ScaleUpCooldownWindow)
	require.NotEqual(t, expectedScaleDownCooldownWindow, ctx.ScaleDownCooldownWindow)
	require.False(t, ctx.ScaleToZero)

	err := ctx.UpdateByPaTypes(pa)

	assert.NoError(t, err)
	assert.Equal(t, expectedMaxScaleUpRate, ctx.MaxScaleUpRate)
	assert.Equal(t, expectedMaxScaleDownRate, ctx.MaxScaleDownRate)
	assert.Equal(t, expectedScaleUpTolerance, ctx.UpFluctuationTolerance)
	assert.Equal(t, expectedScaleDownTolerance, ctx.DownFluctuationTolerance)
	assert.Equal(t, expectedPanicThreshold, ctx.PanicThreshold)
	assert.Equal(t, expectedScaleUpCooldownWindow, ctx.ScaleUpCooldownWindow)
	assert.Equal(t, expectedScaleDownCooldownWindow, ctx.ScaleDownCooldownWindow)
	assert.True(t, ctx.ScaleToZero)
}

func TestUpdateByPaTypes_InvalidTargetValue(t *testing.T) {
	tests := []struct {
		name        string
		targetValue string
	}{
		{
			name:        "Zero Target Value",
			targetValue: "0",
		},
		{
			name:        "Negative Target Value",
			targetValue: "-10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-llm-pa",
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.POD,
							TargetMetric:     "kv_cache_usage_perc",
							TargetValue:      tt.targetValue,
						},
					},
				},
			}

			ctx := NewBaseScalingContext()
			err := ctx.UpdateByPaTypes(pa)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), "must be greater than 0")
		})
	}
}
