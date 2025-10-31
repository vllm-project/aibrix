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

package routingalgorithms

import (
	"fmt"
	"math"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// mean calculates the mean of a slice of float64 numbers.
func mean(numbers []float64) float64 {
	if len(numbers) == 0 {
		return 0
	}
	sum := 0.0
	for _, number := range numbers {
		sum += number
	}
	return sum / float64(len(numbers))
}

// standardDeviation calculates the standard deviation of a slice of float64 numbers.
func standardDeviation(numbers []float64) float64 {
	if len(numbers) <= 1 {
		return 0
	}
	avg := mean(numbers)
	sumOfSquares := 0.0
	for _, number := range numbers {
		sumOfSquares += math.Pow(number-avg, 2)
	}
	variance := sumOfSquares / float64(len(numbers)-1)
	return math.Sqrt(variance)
}

// SelectRandomPodAsFallback selects a pod randomly as a fallback.
// This method should only be used when all other selection mechanisms have failed.
// For example, if no pods meet the required criteria (e.g., valid metrics or specific conditions),
// this method can be called to randomly select a pod from the provided list.
func SelectRandomPodAsFallback(ctx *types.RoutingContext, pods []*v1.Pod, randomFunc func(int) int) (*v1.Pod, error) {
	klog.Warningf("No suitable pods found; selecting a pod randomly as fallback, requestID: %s", ctx.RequestID)
	targetPod, err := utils.SelectRandomPod(pods, randomFunc)
	if err != nil {
		klog.ErrorS(err, "Random fallback selection failed", "requestID", ctx.RequestID)
		return nil, fmt.Errorf("random fallback selection failed: %w", err)
	}
	return targetPod, nil
}
