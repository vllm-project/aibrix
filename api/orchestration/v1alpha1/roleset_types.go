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
	"k8s.io/apimachinery/pkg/util/intstr"

	// let's temporarily use godel scheduler's definition, consider to switch to community definitions
	schedv1alpha1 "github.com/kubewharf/godel-scheduler-api/pkg/apis/scheduling/v1alpha1"
)

// RoleSetSpec defines the desired state of RoleSet
type RoleSetSpec struct {
	Roles []RoleSpec `json:"roles,omitempty"`

	// +optional
	UpdateStrategy RoleSetUpdateStrategyType `json:"updateStrategy,omitempty"`

	// +optional
	SchedulingStrategy SchedulingStrategy `json:"schedulingStrategy,omitempty"`
}

// +enum
type SchedulingStrategy struct {
	PodGroup *schedv1alpha1.PodGroupSpec `json:"podGroup,omitempty"`
}

// +enum
type RoleSetUpdateStrategyType string

const (
	// ParallelRoleSetUpdateStrategyType update all roles in parallel
	ParallelRoleSetUpdateStrategyType RoleSetUpdateStrategyType = "Parallel"
	// SequentialRoleSetStrategyType update all roles in sequential
	SequentialRoleSetStrategyType RoleSetUpdateStrategyType = "Sequential"
	// InterleaveRoleSetStrategyType update all roles in interleave, follow the rolling step defined in roles
	InterleaveRoleSetStrategyType RoleSetUpdateStrategyType = "Interleave"
)

type DisruptionTolerance struct {
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

type RoleSpec struct {
	Name string `json:"name,omitempty"`

	// Replicas is the number of desired replicas.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// UpgradeOrder specifies the order in which this role should be upgraded.
	// Lower values are upgraded first. If not specified, defaults to 0.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Type=integer
	// +kubebuilder:default:=0
	UpgradeOrder *int32 `json:"upgradeOrder,omitempty"`

	// PodGroupSize is the number of pods to form a minimum role instance.
	// +optional
	PodGroupSize *int32 `json:"podGroupSize,omitempty"`

	// +optional
	// +patchStrategy=retainKeys
	UpdateStrategy RoleUpdateStrategy `json:"updateStrategy,omitempty"`

	// +optional
	Stateful bool `json:"stateful,omitempty"`

	// +optional
	Template v1.PodTemplateSpec `json:"template,omitempty"`

	// DisruptionTolerance indicates how many pods can be unavailable during the preemption/eviction.
	// +optional
	DisruptionTolerance DisruptionTolerance `json:"disruptionTolerance,omitempty"`
}

// +enum
type RoleUpdateStrategy struct {
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty" protobuf:"bytes,1,opt,name=maxUnavailable"`

	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty" protobuf:"bytes,2,opt,name=maxSurge"`
}

// RoleSetStatus defines the observed state of RoleSet
type RoleSetStatus struct {
	Roles []RoleStatus `json:"roles,omitempty"`
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions Conditions `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// These are valid conditions of roleSet.
const (
	// RoleSetReady means the roleSet meeting the minimal requirements for the roleSet to be considered ready.
	RoleSetReady          ConditionType = "Ready"
	RoleSetReplicaFailure ConditionType = "ReplicaFailure"
	RoleSetProgressing    ConditionType = "Progressing"
)

type RoleStatus struct {
	Name string `json:"name,omitempty"`

	// Replicas is the most recently oberved number of replicas.
	Replicas int32 `json:"replicas"`

	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// +optional
	NotReadyReplicas int32 `json:"notReadyReplicas,omitempty"`

	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// +optional
	UpdatedReadyReplicas int32 `json:"updatedReadyReplicas,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// RoleSet is the Schema for the rolesets API
type RoleSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoleSetSpec   `json:"spec,omitempty"`
	Status RoleSetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// RoleSetList contains a list of RoleSet
type RoleSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoleSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RoleSet{}, &RoleSetList{})
}
