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
					TargetMetric:     "gpu_cache_usage_perc",
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
		"gpu_cache_usage_perc": {
			TargetValue:   50,
			TotalValue:    100,
			ScalingMetric: "gpu_cache_usage_perc",
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
							TargetMetric:     "gpu_cache_usage_perc",
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

func TestUpdateByPaTypes_RejectsUnusableAnnotationValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "scale-up rate not a number", key: types.MaxScaleUpRateLabel, value: "fast", want: "must be a number"},
		{name: "scale-up rate NaN", key: types.MaxScaleUpRateLabel, value: "NaN", want: "must be a finite number"},
		{name: "scale-up rate infinite", key: types.MaxScaleUpRateLabel, value: "+Inf", want: "must be a finite number"},
		{name: "scale-up rate below one", key: types.MaxScaleUpRateLabel, value: "0.5", want: "must be greater than or equal to 1"},
		{name: "scale-down rate zero", key: types.MaxScaleDownRateLabel, value: "0", want: "must be greater than or equal to 1"},
		{name: "scale-down rate negative", key: types.MaxScaleDownRateLabel, value: "-2", want: "must be greater than or equal to 1"},
		{name: "scale-up tolerance negative", key: types.ScaleUpToleranceLabel, value: "-0.1", want: "must not be negative"},
		{name: "scale-down tolerance NaN", key: types.ScaleDownToleranceLabel, value: "nan", want: "must be a finite number"},
		{name: "panic threshold zero", key: types.PanicThresholdLabel, value: "0", want: "must be greater than 0"},
		{name: "scale-up cooldown not a duration", key: types.ScaleUpCooldownWindowLabel, value: "30", want: "must be a duration"},
		{name: "scale-down cooldown negative", key: types.ScaleDownCooldownWindowLabel, value: "-5m", want: "must not be negative"},
		{name: "scale-to-zero not a boolean", key: types.ScaleToZeroLabel, value: "maybe", want: "must be a boolean"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{Annotations: map[string]string{tt.key: tt.value}},
			}
			defaults := NewBaseScalingContext()
			ctx := NewBaseScalingContext()

			err := ctx.UpdateByPaTypes(pa)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
			assert.Contains(t, err.Error(), tt.want)
			// The unusable value must not replace the default.
			assert.Equal(t, defaults, ctx)
		})
	}
}

// An unusable annotation must only cost its own setting. Before this was made
// independent per key, parsing stopped at the first bad value while ranging
// over the annotations map, so which of the other settings applied depended on
// map iteration order and could differ from one reconcile to the next.
func TestUpdateByPaTypes_AppliesUsableAnnotationsNextToUnusableOnes(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{Annotations: map[string]string{
			types.MaxScaleUpRateLabel:          "3",
			types.MaxScaleDownRateLabel:        "0",
			types.ScaleUpToleranceLabel:        "0.3",
			types.ScaleDownToleranceLabel:      "NaN",
			types.PanicThresholdLabel:          "4",
			types.ScaleUpCooldownWindowLabel:   "30s",
			types.ScaleDownCooldownWindowLabel: "-1m",
			types.ScaleToZeroLabel:             "true",
		}},
	}
	defaults := NewBaseScalingContext()

	for i := 0; i < 50; i++ {
		ctx := NewBaseScalingContext()

		err := ctx.UpdateByPaTypes(pa)

		require.Error(t, err)
		assert.Equal(t, 3.0, ctx.MaxScaleUpRate)
		assert.Equal(t, 0.3, ctx.UpFluctuationTolerance)
		assert.Equal(t, 4.0, ctx.PanicThreshold)
		assert.Equal(t, 30*time.Second, ctx.ScaleUpCooldownWindow)
		assert.True(t, ctx.ScaleToZero)
		assert.Equal(t, defaults.MaxScaleDownRate, ctx.MaxScaleDownRate)
		assert.Equal(t, defaults.DownFluctuationTolerance, ctx.DownFluctuationTolerance)
		assert.Equal(t, defaults.ScaleDownCooldownWindow, ctx.ScaleDownCooldownWindow)
	}
}

func TestValidateAnnotations(t *testing.T) {
	t.Run("accepts usable values and ignores annotations it does not read", func(t *testing.T) {
		errs := ValidateAnnotations(map[string]string{
			types.MaxScaleUpRateLabel:          "1",
			types.MaxScaleDownRateLabel:        "2.5",
			types.ScaleUpToleranceLabel:        "0",
			types.ScaleDownToleranceLabel:      "0.2",
			types.PanicThresholdLabel:          "200",
			types.ScaleUpCooldownWindowLabel:   "0s",
			types.ScaleDownCooldownWindowLabel: "5m",
			types.ScaleToZeroLabel:             "false",
			"example.com/owner":                "not-a-number",
		})

		assert.Empty(t, errs)
	})

	t.Run("reports every unusable value ordered by key", func(t *testing.T) {
		errs := ValidateAnnotations(map[string]string{
			types.ScaleUpToleranceLabel: "-1",
			types.MaxScaleDownRateLabel: "0",
			types.PanicThresholdLabel:   "2",
		})

		require.Len(t, errs, 2)
		assert.Equal(t, types.MaxScaleDownRateLabel, errs[0].Key)
		assert.Equal(t, "0", errs[0].Value)
		assert.Equal(t, types.ScaleUpToleranceLabel, errs[1].Key)
		assert.Equal(t, "-1", errs[1].Value)
	})
}
