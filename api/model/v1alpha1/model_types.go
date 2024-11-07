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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ModelSpec defines the desired state of Model
type ModelSpec struct {

	// Engine to be used for the server process.
	// +kubebuilder:validation:Enum=VLLM
	Engine string `json:"engine"`

	// Template describes the pods that will be created.
	// +kubebuilder:validation:Required
	Template v1.PodTemplateSpec `json:"template"`

	// Replicas is the desired number of replicas of model
	// +optional
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`
}

// ModelPhase is a string representation of the Model lifecycle phase.
type ModelPhase string

const (
	// ModelPending means the CR has been created and that's the initial status
	ModelPending ModelPhase = "Pending"
	// ModelDeploying means the Model is being deployed
	ModelDeploying ModelPhase = "Deploying"
	// ModelResourceCreated means the model owned resources have been created
	ModelResourceCreated ModelPhase = "ResourceCreated"
	// ModelRunning means Model has been running on the pod
	ModelRunning ModelPhase = "Running"
	// ModelFailed means Model has terminated in a failure
	ModelFailed ModelPhase = "Failed"
	// ModelUnknown means Model clean up some stable resources
	ModelUnknown ModelPhase = "Unknown"
)

// ModelStatus defines the observed state of Model
type ModelStatus struct {
	// Phase is a simple, high-level summary of where the Model is in its lifecycle
	// Phase maps to latest status.conditions.type
	// +optional
	Phase ModelPhase `json:"phase,omitempty"`
	// Conditions represents the observation of a model 's current state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ModelConditionType string

const (
	ModelConditionTypeInitialized     ModelConditionType = "Initialized"
	ModelConditionTypeDeployed        ModelConditionType = "Deployed"
	ModelConditionTypeResourceCreated ModelConditionType = "ResourceCreated"
	ModelConditionReady               ModelConditionType = "Ready"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Model is the Schema for the models API
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSpec   `json:"spec,omitempty"`
	Status ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ModelList contains a list of Model
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
