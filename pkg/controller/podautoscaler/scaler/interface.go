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
	"context"
	"time"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	corev1 "k8s.io/api/core/v1"
)

/**

This implementation is inspired by the scaling solutions provided by Knative.
Our implementation specifically mimics and adapts the autoscaling functionality found in:

- autoscaler:			pkg/autoscaler/scaling/autoscaler.go
- Scaler(interface):	pkg/autoscaler/scaling/autoscaler.go
- KpaScalingContext:		pkg/autoscaler/scaling/multiscaler.go
- ScaleResult:			pkg/autoscaler/scaling/multiscaler.go

*/

// Scaler defines the interface for autoscaling operations.
// Any autoscaler implementation, such as KpaAutoscaler (Kubernetes Pod Autoscaler),
// must implement this interface to respond to scaling events.
type Scaler interface {
	// UpdateScaleTargetMetrics updates the current state of metrics used to determine scaling actions.
	// It processes the latest metrics for a given scaling target (identified by metricKey) and stores
	// these values for later use during scaling decisions.
	//
	// Parameters:
	// - ctx: The context used for managing request-scoped values, cancellation, and deadlines.
	// - metricKey: A unique identifier for the scaling target's metrics (e.g., CPU, memory, or QPS) that
	//   is used to correlate metrics with the appropriate scaling logic.
	// - now: The current time at which the metrics are being processed. This timestamp helps track
	//   when the last metric update occurred and can be used to calculate time-based scaling actions.
	//
	// This method ensures that the autoscaler has up-to-date metrics before making any scaling decisions.
	UpdateScaleTargetMetrics(ctx context.Context, metricKey metrics.NamespaceNameMetric, pods []corev1.Pod, now time.Time) error

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

	// UpdateScalingContext updates the internal scaling context for a given PodAutoscaler (PA) instance.
	// It extracts necessary information from the provided PodAutoscaler resource, such as current
	// metrics, scaling parameters, and other relevant data to refresh the scaling context.
	//
	// Parameters:
	// - pa: The PodAutoscaler resource containing the desired scaling configuration and current state.
	//
	// Returns:
	// - error: If the context update fails due to invalid input or configuration issues, it returns an error.
	//
	// This method ensures that the internal scaling context is always in sync with the latest state
	// and configuration of the target PodAutoscaler, allowing accurate scaling decisions.
	UpdateScalingContext(pa autoscalingv1alpha1.PodAutoscaler) error

	// GetScalingContext retrieves the current scaling context used for making scaling decisions.
	// This method returns a pointer to the ScalingContext, which contains essential data like
	// target values, current metrics, and scaling tolerances.
	//
	// Returns:
	// - *common.ScalingContext: A pointer to the ScalingContext instance containing the relevant
	//   data for autoscaling logic.
	//
	// This method provides access to the scaling context for external components or logic that
	// need to read or adjust the current scaling parameters.
	GetScalingContext() common.ScalingContext
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
