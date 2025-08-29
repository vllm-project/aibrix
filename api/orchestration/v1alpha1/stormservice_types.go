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
	"k8s.io/apimachinery/pkg/util/intstr"
)

// StormServiceSpec defines the desired state of StormService
type StormServiceSpec struct {
	// Number of desired roleSets. This is a pointer to distinguish between explicit
	// zero and not specified. Defaults to 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Label selector for roleSets. Existing ReplicaSets whose roleSets are
	// selected by this will be the ones affected by this stormService.
	// It must match the roleSet template's labels.
	Selector *metav1.LabelSelector `json:"selector"`

	// Stateful indicates whether service is stateful
	Stateful bool `json:"stateful,omitempty"`

	// Template describes the roleSets that will be created.
	Template RoleSetTemplateSpec `json:"template"`

	// The deployment strategy to use to replace existing roleSets with new ones.
	// +optional
	// +patchStrategy=retainKeys
	UpdateStrategy StormServiceUpdateStrategy `json:"updateStrategy,omitempty"`

	// The number of old ReplicaSets to retain to allow rollback.
	// This is a pointer to distinguish between explicit zero and not specified.
	// Defaults to 10.
	// +optional
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty" protobuf:"varint,6,opt,name=revisionHistoryLimit"`

	// Indicates that the stormService is paused.
	// +optional
	Paused bool `json:"paused,omitempty" protobuf:"varint,7,opt,name=paused"`

	// DisruptionTolerance indicates how many roleSets can be unavailable during the preemption/eviction.
	// +optional
	DisruptionTolerance DisruptionTolerance `json:"disruptionTolerance,omitempty"`
}

const (
	// DefaultStormServiceUniqueLabelKey is the default key of the selector that is added
	// to existing ReplicaSets (and label key that is added to its workloads) to prevent the existing ReplicaSets
	// to select new workloads (and old workloads being select by new ReplicaSet).
	DefaultStormServiceUniqueLabelKey string = "stormservice-template-hash"
)

type RoleSetTemplateSpec struct {
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// +optional
	Spec *RoleSetSpec `json:"spec,omitempty"`
}

// StormServiceStatus defines the observed state of StormService
type StormServiceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Total number of non-terminated roleSets targeted by this stormService (their labels match the selector).
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Total number of ready roleSets targeted by this stormService.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Total number of notReady roleSets targeted by this stormService.
	// +optional
	NotReadyReplicas int32 `json:"notReadyReplicas,omitempty"`

	// currentReplicas is the number of roleSets created by the stormService controller from the stormService version
	// indicated by currentRevision.
	CurrentReplicas int32 `json:"currentReplicas,omitempty" protobuf:"varint,4,opt,name=currentReplicas"`

	// updatedReplicas is the number of roleSets created by the stormService controller from the stormService version
	// indicated by updateRevision.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty" protobuf:"varint,5,opt,name=updatedReplicas"`

	// currentRevision, if not empty, indicates the version of the stormService used to generate roleSets in current state.
	CurrentRevision string `json:"currentRevision,omitempty" protobuf:"bytes,6,opt,name=currentRevision"`

	// updateRevision, if not empty, indicates the version of the stormService used to generate roleSets in update state.
	UpdateRevision string `json:"updateRevision,omitempty" protobuf:"bytes,7,opt,name=updateRevision"`

	// updatedReadyReplicas is the number of roleSets created by the stormService controller from the stormService version
	// indicated by updateRevision, and in ready state.
	UpdatedReadyReplicas int32 `json:"updatedReadyReplicas,omitempty" protobuf:"varint,8,opt,name=updatedReadyReplicas"`

	// +optional
	Conditions Conditions `json:"conditions,omitempty"`

	// collisionCount is the count of hash collisions for the StormService. The StormService controller
	// uses this field as a collision avoidance mechanism when it needs to create the name for the newest ControllerRevision.
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty"`

	// The label selector information of the pods belonging to the StormService object.
	ScalingTargetSelector string `json:"scalingTargetSelector,omitempty"`
}

// These are valid conditions of a stormService.
const (
	// StormServiceReady means the stormService is ready, ie. at least the minimum available
	// replicas required are up.
	StormServiceReady ConditionType = "Ready"
	// StormServiceProgressing means the stormService is progressing. Progress for a stormService is
	// considered when it has more than one active replicaSet.
	StormServiceProgressing ConditionType = "Progressing"
	// StormServiceReplicaFailure is added in a stormService when one of its workloads fails to be created
	// or deleted.
	StormServiceReplicaFailure ConditionType = "ReplicaFailure"
)

type StormServiceUpdateStrategy struct {
	// Type of update strategy. Can be "RollingUpdate". Default is RollingUpdate.
	// +optional
	// +kubebuilder:default=RollingUpdate
	// +kubebuilder:validation:Enum={RollingUpdate,InPlaceUpdate}
	Type StormServiceUpdateStrategyType `json:"type,omitempty"`

	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty" protobuf:"bytes,1,opt,name=maxUnavailable"`

	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty" protobuf:"bytes,2,opt,name=maxSurge"`
}

// +enum
type StormServiceUpdateStrategyType string

const (
	// RollingUpdateStormServiceStrategyType replaces the old ReplicaSets by new one using rolling update i.e gradually scale down the old ReplicaSets and scale up the new one.
	RollingUpdateStormServiceStrategyType StormServiceUpdateStrategyType = "RollingUpdate"
	// InPlaceUpdateStormServiceStrategyType inplace updates the ReplicaSets
	InPlaceUpdateStormServiceStrategyType StormServiceUpdateStrategyType = "InPlaceUpdate"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.scalingTargetSelector

// StormService is the Schema for the stormservices API
type StormService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StormServiceSpec   `json:"spec,omitempty"`
	Status StormServiceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// StormServiceList contains a list of StormService
type StormServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StormService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StormService{}, &StormServiceList{})
}
