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
	"sync"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

/**

This implementation is inspired by the scaling solutions provided by Knative.
Our implementation specifically mimics and adapts the autoscaling functionality found in:

- autoscaler:			pkg/autoscaler/scaling/autoscaler.go
- Scaler(interface):	pkg/autoscaler/scaling/autoscaler.go
- DeciderKpaSpec:		pkg/autoscaler/scaling/multiscaler.go
- ScaleResult:			pkg/autoscaler/scaling/multiscaler.go

*/

// Autoscaler represents an instance of the autoscaling engine.
// It encapsulates all the necessary data and state needed for scaling decisions.
// Refer to:  KpaAutoscaler
type Autoscaler struct {
	// specMux guards the current DeciderKpaSpec.
	specMux        sync.RWMutex
	metricsClient  metrics.MetricsClient
	resourceClient client.Client
	scaler         Scaler
}

// Scaler is an interface that defines the scaling operations.
// Any autoscaler implementation, such as KpaAutoscaler (Kubernetes Pod Autoscaler),
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
	// Refer to:  KpaAutoscaler.Scale Implementation
	Scale(originalReadyPodsCount int, observedStableValue float64, observedPanicValue float64, now time.Time) ScaleResult
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
