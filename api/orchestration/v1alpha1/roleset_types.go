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
	// +kubebuilder:validation:Enum={Parallel,Sequential,Interleave}
	// +kubebuilder:default=Sequential
	UpdateStrategy RoleSetUpdateStrategyType `json:"updateStrategy,omitempty"`

	// +optional
	SchedulingStrategy *SchedulingStrategy `json:"schedulingStrategy,omitempty"`

	// +optional
	TopologyPolicy *TopologyPolicy `json:"topologyPolicy,omitempty"`
}

// +kubebuilder:validation:MaxProperties=1
type SchedulingStrategy struct {
	GodelSchedulingStrategy *GodelSchedulingStrategySpec `json:"godelSchedulingStrategy,omitempty"`

	CoschedulingSchedulingStrategy *CoschedulingSchedulingStrategySpec `json:"coschedulingSchedulingStrategy,omitempty"`

	VolcanoSchedulingStrategy *VolcanoSchedulingStrategySpec `json:"volcanoSchedulingStrategy,omitempty"`
}

// GodelSchedulingStrategySpec uses godel scheduler's podgroup definition
type GodelSchedulingStrategySpec struct {
	// MinMember defines the minimal number of members/tasks to run the pod group;
	// if there's not enough resources to start all tasks, the scheduler
	// will not start anyone.
	MinMember int32 `json:"minMember,omitempty"`

	// If specified, indicates the PodGroup's priority. "system-node-critical" and
	// "system-cluster-critical" are two special reserved keywords which indicate the highest priorities.
	// Any other name must be defined by creating a PriorityClass object with that name.
	// If not specified, the PodGroup priority will be default.
	// If default priority class doesn't exist, the PodGroup priority will be zero.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// ScheduleTimeoutSeconds defines the maximal time of tasks to wait before run the pod group;
	// +optional
	ScheduleTimeoutSeconds *int32 `json:"scheduleTimeoutSeconds,omitempty"`

	// Application indicates the podGroup belongs to a logical Application
	// This will be used for coordinate with features like drf and faire share.
	// +optional
	Application string `json:"application,omitempty"`

	// Affinity shows the affinity/anti-affinity rules that scheduler needs to follow
	// when scheduling instances of this pod group.
	// +optional
	Affinity *schedv1alpha1.Affinity `json:"affinity,omitempty"`
}

// CoschedulingSchedulingStrategySpec uses coscheduling scheduler-plugin's podgroup definition
type CoschedulingSchedulingStrategySpec struct {
	// MinMember defines the minimal number of members/tasks to run the pod group;
	// if there's not enough resources to start all tasks, the scheduler
	// will not start any.
	// The minimum is 1
	// +kubebuilder:validation:Minimum=1
	MinMember int32 `json:"minMember,omitempty"`

	// MinResources defines the minimal resource of members/tasks to run the pod group;
	// if there's not enough resources to start all tasks, the scheduler
	// will not start any.
	MinResources v1.ResourceList `json:"minResources,omitempty"`

	// ScheduleTimeoutSeconds defines the maximal time of members/tasks to wait before run the pod group;
	ScheduleTimeoutSeconds *int32 `json:"scheduleTimeoutSeconds,omitempty"`
}

// VolcanoSchedulingStrategySpec uses volcano's podgroup definition
type VolcanoSchedulingStrategySpec struct {
	// MinMember defines the minimal number of members/tasks to run the pod group;
	// if there's not enough resources to start all tasks, the scheduler
	// will not start anyone.
	MinMember int32 `json:"minMember,omitempty" protobuf:"bytes,1,opt,name=minMember"`

	// MinTaskMember defines the minimal number of pods to run each task in the pod group;
	// if there's not enough resources to start each task, the scheduler
	// will not start anyone.
	MinTaskMember map[string]int32 `json:"minTaskMember,omitempty" protobuf:"bytes,1,opt,name=minTaskMember"`

	// Queue defines the queue to allocate resource for PodGroup; if queue does not exist,
	// the PodGroup will not be scheduled. Defaults to `default` Queue with the lowest weight.
	// +optional
	Queue string `json:"queue,omitempty" protobuf:"bytes,2,opt,name=queue"`

	// If specified, indicates the PodGroup's priority. "system-node-critical" and
	// "system-cluster-critical" are two special keywords which indicate the
	// highest priorities with the former being the highest priority. Any other
	// name must be defined by creating a PriorityClass object with that name.
	// If not specified, the PodGroup priority will be default or zero if there is no
	// default.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty" protobuf:"bytes,3,opt,name=priorityClassName"`

	// MinResources defines the minimal resource of members/tasks to run the pod group;
	// if there's not enough resources to start all tasks, the scheduler
	// will not start anyone.
	MinResources v1.ResourceList `json:"minResources,omitempty" protobuf:"bytes,4,opt,name=minResources"`
}

type TopologyScope string

const (
	TopologyStormServiceScope TopologyScope = "StormService"
	TopologyRoleSetScope      TopologyScope = "RoleSet"
	TopologyRoleScope         TopologyScope = "Role"
)

// TopologyPolicy specifies how Pods are co-located based on Kubernetes topology keys.
type TopologyPolicy struct {
	// Scope defines the granularity of co-location.
	// Valid values are:
	// - "StormService": All Pods in the entire StormService share the same topology value.
	// - "RoleSet": All Pods within each RoleSet share the same topology value (different RoleSets may be on different domains).
	// - "Role": All Pods of the same role (across all RoleSets) share the same topology value.
	// +kubebuilder:validation:Enum=StormService;RoleSet;Role
	// +optional
	Scope TopologyScope `json:"scope,omitempty"`

	// Key is the Kubernetes topology label key to enforce co-location on.
	// Common values include:
	//   - "kubernetes.io/hostname" (node-level)
	//   - "topology.kubernetes.io/zone" (zone-level)
	// Required when Scope is set.
	// TODO: validate that Key is not empty when Scope is set in webhook.
	// +optional
	Key string `json:"key,omitempty"`
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
	// Lower values are upgraded first. If not specified, roles upgrade after all explicitly ordered roles.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Type=integer
	UpgradeOrder *int32 `json:"upgradeOrder,omitempty"`

	// PodGroupSize is the number of pods to form a minimum role instance.
	// Must be >= 1 if specified. For multi-node inference, set > 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
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

	// +optional
	SchedulingStrategy *SchedulingStrategy `json:"schedulingStrategy,omitempty"`
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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`,description="Whether the RoleSet is ready (True/False)"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Time this RoleSet was created"

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
