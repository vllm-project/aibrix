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

package v1alpha1

import (
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
	// ModelAdapterBinding means the controller loads ModelAdapter on a selected pod
	ModelAdapterBinding ModelAdapterPhase = "Binding"
	// ModelAdapterRunning means ModelAdapter has been running on the pod
	ModelAdapterRunning ModelAdapterPhase = "Running"
	// ModelAdapterFailed means ModelAdapter has terminated in a failure
	ModelAdapterFailed ModelAdapterPhase = "Failed"
	// ModelAdapterScaling means ModelAdapter is scaling, could be scaling in or out. won't be enabled until we allow multiple replicas
	ModelAdapterScaling ModelAdapterPhase = "Scaling"
)

// ModelAdapterStatus defines the observed state of ModelAdapter
type ModelAdapterStatus struct {
	// Phase is a simple, high-level summary of where the ModelAdapter is in its lifecycle
	// Phase maps to latest status.conditions.type
	// +optional
	Phase ModelAdapterPhase `json:"phase,omitempty"`
	// Conditions represents the observation of a model adapter's current state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Instances lists all pod instances of ModelAdapter
	// +optional
	Instances []string `json:"instances,omitempty"`
}

type ModelAdapterConditionType string

const (
	ModelAdapterConditionTypeInitialized     ModelAdapterConditionType = "Initialized"
	ModelAdapterConditionTypeSelectorMatched ModelAdapterConditionType = "SelectorMatched"
	ModelAdapterConditionTypeScheduled       ModelAdapterConditionType = "Scheduled"
	ModelAdapterConditionTypeResourceCreated ModelAdapterConditionType = "ResourceCreated"
	ModelAdapterConditionReady               ModelAdapterConditionType = "Ready"
	ModelAdapterConditionCleanup             ModelAdapterConditionType = "Cleanup"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ModelAdapter is the Schema for the modeladapters API
type ModelAdapter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelAdapterSpec   `json:"spec,omitempty"`
	Status ModelAdapterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ModelAdapterList contains a list of ModelAdapter
type ModelAdapterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelAdapter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelAdapter{}, &ModelAdapterList{})
}
