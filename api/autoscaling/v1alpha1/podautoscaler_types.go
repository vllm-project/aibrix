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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
// Important: Run "make" to regenerate code after modifying this file

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// PodAutoscaler is the Schema for the podautoscalers API, a resource to scale Kubernetes pods based on observed metrics.
// The fields in the spec determine how the scaling behavior should be applied.
type PodAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired behavior of the PodAutoscaler.
	Spec PodAutoscalerSpec `json:"spec,omitempty"`

	// Status represents the current information about the PodAutoscaler.
	Status PodAutoscalerStatus `json:"status,omitempty"`
}

// PodAutoscalerSpec defines the desired state of PodAutoscaler
type PodAutoscalerSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// ScaleTargetRef points to scale-able resource that this PodAutoscaler should target and scale. e.g. Deployment
	ScaleTargetRef corev1.ObjectReference `json:"scaleTargetRef"`

	// PodSelector allows for more flexible selection of pods to scale based on labels.
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// MinReplicas is the minimum number of replicas to which the target can be scaled down.
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas to which the target can be scaled up.
	MaxReplicas int32 `json:"maxReplicas"`

	// MetricsSources defines a list of sources from which metrics are collected to make scaling decisions.
	MetricsSources []MetricSource `json:"metricsSources,omitempty"`

	// ScalingStrategy defines the strategy to use for scaling.
	// 1. "HPA" for using Kubernetes HPA
	// 2. "Custom" for a custom scaling mechanism.
	ScalingStrategy string `json:"scalingStrategy"`

	// ContainerConcurrency limits the number of concurrent requests to a single instance of the target. A value of 0 means unlimited.
	// TODO it's refer to knative-serving: pkg/apis/autoscaling/v1alpha1/pa_types.go
	//  knative contains this property, but should we involve it?
	ContainerConcurrency int64 `json:"containerConcurrency,omitempty"`

	// AdditionalConfig provides a place for custom settings that are not defined as standard fields.
	AdditionalConfig map[string]string `json:"additionalConfig,omitempty"`
}

// MetricSource defines an endpoint and path from which metrics are collected.
type MetricSource struct {
	// e.g. service1.example.com
	Endpoint string `json:"endpoint"`
	// e.g. /api/metrics/cpu
	Path string `json:"path"`
}

// PodAutoscalerStatus defines the observed state of PodAutoscaler
// PodAutoscalerStatus reflects the current state of the PodAutoscaler,
// including the current number of replicas, operational status, and other metrics.
type PodAutoscalerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// ServiceName is the name of the Kubernetes Service that routes traffic to the target, scaled by this PodAutoscaler.
	ServiceName string `json:"serviceName"`

	// MetricsServiceName is the name of the Kubernetes Service that provides monitoring data for the target.
	// TODO it's refer to knative-serving: pkg/apis/autoscaling/v1alpha1/pa_types.go
	MetricsServiceName string `json:"metricsServiceName,omitempty"`

	// DesiredScale represents the desired number of instances computed by the PodAutoscaler based on the current metrics.
	// it's computed according to Scaling policy after observing service metrics
	DesiredScale *int32 `json:"desiredScale,omitempty"`

	// ActualScale represents the actual number of running instances of the scaled target.
	// it may be different from DesiredScale
	ActualScale *int32 `json:"actualScale,omitempty"`
}

//+kubebuilder:object:root=true

// PodAutoscalerList contains a list of PodAutoscaler
type PodAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodAutoscaler{}, &PodAutoscalerList{})
}
