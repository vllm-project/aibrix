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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
		"Valid Scheduled Bounds": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "weekday-peak",
				Timezone:    "America/Los_Angeles",
				Cron:        "0 9 * * MON-FRI",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(4)),
			}),
			expectError: false,
		},
		"Scheduled Bounds Invalid Cron": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name: "invalid-cron", Cron: "*/5 * * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
			}),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[0].cron",
		},
		"Scheduled Bounds Invalid Duration": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name: "invalid-duration", Cron: "0 9 * * *", MinReplicas: ptr.To(int32(1)),
			}),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[0].duration",
		},
		"Scheduled Bounds Invalid Timezone": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name: "invalid-timezone", Timezone: "Mars/Olympus_Mons", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
			}),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[0].timezone",
		},
		"Scheduled Bounds Duplicate Name": {
			pa: podAutoscalerWithScheduledBounds(
				autoscalingv1alpha1.ScheduledReplicaBounds{Name: "peak", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(2))},
				autoscalingv1alpha1.ScheduledReplicaBounds{Name: "peak", Cron: "0 12 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(3))},
			),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[1].name",
		},
		"Scheduled Bounds Requires Override": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name: "missing-overrides", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour},
			}),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[0]",
		},
		"Scheduled Bounds Invalid Effective Bounds": {
			pa: podAutoscalerWithScheduledBounds(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name: "invalid-effective-bounds", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(11)),
			}),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[0]",
		},
		"Scheduled Bounds Overlap": {
			pa: podAutoscalerWithScheduledBounds(
				autoscalingv1alpha1.ScheduledReplicaBounds{Name: "morning", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: 2 * time.Hour}, MinReplicas: ptr.To(int32(2))},
				autoscalingv1alpha1.ScheduledReplicaBounds{Name: "late-morning", Cron: "0 10 * * *", Duration: metav1.Duration{time.Hour}, MinReplicas: ptr.To(int32(3))},
			),
			expectError: true,
			errorMsg:    "spec.scheduledBounds[1]",
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

func podAutoscalerWithScheduledBounds(scheduledBounds ...autoscalingv1alpha1.ScheduledReplicaBounds) *autoscalingv1alpha1.PodAutoscaler {
	return &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     10,
			ScheduledBounds: scheduledBounds,
			ScalingStrategy: autoscalingv1alpha1.HPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "50",
			}},
		},
	}
}
