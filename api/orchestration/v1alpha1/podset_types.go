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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodSetSpec defines the desired state of a PodSet
// PodSet represents one atomic group of pods that work together
type PodSetSpec struct {
	// PodGroupSize is the number of pods in this set
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=100
	PodGroupSize int32 `json:"podGroupSize"`

	// Template is the pod template for pods in this set
	Template v1.PodTemplateSpec `json:"template"`

	// Stateful indicates whether pods should have stable network identities
	// +optional
	Stateful bool `json:"stateful,omitempty"`

	// RecoveryPolicy defines how pods in the set are recreated when failures happen
	// +kubebuilder:default=ReplaceUnhealthy
	// +kubebuilder:validation:Enum={ReplaceUnhealthy,Recreate}
	// +optional
	RecoveryPolicy PodRecoveryPolicy `json:"recoveryPolicy,omitempty"`

	// +optional
	SchedulingStrategy *SchedulingStrategy `json:"schedulingStrategy,omitempty"`
}

// PodSetStatus defines the observed state of PodSet
type PodSetStatus struct {
	// ReadyPods is the number of pods in this set that are ready
	ReadyPods int32 `json:"readyPods"`

	// TotalPods is the total number of pods in this set
	TotalPods int32 `json:"totalPods"`

	// Phase represents the current lifecycle phase of the pod set
	Phase PodSetPhase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the PodSet
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions Conditions `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// PodSetPhase represents the current lifecycle phase of a PodSet
// +enum
type PodSetPhase string

const (
	// PodSetPhasePending indicates the PodSet is being created
	PodSetPhasePending PodSetPhase = "Pending"

	// PodSetPhaseRunning indicates at least one pod is running
	PodSetPhaseRunning PodSetPhase = "Running"

	// PodSetPhaseReady indicates all pods in the set are ready
	PodSetPhaseReady PodSetPhase = "Ready"

	// PodSetPhaseFailed indicates the PodSet has failed
	PodSetPhaseFailed PodSetPhase = "Failed"
)

// PodRecoveryPolicy defines how a PodSet handles pod recreation when pods are lost or fail.
// +enum
type PodRecoveryPolicy string

const (
	// ReplaceUnhealthy means only missing pods (by index) will be recreated.
	ReplaceUnhealthy PodRecoveryPolicy = "ReplaceUnhealthy"

	// RecreatePodRecreateStrategy means if any pod is missing, all pods in the set will be deleted, and then all pods will be recreated.
	RecreatePodRecreateStrategy PodRecoveryPolicy = "Recreate"
)

// PodSet conditions
const (
	// PodSetReady indicates whether all pods in the set are ready
	PodSetReady ConditionType = "Ready"

	// PodSetProgressing indicates the PodSet is in the process of updating
	PodSetProgressing ConditionType = "Progressing"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ReadyPods",type=integer,JSONPath=`.status.readyPods`
// +kubebuilder:printcolumn:name="TotalPods",type=integer,JSONPath=`.status.totalPods`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Time this PodSet was created"

// PodSet is the Schema for the podsets API
// PodSet represents an atomic group of pods that work together
// This is an internal API used by RoleSet controller when podGroupSize > 1
type PodSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodSetSpec   `json:"spec,omitempty"`
	Status PodSetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PodSetList contains a list of PodSet
type PodSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodSet{}, &PodSetList{})
}
