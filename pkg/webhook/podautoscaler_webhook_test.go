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
)

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
