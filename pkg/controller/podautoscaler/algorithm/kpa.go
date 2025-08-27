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
	"math"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"
	"k8s.io/klog/v2"
)

type KpaScalingAlgorithm struct{}

var _ ScalingAlgorithm = (*KpaScalingAlgorithm)(nil)

const (
	MinScaleRate     = 1.0
	DefaultTolerance = 0.0
)

func (a *KpaScalingAlgorithm) ComputeTargetReplicas(currentPodCount float64, context common.ScalingContext) int32 {
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
