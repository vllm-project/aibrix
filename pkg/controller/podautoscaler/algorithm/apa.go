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
	"math"

	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"k8s.io/klog/v2"
)

// APAAlgorithm implements Application-specific Pod Autoscaling
type APAAlgorithm struct {
	config AlgorithmConfig
}

// NewAPAAlgorithm creates a new APA algorithm instance
func NewAPAAlgorithm(config AlgorithmConfig) *APAAlgorithm {
	return &APAAlgorithm{
		config: config,
	}
}

var _ ScalingAlgorithm = (*APAAlgorithm)(nil)

// ComputeRecommendation calculates desired replica count for APA strategy
func (a *APAAlgorithm) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	metrics := request.AggregatedMetrics

	// APA uses current value directly
	request.ScalingContext.SetCurrentUsePerPod(metrics.CurrentValue)

	// Compute target replicas using APA algorithm
	desiredReplicas := a.computeTargetReplicas(float64(request.CurrentReplicas), request.ScalingContext)

	// Apply constraints
	desiredReplicas = applyConstraints(desiredReplicas, request.Constraints)

	return &ScalingRecommendation{
		DesiredReplicas: desiredReplicas,
		Confidence:      metrics.Confidence,
		Reason:          "apa scaling based on current metrics",
		Algorithm:       "apa",
		ScaleValid:      true,
		Metadata: map[string]interface{}{
			"current_value": metrics.CurrentValue,
			"trend":         metrics.Trend,
		},
	}, nil
}

// GetAlgorithmType returns the algorithm type
func (a *APAAlgorithm) GetAlgorithmType() string {
	return "apa"
}

// UpdateConfiguration updates the algorithm configuration
func (a *APAAlgorithm) UpdateConfiguration(config AlgorithmConfig) error {
	a.config = config
	return nil
}

// computeTargetReplicas - APA's algorithm references and enhances the algorithm in the following paper:
// Huo, Qizheng, et al. "High Concurrency Response Strategy based on Kubernetes Horizontal Pod Autoscaler."
// Journal of Physics: Conference Series. Vol. 2451. No. 1. IOP Publishing, 2023.
func (a *APAAlgorithm) computeTargetReplicas(currentPodCount float64, context scalingctx.ScalingContext) int32 {
	expectedUse := context.GetTargetValue()
	upTolerance := context.GetUpFluctuationTolerance()
	downTolerance := context.GetDownFluctuationTolerance()
	currentUsePerPod := context.GetCurrentUsePerPod()

	klog.V(4).InfoS("--- APA Details", "currentPodCount", currentPodCount,
		"expectedUse", expectedUse, "upTolerance", upTolerance, "downTolerance", downTolerance,
		"currentUsePerPod", currentUsePerPod, "current/expected", currentUsePerPod/expectedUse,
	)

	if currentUsePerPod/expectedUse > (1 + upTolerance) {
		maxScaleUp := math.Ceil(context.GetMaxScaleUpRate() * currentPodCount)
		expectedPods := int32(math.Ceil(currentPodCount * (currentUsePerPod / expectedUse)))
		if float64(expectedPods) > maxScaleUp {
			expectedPods = int32(maxScaleUp)
		}
		return expectedPods
	} else if currentUsePerPod/expectedUse < (1 - downTolerance) {
		maxScaleDown := math.Floor(currentPodCount / context.GetMaxScaleDownRate())
		expectedPods := int32(math.Ceil(currentPodCount * (currentUsePerPod / expectedUse)))
		if float64(expectedPods) < maxScaleDown {
			expectedPods = int32(maxScaleDown)
		}
		return expectedPods
	}
	return int32(currentPodCount)
}
