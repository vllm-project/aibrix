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

package algorithm

import (
	"context"
	"fmt"
	"math"

	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/klog/v2"
)

// KPAAlgorithm implements Knative-style Pod Autoscaling with panic and stable windows
type KPAAlgorithm struct {
	config AlgorithmConfig
}

// NewKPAAlgorithm creates a new KPA algorithm instance
func NewKPAAlgorithm(config AlgorithmConfig) *KPAAlgorithm {
	return &KPAAlgorithm{
		config: config,
	}
}

var _ ScalingAlgorithm = (*KPAAlgorithm)(nil)

const (
	MinScaleRate     = 1.0
	DefaultTolerance = 0.0
)

// ComputeRecommendation calculates desired replica count for KPA strategy
func (a *KPAAlgorithm) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	metrics := request.AggregatedMetrics

	// KPA logic: Choose between stable and panic metrics
	var currentValue float64
	var mode string

	// Detect panic mode based on metrics
	if a.shouldEnterPanicMode(metrics) {
		currentValue = metrics.PanicValue
		mode = "panic"
		request.ScalingContext.SetInPanicMode(true)
	} else {
		currentValue = metrics.StableValue
		mode = "stable"
		request.ScalingContext.SetInPanicMode(false)
	}

	// Update context with current metrics
	request.ScalingContext.SetStableValue(metrics.StableValue)
	request.ScalingContext.SetPanicValue(metrics.PanicValue)

	// Compute target replicas using KPA algorithm
	desiredReplicas := a.computeTargetReplicas(float64(request.CurrentReplicas), request.ScalingContext)

	// Apply constraints
	desiredReplicas = applyConstraints(desiredReplicas, request.Constraints)

	return &ScalingRecommendation{
		DesiredReplicas: desiredReplicas,
		Confidence:      metrics.Confidence,
		Reason:          fmt.Sprintf("%s mode scaling", mode),
		Algorithm:       "kpa",
		ScaleValid:      true,
		Metadata: map[string]interface{}{
			"mode":          mode,
			"stable_value":  metrics.StableValue,
			"panic_value":   metrics.PanicValue,
			"current_value": currentValue,
		},
	}, nil
}

// shouldEnterPanicMode determines if we should switch to panic mode
func (a *KPAAlgorithm) shouldEnterPanicMode(metrics *types.AggregatedMetrics) bool {
	if metrics.StableValue <= 0 {
		return true
	}
	return metrics.PanicValue/metrics.StableValue > a.config.PanicThreshold
}

// GetAlgorithmType returns the algorithm type
func (a *KPAAlgorithm) GetAlgorithmType() string {
	return "kpa"
}

// UpdateConfiguration updates the algorithm configuration
func (a *KPAAlgorithm) UpdateConfiguration(config AlgorithmConfig) error {
	a.config = config
	return nil
}

// computeTargetReplicas is the core KPA scaling logic (moved from ComputeTargetReplicas)
func (a *KPAAlgorithm) computeTargetReplicas(currentPodCount float64, context scalingctx.ScalingContext) int32 {
	// Get all the necessary values from context
	observedStableValue := context.GetStableValue()
	observedPanicValue := context.GetPanicValue()
	targetValue := context.GetTargetValue()
	panicThreshold := context.GetPanicThreshold()
	inPanicMode := context.GetInPanicMode()
	maxPanicPods := context.GetMaxPanicPods()

	// Original scaling rate calculations
	readyPodsCount := math.Max(0, currentPodCount)                                   // A little sanitizing.
	maxScaleUp := math.Max(1, math.Ceil(context.GetMaxScaleUpRate()*readyPodsCount)) // Keep scale up non zero
	maxScaleDown := math.Floor(readyPodsCount / context.GetMaxScaleDownRate())       // Make scale down zero-able

	dspc := math.Ceil(observedStableValue / targetValue)
	dppc := math.Ceil(observedPanicValue / targetValue)

	// We want to keep desired pod count in the [maxScaleDown, maxScaleUp] range.
	desiredStablePodCount := int32(math.Min(math.Max(dspc, maxScaleDown), maxScaleUp))
	desiredPanicPodCount := int32(math.Min(math.Max(dppc, maxScaleDown), maxScaleUp))

	// If ActivationScale > 1, then adjust the desired pod counts
	activationScale := context.GetActivationScale()
	if activationScale > 1 {
		// ActivationScale only makes sense if activated (desired > 0)
		if activationScale > desiredStablePodCount && desiredStablePodCount > 0 {
			desiredStablePodCount = activationScale
		}
		if activationScale > desiredPanicPodCount && desiredPanicPodCount > 0 {
			desiredPanicPodCount = activationScale
		}
	}

	// Now readyPodsCount can be 0, use max(1, readyPodsCount) to prevent error.
	isOverPanicThreshold := dppc/math.Max(1, readyPodsCount) >= panicThreshold

	klog.V(4).InfoS("--- KPA Details", "readyPodsCount", readyPodsCount,
		"MaxScaleUpRate", context.GetMaxScaleUpRate(), "MaxScaleDownRate", context.GetMaxScaleDownRate(),
		"TargetValue", targetValue, "PanicThreshold", panicThreshold,
		"StableWindow", context.GetStableWindow(), "PanicWindow", context.GetPanicWindow(), "ScaleDownDelay", context.GetScaleDownDelay(),
		"dppc", dppc, "dspc", dspc, "desiredStablePodCount", desiredStablePodCount,
		"isOverPanicThreshold", isOverPanicThreshold,
	)

	desiredPodCount := desiredStablePodCount
	if inPanicMode {
		// In some edge cases stable window metric might be larger
		// than panic one. And we should provision for stable as for panic,
		// so pick the larger of the two.
		klog.InfoS("Operating in panic mode.", "desiredPodCount", desiredPodCount, "desiredPanicPodCount", desiredPanicPodCount)
		if desiredPodCount < desiredPanicPodCount {
			desiredPodCount = desiredPanicPodCount
		}
		// We do not scale down while in panic mode. Only increases will be applied.
		if desiredPodCount > maxPanicPods {
			klog.InfoS("Increasing pods count.", "originalPodCount", int(currentPodCount), "desiredPodCount", desiredPodCount)
			maxPanicPods = desiredPodCount
			context.SetMaxPanicPods(maxPanicPods)
		} else if desiredPodCount < maxPanicPods {
			klog.InfoS("Skipping pod count decrease", "current", maxPanicPods, "desired", desiredPodCount)
		}
		desiredPodCount = maxPanicPods
	} else {
		klog.V(4).InfoS("Operating in stable mode.", "desiredPodCount", desiredPodCount)
	}

	return desiredPodCount
}
