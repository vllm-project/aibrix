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

	//// PodSelector allows for more flexible selection of pods to scale based on labels.
	//PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// MinReplicas is the minimum number of replicas to which the target can be scaled down.
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas to which the target can be scaled up.
	// It cannot be less than minReplicas
	MaxReplicas int32 `json:"maxReplicas"`

	TargetMetric string `json:"targetMetric"`

	TargetValue string `json:"targetValue"`

	// MetricsSources defines a list of sources from which metrics are collected to make scaling decisions.
	MetricsSources []MetricSource `json:"metricsSources,omitempty"`

	// ScalingStrategy defines the strategy to use for scaling.
	ScalingStrategy ScalingStrategyType `json:"scalingStrategy"`
}

// ScalingStrategyType defines the type for scaling strategies.
type ScalingStrategyType string

const (
	// HPA represents the Kubernetes native Horizontal Pod Autoscaler.
	HPA ScalingStrategyType = "HPA"

	// KPA represents the KNative Pod Autoscaling Algorithms
	KPA ScalingStrategyType = "KPA"

	// Custom represents any custom scaling mechanism.
	Custom ScalingStrategyType = "Custom"
)

// MetricSource defines an endpoint and path from which metrics are collected.
type MetricSource struct {
	// e.g. service1.example.com
	Endpoint string `json:"endpoint"`
	// e.g. /api/metrics/cpu
	Path string `json:"path"`
}

// PodAutoscalerStatus defines the observed state of PodAutoscaler
// including the current number of replicas, operational status, and other metrics.
type PodAutoscalerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// LastScaleTime is the last time the PodAutoscaler scaled the number of pods,
	// used by the autoscaler to control how often the number of pods is changed.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// DesiredScale represents the desired number of instances computed by the PodAutoscaler based on the current metrics.
	// it's computed according to Scaling policy after observing service metrics
	DesiredScale int32 `json:"desiredScale,omitempty"`

	// ActualScale represents the actual number of running instances of the scaled target.
	// it may be different from DesiredScale
	ActualScale int32 `json:"actualScale,omitempty"`

	// Conditions is the set of conditions required for this autoscaler to scale its target,
	// and indicates whether or not those conditions are met.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

const (
	// CPU is the amount of the requested cpu actually being consumed by the Pod.
	CPU = "cpu"
	// Memory is the amount of the requested memory actually being consumed by the Pod.
	Memory = "memory"
	// QPS is the requests per second reaching the Pod.
	QPS = "qps"
)
