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

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"
)

type KpaScalingAlgorithm struct{}

var _ ScalingAlgorithm = (*KpaScalingAlgorithm)(nil)

func (a *KpaScalingAlgorithm) ComputeTargetReplicas(currentPodCount float64, context common.ScalingContext) int32 {
	expectedUse := context.GetTargetValue()
	upTolerance := context.GetUpFluctuationTolerance()
	downTolerance := context.GetDownFluctuationTolerance()
	currentUsePerPod := context.GetCurrentUsePerPod()

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
