/*
Copyright 2024 The Aibrix Team.

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
	"fmt"
	"math"
	"strconv"
	"strings"

	pav1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// MakeHPA creates an HPA resource from a PodAutoscaler resource.
func makeHPA(pa *pav1.PodAutoscaler, scalingContext scalingctx.ScalingContext) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	minReplicas, maxReplicas := pa.Spec.MinReplicas, pa.Spec.MaxReplicas
	if maxReplicas == 0 {
		maxReplicas = math.MaxInt32 // Set default to no upper limit if not specified
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-hpa", pa.Name),
			Namespace:   pa.Namespace,
			Labels:      pa.Labels,
			Annotations: pa.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(pa, pav1.GroupVersion.WithKind("PodAutoscaler")),
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: pa.Spec.ScaleTargetRef.APIVersion,
				Kind:       pa.Spec.ScaleTargetRef.Kind,
				Name:       pa.Spec.ScaleTargetRef.Name,
			},
			MaxReplicas: maxReplicas,
			Behavior:    buildHPABehavior(scalingContext),
		},
	}
	if minReplicas != nil && *minReplicas > 0 {
		hpa.Spec.MinReplicas = minReplicas
		// if minReplicas exist, check validation of minReplicas and maxReplicas
		if maxReplicas < *minReplicas {
			return nil, fmt.Errorf("HPA Strategy: maxReplicas %d must be equal or larger than minReplicas %d", maxReplicas, *minReplicas)
		}
	}

	hpa.Spec.Metrics = []autoscalingv2.MetricSpec{}
	sources := pa.Spec.MetricsSources
	if len(sources) == 0 {
		return nil, fmt.Errorf("HPA Strategy: no metric source is specified")
	}

	for _, source := range sources {
		if targetValue, err := strconv.ParseFloat(source.TargetValue, 64); err != nil {
			return nil, fmt.Errorf("failed to parse target value of the metric source: %w", err)
		} else {
			klog.V(4).InfoS("Creating HPA", "metric", source.TargetMetric, "target", targetValue)

			switch strings.ToLower(source.TargetMetric) {
			case pav1.CPU:
				cpu := int32(math.Ceil(targetValue))
				hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: &cpu,
						},
					},
				})

			case pav1.Memory:
				memory := resource.NewQuantity(int64(targetValue)*1024*1024, resource.BinarySI)
				hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceMemory,
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: memory,
						},
					},
				})

			default:
				targetQuantity := resource.NewQuantity(int64(targetValue), resource.DecimalSI)
				hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: source.TargetMetric,
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: targetQuantity,
						},
					},
				})
			}
		}
	}

	return hpa, nil
}

// buildHPABehavior creates HPA behavior configuration from ScalingContext
func buildHPABehavior(scalingContext scalingctx.ScalingContext) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	// Extract configuration from ScalingContext (single source of truth)
	scaleUpStabilization := int32(scalingContext.GetScaleUpCooldownWindow().Seconds())
	scaleDownStabilization := int32(scalingContext.GetScaleDownCooldownWindow().Seconds())
	maxScaleUpRate := scalingContext.GetMaxScaleUpRate()
	maxScaleDownRate := scalingContext.GetMaxScaleDownRate()

	// Convert rate to percent for K8s HPA
	// max-scale-up-rate: 2.0 means "can double" -> 100% increase
	scaleUpPercent := int32((maxScaleUpRate - 1.0) * 100)
	if scaleUpPercent < 0 {
		scaleUpPercent = 100 // Minimum 100% to allow scaling
	}

	// max-scale-down-rate: 2.0 means "can halve" -> 50% decrease
	scaleDownPercent := int32((1.0 - 1.0/maxScaleDownRate) * 100)
	if scaleDownPercent < 0 {
		scaleDownPercent = 100 // Minimum 100% to allow scaling
	}

	// Build behavior spec
	maxPolicy := autoscalingv2.MaxChangePolicySelect
	minPolicy := autoscalingv2.MinChangePolicySelect
	periodSeconds := int32(60)

	behavior := &autoscalingv2.HorizontalPodAutoscalerBehavior{
		ScaleUp: &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: &scaleUpStabilization,
			SelectPolicy:               &maxPolicy,
			Policies: []autoscalingv2.HPAScalingPolicy{
				{
					Type:          autoscalingv2.PercentScalingPolicy,
					Value:         scaleUpPercent,
					PeriodSeconds: periodSeconds,
				},
			},
		},
		ScaleDown: &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: &scaleDownStabilization,
			SelectPolicy:               &minPolicy,
			Policies: []autoscalingv2.HPAScalingPolicy{
				{
					Type:          autoscalingv2.PercentScalingPolicy,
					Value:         scaleDownPercent,
					PeriodSeconds: periodSeconds,
				},
			},
		},
	}

	klog.V(4).InfoS("Built HPA behavior",
		"scaleUpStabilization", scaleUpStabilization,
		"scaleDownStabilization", scaleDownStabilization,
		"scaleUpPercent", scaleUpPercent,
		"scaleDownPercent", scaleDownPercent)

	return behavior
}
