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
	"testing"

	"github.com/stretchr/testify/assert"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"

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
							Name: "kv_cache_usage_perc",
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
