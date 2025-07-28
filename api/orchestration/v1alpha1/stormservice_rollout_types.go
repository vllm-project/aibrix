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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// RolloutStrategy defines the strategy for performing rollouts
type RolloutStrategy struct {
	// Strategy type for the rollout. Can be "Canary" or "BlueGreen"
	// +optional
	Type RolloutStrategyType `json:"type,omitempty"`

	// Canary configuration for canary rollouts
	// +optional
	Canary *CanaryStrategy `json:"canary,omitempty"`

	// BlueGreen configuration for blue-green rollouts
	// +optional
	BlueGreen *BlueGreenStrategy `json:"blueGreen,omitempty"`

	// AutoRollback configuration for automatic rollback
	// +optional
	AutoRollback *AutoRollbackConfig `json:"autoRollback,omitempty"`
}

// +enum
type RolloutStrategyType string

const (
	// CanaryRolloutStrategyType uses canary deployment with traffic splitting
	CanaryRolloutStrategyType RolloutStrategyType = "Canary"
	// BlueGreenRolloutStrategyType uses blue-green deployment with full environment switching
	BlueGreenRolloutStrategyType RolloutStrategyType = "BlueGreen"
)

// CanaryStrategy defines the configuration for canary rollouts
type CanaryStrategy struct {
	// Steps defines the steps of the canary rollout
	// +optional
	Steps []CanaryStep `json:"steps,omitempty"`

	// TrafficRouting defines how traffic should be routed during the canary
	// +optional
	TrafficRouting *TrafficRoutingConfig `json:"trafficRouting,omitempty"`

	// MaxSurge defines the maximum number of replicas that can be created above the desired number
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// MaxUnavailable defines the maximum number of replicas that can be unavailable during the rollout
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// CanaryService defines the name of the service to route canary traffic to
	// +optional
	CanaryService string `json:"canaryService,omitempty"`

	// StableService defines the name of the service to route stable traffic to
	// +optional
	StableService string `json:"stableService,omitempty"`
}

// CanaryStep defines a step in the canary rollout
type CanaryStep struct {
	// Weight defines the percentage of traffic to route to the canary
	// +optional
	Weight *int32 `json:"weight,omitempty"`

	// Replicas defines the number of canary replicas
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Pause defines if the rollout should pause at this step
	// +optional
	Pause *PauseConfig `json:"pause,omitempty"`

	// Analysis defines analysis to run at this step
	// +optional
	Analysis *AnalysisStep `json:"analysis,omitempty"`
}

// PauseConfig defines how long to pause and conditions for resuming
type PauseConfig struct {
	// Duration defines how long to pause. If not specified, pauses indefinitely
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// UntilApproval indicates that the rollout should pause until manual approval
	// +optional
	UntilApproval bool `json:"untilApproval,omitempty"`
}

// AnalysisStep defines analysis to run during the rollout
type AnalysisStep struct {
	// Templates defines the analysis templates to run
	// +optional
	Templates []AnalysisTemplateRef `json:"templates,omitempty"`

	// Args defines arguments to pass to the analysis templates
	// +optional
	Args []AnalysisArg `json:"args,omitempty"`
}

// AnalysisTemplateRef references an analysis template
type AnalysisTemplateRef struct {
	// Name of the analysis template
	Name string `json:"name"`

	// Namespace of the analysis template. If not specified, uses the same namespace as the StormService
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AnalysisArg defines an argument to pass to an analysis template
type AnalysisArg struct {
	// Name of the argument
	Name string `json:"name"`

	// Value of the argument
	Value string `json:"value"`
}

// TrafficRoutingConfig defines how to route traffic during rollouts
type TrafficRoutingConfig struct {
	// Nginx configuration for nginx ingress controller
	// +optional
	Nginx *NginxTrafficRouting `json:"nginx,omitempty"`

	// Istio configuration for Istio service mesh
	// +optional
	Istio *IstioTrafficRouting `json:"istio,omitempty"`

	// SMI configuration for Service Mesh Interface
	// +optional
	SMI *SMITrafficRouting `json:"smi,omitempty"`
}

// NginxTrafficRouting defines nginx-specific traffic routing configuration
type NginxTrafficRouting struct {
	// Ingress refers to the ingress name to use for traffic routing
	Ingress string `json:"ingress"`

	// ServicePort refers to the service port to route traffic to
	ServicePort int32 `json:"servicePort"`
}

// IstioTrafficRouting defines Istio-specific traffic routing configuration
type IstioTrafficRouting struct {
	// VirtualService refers to the virtual service name
	VirtualService string `json:"virtualService"`

	// DestinationRule refers to the destination rule name
	// +optional
	DestinationRule string `json:"destinationRule,omitempty"`
}

// SMITrafficRouting defines SMI-specific traffic routing configuration
type SMITrafficRouting struct {
	// TrafficSplit refers to the traffic split resource name
	TrafficSplit string `json:"trafficSplit"`
}

// BlueGreenStrategy defines the configuration for blue-green rollouts
type BlueGreenStrategy struct {
	// ActiveService defines the name of the service for the active environment
	ActiveService string `json:"activeService"`

	// PreviewService defines the name of the service for the preview environment
	PreviewService string `json:"previewService"`

	// AutoPromotionEnabled indicates if the rollout should automatically promote
	// +optional
	AutoPromotionEnabled bool `json:"autoPromotionEnabled,omitempty"`

	// ScaleDownDelaySeconds defines how long to wait before scaling down old version
	// +optional
	ScaleDownDelaySeconds *int32 `json:"scaleDownDelaySeconds,omitempty"`

	// PrePromotionAnalysis defines analysis to run before promotion
	// +optional
	PrePromotionAnalysis *AnalysisStep `json:"prePromotionAnalysis,omitempty"`

	// PostPromotionAnalysis defines analysis to run after promotion
	// +optional
	PostPromotionAnalysis *AnalysisStep `json:"postPromotionAnalysis,omitempty"`
}

// AutoRollbackConfig defines automatic rollback configuration
type AutoRollbackConfig struct {
	// Enabled indicates if auto rollback is enabled
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// OnFailure defines conditions that trigger automatic rollback
	// +optional
	OnFailure *RollbackTrigger `json:"onFailure,omitempty"`

	// OnTimeout defines timeout that triggers automatic rollback
	// +optional
	OnTimeout *metav1.Duration `json:"onTimeout,omitempty"`
}

// RollbackTrigger defines conditions that trigger rollback
type RollbackTrigger struct {
	// AnalysisFailure indicates if analysis failure should trigger rollback
	// +optional
	AnalysisFailure bool `json:"analysisFailure,omitempty"`

	// HealthCheckFailure indicates if health check failure should trigger rollback
	// +optional
	HealthCheckFailure bool `json:"healthCheckFailure,omitempty"`

	// ErrorThreshold defines the error rate threshold that triggers rollback
	// +optional
	ErrorThreshold *intstr.IntOrString `json:"errorThreshold,omitempty"`
}

// RolloutConditionType defines the type of rollout condition
type RolloutConditionType string

const (
	// RolloutProgressing indicates that the rollout is progressing
	RolloutProgressing RolloutConditionType = "Progressing"
	// RolloutCompleted indicates that the rollout has completed successfully
	RolloutCompleted RolloutConditionType = "Completed"
	// RolloutPaused indicates that the rollout is paused
	RolloutPaused RolloutConditionType = "Paused"
	// RolloutDegraded indicates that the rollout has degraded
	RolloutDegraded RolloutConditionType = "Degraded"
	// RolloutHealthy indicates that the rollout is healthy
	RolloutHealthy RolloutConditionType = "Healthy"
)

// RolloutCondition describes the condition of a rollout
type RolloutCondition struct {
	// Type of rollout condition
	Type RolloutConditionType `json:"type"`

	// Status of the condition, one of True, False, Unknown
	Status corev1.ConditionStatus `json:"status"`

	// LastUpdateTime is the last time this condition was updated
	LastUpdateTime metav1.Time `json:"lastUpdateTime"`

	// LastTransitionTime is the last time the condition transitioned from one status to another
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`

	// Reason is a unique, one-word, CamelCase reason for the condition's last transition
	Reason string `json:"reason"`

	// Message is a human-readable message indicating details about the transition
	Message string `json:"message"`
}

// RolloutStatus defines the status of the rollout
type RolloutStatus struct {
	// Phase indicates the current phase of the rollout
	// +optional
	Phase RolloutPhase `json:"phase,omitempty"`

	// CurrentStepIndex indicates the current step in the rollout
	// +optional
	CurrentStepIndex *int32 `json:"currentStepIndex,omitempty"`

	// CurrentStepHash is a hash of the current step for detecting changes
	// +optional
	CurrentStepHash string `json:"currentStepHash,omitempty"`

	// PauseStartTime indicates when the rollout was paused
	// +optional
	PauseStartTime *metav1.Time `json:"pauseStartTime,omitempty"`

	// Conditions describe the current conditions of the rollout
	// +optional
	Conditions []RolloutCondition `json:"conditions,omitempty"`

	// CanaryStatus provides status about the canary rollout
	// +optional
	CanaryStatus *CanaryStatus `json:"canaryStatus,omitempty"`

	// BlueGreenStatus provides status about the blue-green rollout
	// +optional
	BlueGreenStatus *BlueGreenStatus `json:"blueGreenStatus,omitempty"`

	// Message provides additional information about the rollout status
	// +optional
	Message string `json:"message,omitempty"`
}

// +enum
type RolloutPhase string

const (
	// RolloutPhaseHealthy indicates that the rollout is healthy
	RolloutPhaseHealthy RolloutPhase = "Healthy"
	// RolloutPhaseProgressing indicates that the rollout is progressing
	RolloutPhaseProgressing RolloutPhase = "Progressing"
	// RolloutPhasePaused indicates that the rollout is paused
	RolloutPhasePaused RolloutPhase = "Paused"
	// RolloutPhaseCompleted indicates that the rollout has completed
	RolloutPhaseCompleted RolloutPhase = "Completed"
	// RolloutPhaseDegraded indicates that the rollout is degraded
	RolloutPhaseDegraded RolloutPhase = "Degraded"
	// RolloutPhaseAborted indicates that the rollout was aborted
	RolloutPhaseAborted RolloutPhase = "Aborted"
)

// CanaryStatus provides information about the canary rollout
type CanaryStatus struct {
	// CurrentWeight indicates the current traffic weight to the canary
	// +optional
	CurrentWeight int32 `json:"currentWeight,omitempty"`

	// StableRS indicates the stable replica set
	// +optional
	StableRS string `json:"stableRS,omitempty"`

	// CanaryRS indicates the canary replica set
	// +optional
	CanaryRS string `json:"canaryRS,omitempty"`
}

// BlueGreenStatus provides information about the blue-green rollout
type BlueGreenStatus struct {
	// ActiveSelector indicates the selector for the active environment
	// +optional
	ActiveSelector string `json:"activeSelector,omitempty"`

	// PreviewSelector indicates the selector for the preview environment
	// +optional
	PreviewSelector string `json:"previewSelector,omitempty"`

	// ScaleDownDelayStartTime indicates when the scale down delay started
	// +optional
	ScaleDownDelayStartTime *metav1.Time `json:"scaleDownDelayStartTime,omitempty"`

	// PreviewExposeTime indicates when the preview was first exposed
	// +optional
	PreviewExposeTime *metav1.Time `json:"previewExposeTime,omitempty"`
}
