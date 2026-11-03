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

package podautoscaler

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/utils/paschedules"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestMakeHPA(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-llm-pa",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "aibrix",
			},
			Annotations: map[string]string{
				AutoscalingStormServiceModeAnnotationKey: "replica",
			},
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-llm",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: int32(5),
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

	ctx := context.NewBaseScalingContext()

	expectedHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-llm-pa-hpa",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "aibrix",
			},
			Annotations: map[string]string{
				AutoscalingStormServiceModeAnnotationKey: "replica",
			},
			OwnerReferences: []v1.OwnerReference{
				{
					APIVersion:         "autoscaling.aibrix.ai/v1alpha1",
					Kind:               "PodAutoscaler",
					Name:               "test-llm-pa",
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-llm",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: int32(5),
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
					SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PercentScalingPolicy,
							Value:         100,
							PeriodSeconds: 60,
						},
					},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(300)),
					SelectPolicy:               ptr.To(autoscalingv2.MinChangePolicySelect),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PercentScalingPolicy,
							Value:         50,
							PeriodSeconds: 60,
						},
					},
				},
			},
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: "gpu_cache_usage_perc",
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: resource.NewQuantity(int64(50), resource.DecimalSI),
						},
					},
				},
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: "cpu",
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: ptr.To(int32(math.Ceil(30))),
						},
					},
				},
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: "memory",
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: resource.NewQuantity(int64(128)*1024*1024, resource.BinarySI),
						},
					},
				},
			},
		},
	}

	actualHPA, err := makeHPA(pa, ctx)

	assert.NoError(t, err)
	assert.NotNil(t, actualHPA)
	assert.Equal(t, expectedHPA, actualHPA)
}

func TestMakeHPAFractionalTargetValue(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-fractional-pa",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-llm",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: int32(5),
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "memory",
					TargetValue:      "0.5",
				},
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      "0.5",
				},
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "num_requests_waiting",
					TargetValue:      "1.2",
				},
				{
					MetricSourceType: autoscalingv1alpha1.CUSTOM,
					TargetMetric:     "requests",
					TargetValue:      "9223372036854774784",
				},
			},
		},
	}

	hpa, err := makeHPA(pa, context.NewBaseScalingContext())

	assert.NoError(t, err)
	assert.NotNil(t, hpa)
	assert.Len(t, hpa.Spec.Metrics, 4)

	// The memory target is a whole number of MiB, so a fractional one rounds up
	// instead of being truncated by int64(): 0.5 MiB -> 1 MiB, never 0 bytes.
	memoryTarget := hpa.Spec.Metrics[0].Resource.Target
	assert.NotNil(t, memoryTarget.AverageValue)
	assert.Equal(t, int64(1*1024*1024), memoryTarget.AverageValue.Value())

	// A pod metric target is a Quantity, so it keeps the fraction. Rounding
	// gpu_cache_usage_perc's 0.5 up to 1 would leave a 0-1 metric that can never exceed
	// its target.
	for i, want := range []string{"500m", "1200m", "9223372036854774784"} {
		podsTarget := hpa.Spec.Metrics[i+1].Pods.Target
		if assert.NotNil(t, podsTarget.AverageValue) {
			assert.Zero(t, podsTarget.AverageValue.Cmp(resource.MustParse(want)),
				"%s target = %s, want %s", hpa.Spec.Metrics[i+1].Pods.Metric.Name, podsTarget.AverageValue, want)
		}
	}
}

func TestMakeHPARejectsTargetValueOverflow(t *testing.T) {
	tests := map[string]struct {
		metricSourceType autoscalingv1alpha1.MetricSourceType
		targetMetric     string
		targetValue      string
	}{
		"cpu": {
			metricSourceType: autoscalingv1alpha1.RESOURCE,
			targetMetric:     "cpu",
			targetValue:      "2147483647.1",
		},
		"memory": {
			metricSourceType: autoscalingv1alpha1.RESOURCE,
			targetMetric:     "memory",
			targetValue:      "8796093022207.1",
		},
		"pods": {
			metricSourceType: autoscalingv1alpha1.CUSTOM,
			targetMetric:     "requests",
			targetValue:      "9223372036854775808",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{Name: "test-overflow-pa", Namespace: "default"},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "test-llm"},
					MinReplicas:    ptr.To(int32(1)),
					MaxReplicas:    5,
					MetricsSources: []autoscalingv1alpha1.MetricSource{{
						MetricSourceType: tt.metricSourceType,
						TargetMetric:     tt.targetMetric,
						TargetValue:      tt.targetValue,
					}},
				},
			}

			_, err := makeHPA(pa, context.NewBaseScalingContext())

			assert.ErrorContains(t, err, "must be representable as an HPA metric target")
		})
	}
}

func TestMakeHPAWithScheduledBounds(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-llm-pa",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test-llm",
			},
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: int32(10),
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "30",
			}},
		},
	}
	ctx := context.NewBaseScalingContext()

	hpa, err := makeHPAWithBounds(pa, ctx, paschedules.Bounds{MinReplicas: 3, MaxReplicas: 12, ActiveSchedule: "business-hours"})

	assert.NoError(t, err)
	assert.NotNil(t, hpa)
	assert.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(3), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(12), hpa.Spec.MaxReplicas)
}

func TestMakeHPAWithInvalidScheduledBounds(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{Name: "test-llm-pa", Namespace: "default"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "test-llm"},
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "30",
			}},
		},
	}

	_, err := makeHPAWithBounds(pa, context.NewBaseScalingContext(), paschedules.Bounds{MinReplicas: 3, MaxReplicas: 2})

	assert.ErrorIs(t, err, errInvalidHPABounds)
	assert.Equal(t, ReasonInvalidBounds, reasonForHPAGenerationError(err))
}

func TestReasonForHPAGenerationErrorDefaultsToMetricsConfig(t *testing.T) {
	assert.Equal(t, ReasonMetricsConfigError, reasonForHPAGenerationError(assert.AnError))
}

// The percent policies must match the rates the PodAutoscaler sets. Common rates such as
// 1.2 must not lose a point to float error, and a rate just above 1 must still give a
// positive policy value, which the API server requires.
func TestBuildHPABehaviorPolicyPercents(t *testing.T) {
	tests := []struct {
		rate             float64
		wantUp, wantDown int32
	}{
		{rate: 1.1, wantUp: 10, wantDown: 9},
		{rate: 1.15, wantUp: 15, wantDown: 13},
		{rate: 1.2, wantUp: 20, wantDown: 16},
		{rate: 1.25, wantUp: 25, wantDown: 20},
		{rate: 1.5, wantUp: 50, wantDown: 33},
		{rate: 3, wantUp: 200, wantDown: 66},
		{rate: 5, wantUp: 400, wantDown: 80},
		{rate: 1.005, wantUp: 1, wantDown: 1},
		// parseScaleRate has no upper bound, so a huge rate must not overflow int32.
		{rate: 21474838, wantUp: math.MaxInt32, wantDown: 99},
		{rate: 1e9, wantUp: math.MaxInt32, wantDown: 99},
		{rate: math.MaxFloat64, wantUp: math.MaxInt32, wantDown: 100},
		// An invalid rate below 1 falls back to 100% in both directions.
		{rate: 0.5, wantUp: 100, wantDown: 100},
		{rate: 0.99, wantUp: 100, wantDown: 100},
		{rate: 0, wantUp: 100, wantDown: 100},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatFloat(tt.rate, 'g', -1, 64), func(t *testing.T) {
			ctx := context.NewBaseScalingContext()
			ctx.MaxScaleUpRate, ctx.MaxScaleDownRate = tt.rate, tt.rate
			behavior := buildHPABehavior(ctx)
			require.Len(t, behavior.ScaleUp.Policies, 1)
			require.Len(t, behavior.ScaleDown.Policies, 1)
			assert.Equal(t, tt.wantUp, behavior.ScaleUp.Policies[0].Value, "scale-up percent")
			assert.Equal(t, tt.wantDown, behavior.ScaleDown.Policies[0].Value, "scale-down percent")
		})
	}
}

func TestBuildHPABehaviorScaleRates(t *testing.T) {
	tests := []struct {
		name               string
		scaleUp, scaleDown float64
	}{
		{"scale up disabled", 1, 2},
		{"scale down disabled", 2, 1},
		{"both disabled", 1, 1},
		{"rate two control", 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.NewBaseScalingContext()
			ctx.MaxScaleUpRate, ctx.MaxScaleDownRate = tt.scaleUp, tt.scaleDown
			behavior := buildHPABehavior(ctx)
			for _, direction := range []struct {
				rules        *autoscalingv2.HPAScalingRules
				rate         float64
				selectPolicy autoscalingv2.ScalingPolicySelect
				percent      int32
			}{
				{behavior.ScaleUp, tt.scaleUp, autoscalingv2.MaxChangePolicySelect, 100},
				{behavior.ScaleDown, tt.scaleDown, autoscalingv2.MinChangePolicySelect, 50},
			} {
				if direction.rate == 1 {
					assert.Equal(t, ptr.To(autoscalingv2.DisabledPolicySelect), direction.rules.SelectPolicy)
				} else {
					assert.Equal(t, ptr.To(direction.selectPolicy), direction.rules.SelectPolicy)
					assert.Equal(t, []autoscalingv2.HPAScalingPolicy{{
						Type: autoscalingv2.PercentScalingPolicy, Value: direction.percent, PeriodSeconds: 60,
					}}, direction.rules.Policies)
				}
				for _, policy := range direction.rules.Policies {
					assert.Greater(t, policy.Value, int32(0))
				}
			}
		})
	}
}
