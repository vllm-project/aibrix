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

	pav1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

var (
	controllerKind = pav1.GroupVersion.WithKind("PodAutoScaler") // Define the resource type for the controller
)

func getHPANameFromPa(pa *pav1.PodAutoscaler) string {
	return fmt.Sprintf("%s-hpa", pa.Name)
}

// MakeHPA creates an HPA resource from a PodAutoscaler resource.
func makeHPA(pa *pav1.PodAutoscaler) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas, maxReplicas := pa.Spec.MinReplicas, pa.Spec.MaxReplicas
	// TODO: add some validation logics, has to be larger than minReplicas
	if maxReplicas == 0 {
		maxReplicas = math.MaxInt32 // Set default to no upper limit if not specified
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        getHPANameFromPa(pa),
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
			MaxReplicas: maxReplicas,
		},
	}
	if minReplicas != nil && *minReplicas > 0 {
		hpa.Spec.MinReplicas = minReplicas
	}

	if targetValue, err := strconv.ParseFloat(pa.Spec.TargetValue, 64); err != nil {
		klog.ErrorS(err, "Failed to parse target value")
	} else {
		klog.V(4).InfoS("Creating HPA", "metric", pa.Spec.TargetMetric, "target", targetValue)

		switch strings.ToLower(pa.Spec.TargetMetric) {
		case pav1.CPU:
			cpu := int32(math.Ceil(targetValue))
			hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &cpu,
					},
				},
			}}

		case pav1.Memory:
			memory := resource.NewQuantity(int64(targetValue)*1024*1024, resource.BinarySI)
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
			targetQuantity := resource.NewQuantity(int64(targetValue), resource.DecimalSI)
			hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{
						Name: pa.Spec.TargetMetric,
					},
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: targetQuantity,
					},
				},
			}}
		}
	}

	return hpa
}
