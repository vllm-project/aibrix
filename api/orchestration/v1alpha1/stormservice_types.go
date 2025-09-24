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
	"strconv"
	"time"

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

	// CanaryStatus tracks the progress of canary deployments.
	// +optional
	CanaryStatus *CanaryStatus `json:"canaryStatus,omitempty"`
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

	// Canary defines the canary deployment strategy for gradual rollouts.
	// +optional
	Canary *CanaryUpdateStrategy `json:"canary,omitempty"`
}

// +enum
type StormServiceUpdateStrategyType string

const (
	// RollingUpdateStormServiceStrategyType replaces the old ReplicaSets by new one using rolling update i.e gradually scale down the old ReplicaSets and scale up the new one.
	RollingUpdateStormServiceStrategyType StormServiceUpdateStrategyType = "RollingUpdate"
	// InPlaceUpdateStormServiceStrategyType inplace updates the ReplicaSets
	InPlaceUpdateStormServiceStrategyType StormServiceUpdateStrategyType = "InPlaceUpdate"
)

//+genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.scalingTargetSelector
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`,description="Desired number of replicas"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`,description="Whether the StormService is ready"
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`,description="Whether the StormService is paused"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Time this StormService was created"

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

// CanaryUpdateStrategy defines the canary deployment configuration
type CanaryUpdateStrategy struct {
	// Steps defines the sequence of canary deployment steps
	Steps []CanaryStep `json:"steps,omitempty"`
}

// CanaryStep defines a single step in the canary deployment process
type CanaryStep struct {
	// SetWeight defines the percentage of traffic/replicas to route to the new version
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	SetWeight *int32 `json:"setWeight,omitempty"`

	// Pause defines a pause in the canary deployment
	// +optional
	Pause *PauseStep `json:"pause,omitempty"`
}

// PauseStep defines pause behavior in canary deployments
type PauseStep struct {
	// Duration specifies how long to pause
	// - String: "30s", "5m", etc. (parsed as time.Duration)
	// - Int: seconds as integer
	// - nil: manual pause requiring user intervention
	// Resume manual pause by setting duration to "0" or 0
	// +optional
	Duration *intstr.IntOrString `json:"duration,omitempty"`
}

// DurationSeconds converts the pause duration to seconds
// Returns:
// - >= 0: pause duration in seconds
// - 0: manual pause (nil duration) or resume (duration "0"/0)
// - -1: invalid duration string
func (p *PauseStep) DurationSeconds() int32 {
	if p.Duration == nil {
		return 0 // Manual pause
	}

	if p.Duration.Type == intstr.String {
		// Try parsing as integer first
		if s, err := strconv.ParseInt(p.Duration.StrVal, 10, 32); err == nil {
			return int32(s)
		}
		// Try parsing as duration string
		if d, err := time.ParseDuration(p.Duration.StrVal); err == nil {
			return int32(d.Seconds())
		}
		return -1 // Invalid string
	}

	return p.Duration.IntVal
}

// IsManualPause returns true if this is a manual pause (nil duration)
func (p *PauseStep) IsManualPause() bool {
	return p.Duration == nil
}

// IsResume returns true if this represents a resume action (duration 0 or "0")
func (p *PauseStep) IsResume() bool {
	if p.Duration == nil {
		return false
	}
	return p.DurationSeconds() == 0
}

// CanaryStatus tracks the progress of a canary deployment
type CanaryStatus struct {
	// CurrentStep is the index of the current step in the canary deployment
	// +optional
	CurrentStep int32 `json:"currentStep,omitempty"`

	// PauseConditions indicates the reasons why the canary deployment is paused
	// When paused, the first pause condition's StartTime indicates when the pause began
	// +optional
	PauseConditions []PauseCondition `json:"pauseConditions,omitempty"`

	// NOTE: Removed StableRevision and CanaryRevision fields
	// Use status.CurrentRevision for stable revision
	// Use status.UpdateRevision for canary revision

	// Phase indicates the current phase of the canary deployment
	// +optional
	Phase CanaryPhase `json:"phase,omitempty"`

	// NOTE: Removed CanaryReplicas and StableReplicas fields for replica mode
	// Use status.UpdatedReplicas for canary replica count
	// Calculate stable replicas as: status.Replicas - status.UpdatedReplicas

	// RoleCanaryCounts tracks per-role canary pod counts (pooled mode)
	// TODO(jiaxin): use top level status instead once the separate PR is merged.
	// +optional
	RoleCanaryCounts map[string]int32 `json:"roleCanaryCounts,omitempty"`

	// TotalCanaryPods is the total number of canary pods across all roles (pooled mode)
	// +optional
	TotalCanaryPods int32 `json:"totalCanaryPods,omitempty"`

	// AbortedAt indicates when the canary deployment was aborted
	// +optional
	AbortedAt *metav1.Time `json:"abortedAt,omitempty"`

	// Message provides details about the current canary state
	// +optional
	Message string `json:"message,omitempty"`
}

// CanaryPhase represents the phase of a canary deployment
// +enum
type CanaryPhase string

const (
	// CanaryPhaseInitializing indicates the canary deployment is starting
	CanaryPhaseInitializing CanaryPhase = "Initializing"
	// CanaryPhaseProgressing indicates the canary deployment is progressing through steps
	CanaryPhaseProgressing CanaryPhase = "Progressing"
	// CanaryPhasePaused indicates the canary deployment is paused
	CanaryPhasePaused CanaryPhase = "Paused"
	// CanaryPhaseCompleted indicates the canary deployment has completed successfully
	CanaryPhaseCompleted CanaryPhase = "Completed"
	// CanaryPhaseAborted indicates the canary deployment was aborted/rolled back
	CanaryPhaseAborted CanaryPhase = "Aborted"
)

// PauseReason represents the reason for a pause condition
// +enum
type PauseReason string

const (
	// PauseReasonCanaryPauseStep indicates a pause at a canary step
	PauseReasonCanaryPauseStep PauseReason = "CanaryPauseStep"
)

// PauseCondition represents a pause condition in the canary deployment
type PauseCondition struct {
	// Reason indicates why the canary deployment was paused
	Reason PauseReason `json:"reason"`
	// StartTime is when the pause condition was added
	StartTime metav1.Time `json:"startTime"`
}

func init() {
	SchemeBuilder.Register(&StormService{}, &StormServiceList{})
}
