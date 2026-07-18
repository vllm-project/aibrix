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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestPodAutoscalerCustomDefaulter_DefaultCircuitBreaker(t *testing.T) {
	defaulter := &PodAutoscalerCustomDefaulter{}

	t.Run("enabled configuration gets defaults", func(t *testing.T) {
		pa := &autoscalingv1alpha1.PodAutoscaler{
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				CircuitBreaker: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true},
			},
		}

		require.NoError(t, defaulter.Default(context.Background(), pa))
		require.NotNil(t, pa.Spec.CircuitBreaker)
		assert.Equal(t, autoscalingv1alpha1.CircuitBreakerActionFreeze, pa.Spec.CircuitBreaker.Action)
		assert.Equal(t, int32(3), pa.Spec.CircuitBreaker.FailureThreshold)
		assert.Equal(t, int32(3), pa.Spec.CircuitBreaker.RecoveryThreshold)
	})

	t.Run("absent configuration remains absent", func(t *testing.T) {
		pa := &autoscalingv1alpha1.PodAutoscaler{}

		require.NoError(t, defaulter.Default(context.Background(), pa))
		assert.Nil(t, pa.Spec.CircuitBreaker)
	})
}

func TestPodAutoscalerCustomValidator_CircuitBreaker(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}
	validPA := func(strategy autoscalingv1alpha1.ScalingStrategyType, cfg *autoscalingv1alpha1.CircuitBreakerConfig) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
				ScalingStrategy: strategy,
				MetricsSources: []autoscalingv1alpha1.MetricSource{{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "50",
				}},
				CircuitBreaker: cfg,
			},
		}
	}

	tests := []struct {
		name    string
		pa      *autoscalingv1alpha1.PodAutoscaler
		wantErr string
	}{
		{name: "KPA enabled", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true})},
		{name: "APA enabled", pa: validPA(autoscalingv1alpha1.APA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true})},
		{name: "HPA enabled", pa: validPA(autoscalingv1alpha1.HPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}), wantErr: "spec.circuitBreaker"},
		{name: "HPA disabled", pa: validPA(autoscalingv1alpha1.HPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: false})},
		{name: "invalid action", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: "panic"}), wantErr: "spec.circuitBreaker.action"},
		{name: "invalid failure threshold", pa: validPA(autoscalingv1alpha1.APA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, FailureThreshold: -1}), wantErr: "spec.circuitBreaker.failureThreshold"},
		{name: "invalid recovery threshold", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, RecoveryThreshold: -1}), wantErr: "spec.circuitBreaker.recoveryThreshold"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.validatePodAutoscaler(tt.pa)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPodAutoscalerCustomValidator_MetricsSources(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}
	validPA := func(sources ...autoscalingv1alpha1.MetricSource) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
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
			TargetValue:      "1Gi",
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
			errorMsg:    "must be greater than 0",
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
			errorMsg:    "must be greater than 0",
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
