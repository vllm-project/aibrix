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

package wrapper

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// PodAutoscalerWrapper wraps PodAutoscaler to provide a fluent API for test construction.
type PodAutoscalerWrapper struct {
	autoscalingv1alpha1.PodAutoscaler
}

// MakePodAutoscaler creates a new PodAutoscalerWrapper with the given name.
func MakePodAutoscaler(name string) *PodAutoscalerWrapper {
	return &PodAutoscalerWrapper{
		PodAutoscaler: autoscalingv1alpha1.PodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

// Obj returns the pointer to the underlying PodAutoscaler object.
func (w *PodAutoscalerWrapper) Obj() *autoscalingv1alpha1.PodAutoscaler {
	return &w.PodAutoscaler
}

// Namespace sets the namespace of the PodAutoscaler.
func (w *PodAutoscalerWrapper) Namespace(namespace string) *PodAutoscalerWrapper {
	w.PodAutoscaler.Namespace = namespace
	return w
}

// ScalingStrategy sets the scaling strategy (HPA, KPA, or APA).
func (w *PodAutoscalerWrapper) ScalingStrategy(strategy autoscalingv1alpha1.ScalingStrategyType) *PodAutoscalerWrapper {
	w.Spec.ScalingStrategy = strategy
	return w
}

// MinReplicas sets the minimum number of replicas.
func (w *PodAutoscalerWrapper) MinReplicas(min int32) *PodAutoscalerWrapper {
	w.Spec.MinReplicas = &min
	return w
}

// MaxReplicas sets the maximum number of replicas.
func (w *PodAutoscalerWrapper) MaxReplicas(max int32) *PodAutoscalerWrapper {
	w.Spec.MaxReplicas = max
	return w
}

// ScaleTargetRef sets the target resource to scale.
func (w *PodAutoscalerWrapper) ScaleTargetRef(ref corev1.ObjectReference) *PodAutoscalerWrapper {
	w.Spec.ScaleTargetRef = ref
	return w
}

// ScaleTargetRefWithKind sets the target resource with kind, apiVersion, and name.
func (w *PodAutoscalerWrapper) ScaleTargetRefWithKind(kind, apiVersion, name string) *PodAutoscalerWrapper {
	w.Spec.ScaleTargetRef = corev1.ObjectReference{
		Kind:       kind,
		APIVersion: apiVersion,
		Name:       name,
	}
	return w
}

// SubTargetSelector sets the sub-target selector (e.g., for role-level scaling).
func (w *PodAutoscalerWrapper) SubTargetSelector(roleName string) *PodAutoscalerWrapper {
	w.Spec.SubTargetSelector = &autoscalingv1alpha1.SubTargetSelector{
		RoleName: roleName,
	}
	return w
}

// MetricSource sets a single metric source (replaces any existing).
func (w *PodAutoscalerWrapper) MetricSource(source autoscalingv1alpha1.MetricSource) *PodAutoscalerWrapper {
	w.Spec.MetricsSources = []autoscalingv1alpha1.MetricSource{source}
	return w
}

// AddMetricSource adds a metric source to the list.
func (w *PodAutoscalerWrapper) AddMetricSource(source autoscalingv1alpha1.MetricSource) *PodAutoscalerWrapper {
	w.Spec.MetricsSources = append(w.Spec.MetricsSources, source)
	return w
}

// Annotations sets annotations on the PodAutoscaler.
func (w *PodAutoscalerWrapper) Annotations(annotations map[string]string) *PodAutoscalerWrapper {
	if w.PodAutoscaler.Annotations == nil {
		w.PodAutoscaler.Annotations = make(map[string]string)
	}
	for k, v := range annotations {
		w.PodAutoscaler.Annotations[k] = v
	}
	return w
}

// Labels sets labels on the PodAutoscaler.
func (w *PodAutoscalerWrapper) Labels(labels map[string]string) *PodAutoscalerWrapper {
	if w.PodAutoscaler.Labels == nil {
		w.PodAutoscaler.Labels = make(map[string]string)
	}
	for k, v := range labels {
		w.PodAutoscaler.Labels[k] = v
	}
	return w
}

// MakeMetricSourcePod creates a POD-type metric source.
func MakeMetricSourcePod(
	protocolType autoscalingv1alpha1.ProtocolType,
	port, path, targetMetric, targetValue string,
) autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.POD,
		ProtocolType:     protocolType,
		Port:             port,
		Path:             path,
		TargetMetric:     targetMetric,
		TargetValue:      targetValue,
	}
}

// MakeMetricSourceResource creates a RESOURCE-type metric source (e.g., CPU, memory).
func MakeMetricSourceResource(targetMetric, targetValue string) autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.RESOURCE,
		TargetMetric:     targetMetric,
		TargetValue:      targetValue,
	}
}

// MakeMetricSourceExternal creates an EXTERNAL-type metric source.
func MakeMetricSourceExternal(
	protocolType autoscalingv1alpha1.ProtocolType,
	endpoint, path, targetMetric, targetValue string,
) autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.EXTERNAL,
		ProtocolType:     protocolType,
		Endpoint:         endpoint,
		Path:             path,
		TargetMetric:     targetMetric,
		TargetValue:      targetValue,
	}
}

// MakeMetricSourceCustom creates a CUSTOM-type metric source.
func MakeMetricSourceCustom(targetMetric, targetValue string) autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.CUSTOM,
		TargetMetric:     targetMetric,
		TargetValue:      targetValue,
	}
}
