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

package scaler

import (
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
)

/**

This implementation is inspired by the scaling solutions provided by Knative.
Our implementation specifically mimics and adapts the autoscaling functionality found in:

- autoscaler:			pkg/autoscaler/scaling/autoscaler.go
- Scaler(interface):	pkg/autoscaler/scaling/autoscaler.go
- DeciderKpaSpec:		pkg/autoscaler/scaling/multiscaler.go
- ScaleResult:			pkg/autoscaler/scaling/multiscaler.go

*/

// Scaler defines the interface for autoscaling operations.
// Any autoscaler implementation, such as KpaAutoscaler (Kubernetes Pod Autoscaler),
// must implement this interface to respond to scaling events.
type Scaler interface {
	// Scale calculates the necessary scaling action based on observed metrics
	// and the current time. This is the core logic of the autoscaler.
	//
	// Parameters:
	// originalReadyPodsCount - the current number of ready pods.
	// metricKey - a unique key to identify the metric for scaling.
	// now - the current time, used to decide if scaling actions are needed based on timing rules or delays.
	//
	// Returns:
	// ScaleResult - contains the recommended number of pods to scale up or down.
	//
	// For reference: see the implementation in KpaAutoscaler.Scale.
	Scale(originalReadyPodsCount int, metricKey metrics.NamespaceNameMetric, now time.Time) ScaleResult
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
