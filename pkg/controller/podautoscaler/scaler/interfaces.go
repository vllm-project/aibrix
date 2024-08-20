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

package kpa

import (
	"sync"
	"time"
)

/**

This implementation is inspired by the scaling solutions provided by Knative.
Our implementation specifically mimics and adapts the autoscaling functionality found in:

- autoscaler:		pkg/autoscaler/scaling/autoscaler.go
- Scaler(interface):	pkg/autoscaler/scaling/autoscaler.go
- DeciderSpec:		pkg/autoscaler/scaling/multiscaler.go
- ScaleResult:		pkg/autoscaler/scaling/multiscaler.go

*/

// Autoscaler represents an instance of the autoscaling engine.
// It encapsulates all the necessary data and state needed for scaling decisions.
// Refer to:  KpaScaler
type Autoscaler struct {
	// specMux guards the current DeciderSpec.
	specMux     sync.RWMutex
	podCounter  int
	deciderSpec *DeciderSpec
	Status      DeciderStatus
}

// Scaler is an interface that defines the scaling operations.
// Any autoscaler implementation, such as KpaScaler (Kubernetes Pod Autoscaler),
// needs to implement this interface to respond to scale events.
type Scaler interface {
	// Scale calculates the necessary scaling action based on the observed metrics
	// and the current time. This method is the core of the autoscaling logic.
	//
	// Parameters:
	// observedStableValue - the metric value (e.g., CPU utilization) averaged over a stable period.
	// observedPanicValue - the metric value observed during a short, recent period which may indicate a spike or drop.
	// now - the current time, used to determine if scaling actions are needed based on time-based rules or delays.
	//
	// Returns:
	// ScaleResult which contains the recommended number of pods to scale up or down to.
	//
	// Refer to:  KpaScaler.Scale Implementation
	Scale(observedStableValue float64, observedPanicValue float64, now time.Time) ScaleResult
}

// DeciderSpec defines parameters for scaling decisions.
type DeciderSpec struct {
	// Maximum rate at which to scale up
	MaxScaleUpRate float64
	// Maximum rate at which to scale down
	MaxScaleDownRate float64
	// The metric used for scaling, i.e. CPU, Memory, QPS.
	ScalingMetric string
	// The value of scaling metric per pod that we target to maintain.
	TargetValue float64
	// The total value of scaling metric that a pod can maintain.
	TotalValue float64
	// The burst capacity that user wants to maintain without queuing at the POD level.
	// Note, that queueing still might happen due to the non-ideal load balancing.
	TargetBurstCapacity float64
	// ActivationScale is the minimum, non-zero value that a service should scale to.
	// For example, if ActivationScale = 2, when a service scaled from zero it would
	// scale up two replicas in this case. In essence, this allows one to set both a
	// min-scale value while also preserving the ability to scale to zero.
	// ActivationScale must be >= 2.
	ActivationScale int32

	// TODO: Note that the following attributes are specific to Knative; but we retain them here temporarily.
	// PanicThreshold is the threshold at which panic mode is entered. It represents
	// a factor of the currently observed load over the panic window over the ready
	// pods. I.e. if this is 2, panic mode will be entered if the observed metric
	// is twice as high as the current population can handle.
	PanicThreshold float64
	// StableWindow is needed to determine when to exit panic mode.
	StableWindow time.Duration
	// ScaleDownDelay is the time that must pass at reduced concurrency before a
	// scale-down decision is applied.
	ScaleDownDelay time.Duration
}

// DeciderStatus is the current scale recommendation.
type DeciderStatus struct {
	// DesiredScale is the target number of instances that autoscaler
	// this revision needs.
	DesiredScale int32

	// TODO: ExcessBurstCapacity might be a general attribute since it describes
	//  how much capacity users want to keep for preparing for burst traffic.

	// ExcessBurstCapacity is the difference between spare capacity
	// (how much more load the pods in the revision deployment can take before being
	// overloaded) and the configured target burst capacity.
	// If this number is negative: Activator will be threaded in
	// the request path by the PodAutoscaler controller.
	ExcessBurstCapacity int32
}

// ScaleResult contains the results of a scaling decision.
type ScaleResult struct {
	// DesiredPodCount is the number of pods Autoscaler suggests for the revision.
	DesiredPodCount int32
	// ExcessBurstCapacity is computed headroom of the revision taking into
	// the account target burst capacity.
	ExcessBurstCapacity int32
	// ScaleValid specifies whether this scale result is valid, i.e. whether
	// Autoscaler had all the necessary information to compute a suggestion.
	ScaleValid bool
}
