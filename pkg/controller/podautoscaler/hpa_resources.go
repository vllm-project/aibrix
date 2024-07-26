/*
Copyright 2024.

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
	"context"
	pa_v1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strconv"
)

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

var (
	controllerKind = pa_v1.GroupVersion.WithKind("PodAutoScaler") // 设置控制器的资源类型
)

// getTarget extracts the autoscaling target value from the PodAutoscaler annotations.
// It returns the target as a float64. If the target annotation does not exist or cannot be converted to float64,
// the function returns 0.0 and whether get success.
func getTarget(pa *pa_v1.PodAutoscaler) (float64, bool) {
	// Retrieve the annotation value using the specific key.
	value, found := pa.GetAnnotations()["aibricks.pod.autoscaling/target"]
	if !found {
		// Return 0.0 and custom error if the annotation is not found.
		return 0.0, false
	}

	// Convert the string value to float64.
	targetValue, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// Return 0.0 and parsing error if conversion fails.
		return 0.0, false
	}

	return targetValue, true
}

func getMetric(pa *pa_v1.PodAutoscaler) string {
	value, found := pa.GetAnnotations()["aibricks.pod.autoscaling/metric"]
	if !found {
		return ""
	}
	return value
}

// MakeHPA creates an HPA resource from a PA resource.
func MakeHPA(pa *pa_v1.PodAutoscaler, ctx context.Context) *autoscalingv2.HorizontalPodAutoscaler {
	_log := log.FromContext(ctx)
	min_, max_ := pa.Spec.MinReplicas, pa.Spec.MaxReplicas
	if max_ == 0 {
		max_ = math.MaxInt32 // default to no limit
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pa.Name,
			Namespace:   pa.Namespace,
			Labels:      pa.Labels,
			Annotations: pa.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(pa.GetObjectMeta(), controllerKind),
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: pa.Spec.ScaleTargetRef.APIVersion,
				Kind:       pa.Spec.ScaleTargetRef.Kind,
				Name:       pa.Spec.ScaleTargetRef.Name,
			},
		},
	}
	hpa.Spec.MaxReplicas = max_
	if *min_ > 0 {
		hpa.Spec.MinReplicas = min_
	}

	if target, ok := getTarget(pa); ok {
		_log.Info("Make HPA, the metric is ", getMetric(pa), " the target is ", target)

		switch getMetric(pa) {
		case pa_v1.CPU:

			hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type: autoscalingv2.UtilizationMetricType,
						AverageUtilization: func() *int32 {
							util := int32(math.Ceil(target))
							return &util
						}(),
					},
				},
			}}

		case pa_v1.Memory:
			memory := resource.NewQuantity(int64(target)*1024*1024, resource.BinarySI)
			hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceMemory,
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: memory,
					},
				},
			}}
		default:
			if target, ok := getTarget(pa); ok {
				targetQuantity := resource.NewQuantity(int64(target), resource.DecimalSI)
				hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: getMetric(pa),
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: targetQuantity,
						},
					},
				}}
			}
		}
	}

	//if window, hasWindow := pa.Window(); hasWindow {
	//	windowSeconds := int32(window.Seconds())
	//	hpa.Spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{
	//		ScaleDown: &autoscalingv2.HPAScalingRules{
	//			StabilizationWindowSeconds: &windowSeconds,
	//		},
	//		ScaleUp: &autoscalingv2.HPAScalingRules{
	//			StabilizationWindowSeconds: &windowSeconds,
	//		},
	//	}
	//}

	return hpa
}
