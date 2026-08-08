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

package circuitbreaker

import (
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoundHealth summarizes the metric health for a reconcile round.
type RoundHealth string

const (
	RoundHealthAllHealthy RoundHealth = "AllHealthy"
	RoundHealthDegraded   RoundHealth = "Degraded"
	RoundHealthAllFailed  RoundHealth = "AllFailed"
)

// Transition identifies state transitions emitted by Evaluate.
type Transition string

const (
	TransitionNone   Transition = ""
	TransitionOpened Transition = "Opened"
	TransitionClosed Transition = "Closed"
)

const (
	ReasonMetricsHealthy = "MetricsHealthy"
	ReasonDegraded       = "MetricSourcesDegraded"
	ReasonAllFailed      = "AllMetricSourcesFailed"
	ReasonOpened         = "CircuitBreakerOpened"
	ReasonRecovering     = "CircuitBreakerRecovering"
	ReasonClosed         = "CircuitBreakerClosed"
)

// Status is the pure state-machine representation persisted by the controller.
type Status struct {
	State              autoscalingv1alpha1.CircuitBreakerState
	Action             autoscalingv1alpha1.CircuitBreakerAction
	FailureCount       int32
	RecoveryCount      int32
	ProtectedReplicas  *int32
	Reason             string
	LastTransitionTime *metav1.Time
}

// Input contains all data needed to evaluate one circuit breaker round.
type Input struct {
	Config          config.Config
	Previous        Status
	Round           RoundHealth
	CurrentReplicas int32
	DesiredReplicas int32
	MinReplicas     int32
	MaxReplicas     int32
	Now             time.Time
}

// Result contains the next state and optional replica override.
type Result struct {
	Status          Status
	Override        bool
	DesiredReplicas int32
	BlockScaleDown  bool
	Transition      Transition
	Reason          string
}

// Evaluate applies the Closed/Open state machine without mutating input.
func Evaluate(in Input) Result {
	if !in.Config.Enabled {
		return Result{DesiredReplicas: clamp(in.DesiredReplicas, in.MinReplicas, in.MaxReplicas)}
	}

	status := in.Previous
	if status.State == "" {
		status.State = autoscalingv1alpha1.CircuitBreakerStateClosed
	}
	if in.Config.Action == "" {
		in.Config.Action = autoscalingv1alpha1.CircuitBreakerActionFreeze
	}
	if status.State == autoscalingv1alpha1.CircuitBreakerStateOpen && status.Action == "" {
		status.Action = in.Config.Action
	}

	result := Result{
		Status:          status,
		DesiredReplicas: clamp(in.DesiredReplicas, in.MinReplicas, in.MaxReplicas),
	}

	switch status.State {
	case autoscalingv1alpha1.CircuitBreakerStateOpen:
		evaluateOpen(in, &result)
	default:
		evaluateClosed(in, &result)
	}

	if result.Status.State == autoscalingv1alpha1.CircuitBreakerStateOpen && result.Override {
		applyProtectiveAction(in, &result)
	}

	result.DesiredReplicas = clamp(result.DesiredReplicas, in.MinReplicas, in.MaxReplicas)
	return result
}

func evaluateClosed(in Input, result *Result) {
	result.Status.State = autoscalingv1alpha1.CircuitBreakerStateClosed
	result.Status.Action = ""
	result.Status.RecoveryCount = 0

	switch in.Round {
	case RoundHealthAllFailed:
		result.Status.FailureCount++
		result.Status.Reason = ReasonAllFailed
		result.Override = true
		result.DesiredReplicas = in.CurrentReplicas
		result.Reason = ReasonAllFailed
		if result.Status.FailureCount >= in.Config.FailureThreshold {
			result.Status.State = autoscalingv1alpha1.CircuitBreakerStateOpen
			result.Status.Action = in.Config.Action
			result.Status.LastTransitionTime = ptrMetaTime(in.Now)
			result.Status.Reason = ReasonOpened
			result.Transition = TransitionOpened
			result.Reason = ReasonOpened
		}
	case RoundHealthDegraded:
		result.Status.FailureCount = 0
		result.Status.Reason = ReasonDegraded
		result.BlockScaleDown = true
		result.Reason = ReasonDegraded
	default:
		result.Status.FailureCount = 0
		result.Status.Reason = ReasonMetricsHealthy
		result.Reason = ReasonMetricsHealthy
	}
}

func evaluateOpen(in Input, result *Result) {
	result.Status.State = autoscalingv1alpha1.CircuitBreakerStateOpen
	result.Status.Action = in.Config.Action
	result.Override = true
	result.DesiredReplicas = in.CurrentReplicas

	switch in.Round {
	case RoundHealthAllHealthy:
		result.Status.RecoveryCount++
		result.Status.Reason = ReasonRecovering
		result.Reason = ReasonRecovering
		if result.Status.RecoveryCount >= in.Config.RecoveryThreshold {
			result.Status.State = autoscalingv1alpha1.CircuitBreakerStateClosed
			result.Status.Action = ""
			result.Status.RecoveryCount = 0
			result.Status.LastTransitionTime = ptrMetaTime(in.Now)
			result.Status.Reason = ReasonClosed
			result.Override = false
			result.DesiredReplicas = in.DesiredReplicas
			result.Transition = TransitionClosed
			result.Reason = ReasonClosed
		}
	case RoundHealthDegraded:
		result.Status.RecoveryCount = 0
		result.Status.Reason = ReasonDegraded
		result.Reason = ReasonDegraded
	default:
		result.Status.RecoveryCount = 0
		result.Status.Reason = ReasonAllFailed
		result.Reason = ReasonAllFailed
	}
}

func applyProtectiveAction(in Input, result *Result) {
	switch in.Config.Action {
	case autoscalingv1alpha1.CircuitBreakerActionMax:
		result.Status.Action = autoscalingv1alpha1.CircuitBreakerActionMax
		result.Status.ProtectedReplicas = nil
		result.DesiredReplicas = in.MaxReplicas
	default:
		result.Status.Action = autoscalingv1alpha1.CircuitBreakerActionFreeze
		protected := in.CurrentReplicas
		if in.Previous.Action == autoscalingv1alpha1.CircuitBreakerActionFreeze && in.Previous.ProtectedReplicas != nil {
			protected = *in.Previous.ProtectedReplicas
		}
		protected = clamp(protected, in.MinReplicas, in.MaxReplicas)
		result.Status.ProtectedReplicas = ptrInt32(protected)
		result.DesiredReplicas = protected
	}
}

func clamp(value, minValue, maxValue int32) int32 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func ptrInt32(value int32) *int32 {
	return &value
}

func ptrMetaTime(t time.Time) *metav1.Time {
	out := metav1.NewTime(t)
	return &out
}
