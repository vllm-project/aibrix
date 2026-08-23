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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestPodAutoscalerCustomValidator_MetricsSources(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}
	validPA := func(sources ...autoscalingv1alpha1.MetricSource) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			MetricsSources:  sources,
		}}
	}
	validSource := autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.RESOURCE,
		TargetMetric:     "cpu",
		TargetValue:      "50",
	}

	t.Run("two valid sources", func(t *testing.T) {
		require.NoError(t, validator.validatePodAutoscaler(validPA(validSource, autoscalingv1alpha1.MetricSource{
			MetricSourceType: autoscalingv1alpha1.RESOURCE,
			TargetMetric:     "memory",
			TargetValue:      "128",
		})))
	})

	t.Run("empty sources", func(t *testing.T) {
		err := validator.validatePodAutoscaler(validPA())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one metricsSource")
	})

	t.Run("invalid second source", func(t *testing.T) {
		invalidSource := validSource
		invalidSource.TargetMetric = "disk"
		err := validator.validatePodAutoscaler(validPA(validSource, invalidSource))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.metricsSources[1].targetMetric")
	})
}

func TestPodAutoscalerCustomValidator_HPAMetricTargetBounds(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}
	tests := map[string]autoscalingv1alpha1.MetricSource{
		"cpu": {
			MetricSourceType: autoscalingv1alpha1.RESOURCE,
			TargetMetric:     "cpu",
			TargetValue:      "2147483647.1",
		},
		"memory": {
			MetricSourceType: autoscalingv1alpha1.RESOURCE,
			TargetMetric:     "memory",
			TargetValue:      "8796093022207.1",
		},
		"pods": {
			MetricSourceType: autoscalingv1alpha1.CUSTOM,
			TargetMetric:     "requests",
			TargetValue:      "9223372036854775808",
		},
	}

	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
				MaxReplicas:     10,
				ScalingStrategy: autoscalingv1alpha1.HPA,
				MetricsSources:  []autoscalingv1alpha1.MetricSource{source},
			}}

			err := validator.validatePodAutoscaler(pa)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be representable as an HPA metric target")
		})
	}
}

func TestPodAutoscalerCustomValidator_Schedules(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}
	validPA := func(schedules []autoscalingv1alpha1.PodAutoscalerSchedule) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
			MinReplicas:     ptr.To[int32](1),
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "50",
			}},
			Schedules: schedules,
		}}
	}

	t.Run("valid schedule", func(t *testing.T) {
		err := validator.validatePodAutoscaler(validPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
			Name:        "business-hours",
			Timezone:    "UTC",
			DaysOfWeek:  []string{"Mon", "Tue"},
			StartTime:   "09:00",
			EndTime:     "18:00",
			MinReplicas: ptr.To[int32](3),
			MaxReplicas: ptr.To[int32](12),
		}}))
		require.NoError(t, err)
	})

	t.Run("invalid schedule", func(t *testing.T) {
		err := validator.validatePodAutoscaler(validPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
			Name:        "bad-time",
			StartTime:   "9:00",
			EndTime:     "18:00",
			MinReplicas: ptr.To[int32](3),
		}}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.schedules")
		assert.Contains(t, err.Error(), "startTime")
	})
}

func TestPodAutoscalerCustomValidator_validatePodAutoscaler(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}

	tests := map[string]struct {
		pa          *autoscalingv1alpha1.PodAutoscaler
		expectError bool
		errorMsg    string
	}{
		"Valid Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: false,
		},
		"Kubernetes External Metrics Source": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.APA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.EXTERNAL,
							TargetMetric:     "aibrix_test_queue_depth",
							TargetValue:      "40",
						},
					},
				},
			},
			expectError: false,
		},
		"Kubernetes Domain Metrics Source": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.APA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.DOMAIN,
							TargetMetric:     "aibrix_test_queue_depth",
							TargetValue:      "40",
						},
					},
				},
			},
			expectError: false,
		},
		"Kubernetes External Metrics Source Requires TargetMetric": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy: autoscalingv1alpha1.APA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.EXTERNAL,
							TargetValue:      "40",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "targetMetric",
		},
		"Zero Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "0",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a finite number greater than 0",
		},
		"Negative Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "-5",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a finite number greater than 0",
		},
		"Invalid Number Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "abc",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a valid number",
		},
		"HPA Quantity Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "100m",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a valid number",
		},
		"NaN Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "NaN",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a finite number greater than 0",
		},
		"Positive Infinity Target Value": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "+Inf",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be a finite number greater than 0",
		},
		"Negative MinReplicas": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MinReplicas:     ptr.To[int32](-1),
					MaxReplicas:     10,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must not be negative",
		},
		"NonPositive MaxReplicas": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MinReplicas:     ptr.To[int32](0),
					MaxReplicas:     0,
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must be positive",
		},
		"HPA Does Not Support Role Subtarget": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-stormservice",
						Kind: "StormService",
					},
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
						RoleName: "decode",
					},
					ScalingStrategy: autoscalingv1alpha1.HPA,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "subTargetSelector",
		},
		"Observe Window Must Be Positive": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy:      autoscalingv1alpha1.KPA,
					ObserveWindowSeconds: ptr.To[int64](0),
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "observeWindowSeconds",
		},
		"Panic Window Must Be Positive": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy:    autoscalingv1alpha1.KPA,
					PanicWindowSeconds: ptr.To[int64](-1),
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "panicWindowSeconds",
		},
		"Observe Window Must Fit Time Duration": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy:      autoscalingv1alpha1.KPA,
					ObserveWindowSeconds: ptr.To[int64](3601),
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "less than or equal to 3600",
		},
		"Panic Window Must Not Exceed Observe Window": {
			pa: &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					ScalingStrategy:      autoscalingv1alpha1.KPA,
					ObserveWindowSeconds: ptr.To[int64](60),
					PanicWindowSeconds:   ptr.To[int64](120),
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "panicWindowSeconds",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tt.pa.Name = "test-pa"
			err := validator.validatePodAutoscaler(tt.pa)
			if tt.expectError {
				require.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}
