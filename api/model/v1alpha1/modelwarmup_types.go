/*
Copyright 2026 The Aibrix Team.

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

const (
	DefaultModelWarmupParallelism             int32 = 8
	DefaultModelWarmupGlobalTimeoutSeconds    int64 = 1800
	DefaultModelWarmupRetryLimit              int32 = 2
	DefaultModelWarmupTTLSecondsAfterFinished int32 = 3600
	MaxModelWarmupTargets                           = 1000
)

type ModelWarmupSpec struct {
	// Targets is the union of explicit nodes and selector-discovered nodes.
	// +kubebuilder:validation:MinItems=1
	Targets []ModelWarmupTarget `json:"targets"`

	ImagePreload ModelWarmupImagePreload `json:"imagePreload"`

	// +optional
	Policies *ModelWarmupPolicies `json:"policies,omitempty"`
}

type ModelWarmupTarget struct {
	// Nodes lists explicitly selected node names.
	// +optional
	Nodes *ModelWarmupNodesTarget `json:"nodes,omitempty"`

	// NodeSelector selects nodes by labels. Empty selectors are rejected by admission.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

type ModelWarmupNodesTarget struct {
	// +kubebuilder:validation:MinItems=1
	Names []string `json:"names"`
}

type ModelWarmupImagePreload struct {
	// +kubebuilder:validation:MinItems=1
	Images []ModelWarmupImage `json:"images"`

	// +optional
	PullSecrets []corev1.LocalObjectReference `json:"pullSecrets,omitempty"`
}

type ModelWarmupImage struct {
	Image string `json:"image"`

	// Command must exit safely after the image is pulled.
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`

	// +optional
	Args []string `json:"args,omitempty"`

	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

type ModelWarmupPolicies struct {
	// +optional
	Parallelism *int32 `json:"parallelism,omitempty"`
	// +optional
	GlobalTimeoutSeconds *int64 `json:"globalTimeoutSeconds,omitempty"`
	// +optional
	RetryLimit *int32 `json:"retryLimit,omitempty"`
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

type ModelWarmupPhase string

const (
	ModelWarmupPending   ModelWarmupPhase = "Pending"
	ModelWarmupRunning   ModelWarmupPhase = "Running"
	ModelWarmupSucceeded ModelWarmupPhase = "Succeeded"
	ModelWarmupFailed    ModelWarmupPhase = "Failed"
	ModelWarmupDegraded  ModelWarmupPhase = "Degraded"
)

type ModelWarmupTargetPhase string

const (
	ModelWarmupTargetPending   ModelWarmupTargetPhase = "Pending"
	ModelWarmupTargetRunning   ModelWarmupTargetPhase = "Running"
	ModelWarmupTargetSucceeded ModelWarmupTargetPhase = "Succeeded"
	ModelWarmupTargetFailed    ModelWarmupTargetPhase = "Failed"
)

type ModelWarmupTargetStatus struct {
	NodeName string `json:"nodeName"`
	// +optional
	Sources []string `json:"sources,omitempty"`
	// +optional
	Revision string `json:"revision,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	Phase ModelWarmupTargetPhase `json:"phase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

type ModelWarmupStatus struct {
	// +optional
	Phase ModelWarmupPhase `json:"phase,omitempty"`
	// +optional
	ObservedRevision string `json:"observedRevision,omitempty"`
	// +optional
	DesiredNodes int32 `json:"desiredNodes,omitempty"`
	// +optional
	ActiveNodes int32 `json:"activeNodes,omitempty"`
	// +optional
	SucceededNodes int32 `json:"succeededNodes,omitempty"`
	// +optional
	FailedNodes int32 `json:"failedNodes,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	Targets []ModelWarmupTargetStatus `json:"targets,omitempty"`
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mw
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredNodes`
// +kubebuilder:printcolumn:name="Succeeded",type=integer,JSONPath=`.status.succeededNodes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelWarmup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelWarmupSpec   `json:"spec,omitempty"`
	Status ModelWarmupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelWarmupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelWarmup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelWarmup{}, &ModelWarmupList{})
}
