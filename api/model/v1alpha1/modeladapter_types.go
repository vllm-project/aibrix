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

// ModelAdapterSpec defines the desired state of ModelAdapter
type ModelAdapterSpec struct {

	// BaseModel is the identifier for the base model to which the ModelAdapter will be attached.
	BaseModel string `json:"baseModel,omitempty"`

	// PodSelector is a label query over pods that should match the ModelAdapter configuration.
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// SchedulerName is the name of the scheduler to use for scheduling the ModelAdapter.
	SchedulerName string `json:"schedulerName,omitempty"`

	// Additional fields can be added here to customize the scheduling and deployment
	// +optional
	AdditionalConfig map[string]string `json:"additionalConfig,omitempty"`
}

// ModelAdapterPhase is a string representation of the ModelAdapter lifecycle phase.
type ModelAdapterPhase string

const (
	// ModelAdapterPending means the CR has been created and that's the initial status
	ModelAdapterPending ModelAdapterPhase = "Pending"
	// ModelAdapterScheduling means the ModelAdapter is pending scheduling
	ModelAdapterScheduling ModelAdapterPhase = "Scheduling"
	// ModelAdapterBinding means the controller starts to load ModelAdapter on a selected pod
	ModelAdapterBinding ModelAdapterPhase = "Binding"
	// ModelAdapterConfiguring means the controller starts to configure the service and endpoint for ModelAdapter
	ModelAdapterConfiguring ModelAdapterPhase = "Configuring"
	// ModelAdapterRunning means ModelAdapter has been running on the pod
	ModelAdapterRunning ModelAdapterPhase = "Running"
	// ModelAdapterFailed means ModelAdapter has terminated in a failure
	ModelAdapterFailed ModelAdapterPhase = "Failed"
	// ModelAdapterScaling means ModelAdapter is scaling, could be scaling in or out
	ModelAdapterScaling ModelAdapterPhase = "Scaling"
)

// ModelAdapterStatus defines the observed state of ModelAdapter
type ModelAdapterStatus struct {

	// Phase is a simple, high-level summary of where the ModelAdapter is in its lifecycle
	// +optional
	Phase ModelAdapterPhase `json:"phase,omitempty"`
	// Conditions represents the latest available observations of an object's state
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []ModelAdapterCondition `json:"conditions,omitempty"`
	// LastTransitionTime is the time the last Phase transitioned to the current one
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// Reason is a unique, one-word, CamelCase reason for the phase's last transition
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message indicating details about the last transition
	// +optional
	Message string `json:"message,omitempty"`
	// Instances lists all pod instances of ModelAdapter
	// +optional
	Instances []string `json:"instances,omitempty"`
}

type ModelAdapterConditionType string

// ModelAdapterCondition contains details for the current condition of this ModelAdapter
type ModelAdapterCondition struct {
	// Type is the type of the condition
	Type ModelAdapterConditionType `json:"type"`
	// Status is the status of the condition
	Status corev1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time the condition last transitioned from one status to another
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	// Reason is a unique, one-word, CamelCase reason for the condition's last transition
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message indicating details about the transition
	// +optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// ModelAdapter is the Schema for the modeladapters API
type ModelAdapter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelAdapterSpec   `json:"spec,omitempty"`
	Status ModelAdapterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ModelAdapterList contains a list of ModelAdapter
type ModelAdapterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelAdapter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelAdapter{}, &ModelAdapterList{})
}
