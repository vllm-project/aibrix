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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

func TestAPAAlgorithm_ComputeTargetReplicas(t *testing.T) {
	algorithm := &APAAlgorithm{}
	metricsName := "test-metrics"

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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
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
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 30.0,
					},
				},
				CurrentUsePerPod:         27.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           2,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2,
			},
			expected: 2,
		},
		{
			name:            "division_by_zero",
			currentPodCount: 1.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 0.0,
					},
				},
				CurrentUsePerPod:         10.0,
				UpFluctuationTolerance:   0.1,
				MaxScaleUpRate:           1000.0,
				DownFluctuationTolerance: 0.1,
				MaxScaleDownRate:         2.0,
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := algorithm.computeTargetReplicas(tt.currentPodCount, tt.context, metricsName, "")
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAPAAlgorithm_ComputeTargetReplicas_MetricSources(t *testing.T) {
	algorithm := &APAAlgorithm{}
	metricsName := "test-metrics"
	tests := []struct {
		name            string
		source          autoscalingv1alpha1.MetricSourceType
		currentPodCount float64
		value           float64
		expected        int32
	}{
		{"external_total", autoscalingv1alpha1.EXTERNAL, 4, 80, 2},
		{"external_downscale_floor", autoscalingv1alpha1.EXTERNAL, 10, 80, 5},
		{"external_within_tolerance", autoscalingv1alpha1.EXTERNAL, 1, 41, 1},
		{"external_normalized_tolerance", autoscalingv1alpha1.EXTERNAL, 4, 164, 4},
		{"external_scale_up_limit", autoscalingv1alpha1.EXTERNAL, 2, 400, 4},
		{"external_round_up", autoscalingv1alpha1.EXTERNAL, 4, 81, 3},
		{"external_exact_downscale", autoscalingv1alpha1.EXTERNAL, 9, 240, 6},
		{"domain_exact_upscale", autoscalingv1alpha1.DOMAIN, 9, 480, 12},
		{"external_from_zero", autoscalingv1alpha1.EXTERNAL, 0, 80, 1},
		{"external_from_zero_small_total", autoscalingv1alpha1.EXTERNAL, 0, 1, 1},
		{"external_zero_load", autoscalingv1alpha1.EXTERNAL, 0, 0, 0},
		{"domain_total", autoscalingv1alpha1.DOMAIN, 4, 80, 2},
		{"domain_from_zero", autoscalingv1alpha1.DOMAIN, 0, 80, 1},
		{"pod_mean", autoscalingv1alpha1.POD, 4, 80, 8},
		{"resource_mean", autoscalingv1alpha1.RESOURCE, 4, 80, 8},
		{"custom_mean", autoscalingv1alpha1.CUSTOM, 4, 80, 8},
		{"pod_from_zero", autoscalingv1alpha1.POD, 0, 80, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			context := &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40, MetricType: tt.source},
				},
				CurrentUsePerPod:         tt.value,
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2,
				MaxScaleDownRate:         2,
			}
			result := algorithm.computeTargetReplicas(tt.currentPodCount, context, metricsName, "")
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAPAAlgorithm_ComputeRecommendation_SharedMetricName(t *testing.T) {
	metricsName := "gpu_cache_usage_perc"
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{MetricSourceType: autoscalingv1alpha1.POD, TargetMetric: metricsName, TargetValue: "40"},
				{MetricSourceType: autoscalingv1alpha1.EXTERNAL, TargetMetric: metricsName, TargetValue: "40"},
			},
		},
	}
	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 100
	require.NoError(t, scalingContext.UpdateByPaTypes(pa))
	// The external source overwrites the pod source in the name-keyed context.
	sourceType, ok := scalingContext.GetMetricSourceTypeForMetric(metricsName)
	require.True(t, ok)
	require.Equal(t, autoscalingv1alpha1.EXTERNAL, sourceType)

	for i, source := range pa.Spec.MetricsSources {
		t.Run(string(source.MetricSourceType), func(t *testing.T) {
			recommendation, err := (&APAAlgorithm{}).ComputeRecommendation(context.Background(), ScalingRequest{
				CurrentReplicas:  4,
				MetricSourceType: source.MetricSourceType,
				AggregatedMetrics: &types.AggregatedMetrics{
					MetricKey:   types.MetricKey{MetricName: source.TargetMetric},
					StableValue: 80,
				},
				ScalingContext: scalingContext,
			})
			require.NoError(t, err)
			assert.Equal(t, []int32{8, 2}[i], recommendation.DesiredReplicas)
		})
	}
}
