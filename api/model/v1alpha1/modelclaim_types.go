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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ModelClaim declares a full-model runtime attachment managed by AIBrix. The
// backing pool may be a manually-created warm Deployment today and a richer
// resource pool later. The controller selects a pod from the pool and asks the
// aibrix-runtime sidecar to activate the model as an engine process using
// kvcached. Multiple ModelClaims may share one GPU.

// ModelClaimSpec defines the desired state of ModelClaim.
type ModelClaimSpec struct {
	// ModelName is the served model identifier that clients address in requests
	// (e.g. the `model` field of an OpenAI-style request). Defaults to the
	// ModelClaim object name when omitted.
	// +optional
	ModelName *string `json:"modelName,omitempty"`

	// PodSelector is a label query selecting the warm GPU pods this model may be
	// attached to. It typically matches `pool.aibrix.ai/name=<pool>`. Candidate
	// pods must also advertise the enabled warm-pool label.
	// +kubebuilder:validation:Required
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// ArtifactURL is the address of the model weights to download. Multiple
	// protocols are supported, e.g. s3://, gcs://, huggingface://.
	// +kubebuilder:validation:Required
	ArtifactURL string `json:"artifactURL,omitempty"`

	// Engine is the inference engine used to serve this model.
	// +optional
	// +kubebuilder:default=vllm
	// +kubebuilder:validation:Enum=vllm;sglang
	Engine string `json:"engine,omitempty"`

	// Replicas is the desired number of active engine instances for this model
	// inside the selected resource pool. For the initial high-density path the
	// supported and default value is 1; the field is retained as the model-level
	// serving slot count rather than a Kubernetes Deployment replica count.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// EngineConfig carries engine-specific startup options. Args maps engine
	// CLI flags to their values, e.g. {"--max-model-len": "2048"}.
	// +optional
	EngineConfig *ModelClaimEngineConfig `json:"engineConfig,omitempty"`
}

// ModelClaimEngineConfig describes engine-specific startup options.
type ModelClaimEngineConfig struct {
	// Args maps engine CLI flags to string values. Boolean flags should use
	// an empty string value.
	// +optional
	Args map[string]string `json:"args,omitempty"`
}

// ModelClaimPhase is a high-level summary of where a ModelClaim is in its
// lifecycle. It maps to the latest status condition.
type ModelClaimPhase string

const (
	// ModelClaimPending means the CR has been created; initial status.
	ModelClaimPending ModelClaimPhase = "Pending"
	// ModelClaimScheduling means the controller is selecting eligible warm pod(s)
	// to attach the model to.
	ModelClaimScheduling ModelClaimPhase = "Scheduling"
	// ModelClaimLoading means the pod agent is fetching weights and starting the
	// engine process for the model.
	ModelClaimLoading ModelClaimPhase = "Loading"
	// ModelClaimActivating means an engine has been spawned but is not yet
	// serveable (booting/compiling). Its warm-pod routing annotation is held at
	// the non-routable marker (port 0) until the runtime reports the engine ready, so
	// the gateway does not route to a not-ready engine.
	ModelClaimActivating ModelClaimPhase = "Activating"
	// ModelClaimActive means the model is serving on at least one warm pod.
	ModelClaimActive ModelClaimPhase = "Active"
	// ModelClaimFailed means activation terminated in a failure.
	ModelClaimFailed ModelClaimPhase = "Failed"
	// ModelClaimUnknown means the controller could not determine the state.
	ModelClaimUnknown ModelClaimPhase = "Unknown"
)

// ModelClaimInstance describes one engine instance of a ModelClaim running on
// a warm GPU pod.
type ModelClaimInstance struct {
	// Pod is the name of the warm GPU pod hosting this instance.
	Pod string `json:"pod"`

	// Port is the port on the pod that serves this model's engine process.
	// +optional
	Port int32 `json:"port,omitempty"`

	// Phase is the per-instance lifecycle phase.
	// +optional
	Phase ModelClaimPhase `json:"phase,omitempty"`
}

// ModelClaimStatus defines the observed state of ModelClaim.
type ModelClaimStatus struct {
	// Phase is a high-level summary of the model's lifecycle.
	// +optional
	Phase ModelClaimPhase `json:"phase,omitempty"`

	// Candidates is the number of warm pods matching the selector.
	// +optional
	Candidates int32 `json:"candidates,omitempty"`

	// ReadyReplicas is the number of warm pods on which the model is active and
	// ready to serve.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// DesiredReplicas is the desired number of active instances derived from
	// spec.replicas.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// Instances lists the per-pod engine instances of this model.
	// +optional
	Instances []ModelClaimInstance `json:"instances,omitempty"`

	// Conditions represents the latest observations of the model's state.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModelClaimConditionType enumerates the condition types reported in status.
type ModelClaimConditionType string

const (
	ModelClaimConditionTypeInitialized ModelClaimConditionType = "Initialized"
	ModelClaimConditionTypeScheduled   ModelClaimConditionType = "Scheduled"
	ModelClaimConditionTypeLoaded      ModelClaimConditionType = "Loaded"
	ModelClaimConditionReady           ModelClaimConditionType = "Ready"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Artifact",type=string,JSONPath=`.spec.artifactURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ModelClaim is the Schema for the modelclaims API.
type ModelClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelClaimSpec   `json:"spec,omitempty"`
	Status ModelClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ModelClaimList contains a list of ModelClaim.
type ModelClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelClaim{}, &ModelClaimList{})
}
