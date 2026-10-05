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
	"math/rand"

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
		sumOfSquares += (number - avg) * (number - avg)
	}
	variance := sumOfSquares / float64(len(numbers)-1)
	return math.Sqrt(variance)
}

// SelectRandomPodAsFallback selects a pod randomly as a fallback.
// This method should only be used when all other selection mechanisms have failed.
func SelectRandomPodAsFallback(ctx *types.RoutingContext, pods []*v1.Pod, randomFunc func(int) int) (*v1.Pod, error) {
	klog.Warningf("No suitable pods found; selecting a pod randomly as fallback, requestID: %s", ctx.RequestID)
	targetPod, err := utils.SelectRandomPod(pods, randomFunc)
	if err != nil {
		klog.ErrorS(err, "Random fallback selection failed", "requestID", ctx.RequestID)
		return nil, fmt.Errorf("random fallback selection failed: %w", err)
	}
	return targetPod, nil
}

// SelectByScore returns the pod with the best score according to polarity (lowest for
// PolarityLeast, highest for PolarityMost), breaking ties uniformly at random. Pods with
// scored[i] == false are ignored. Returns nil if no pod could be scored.
//
// best is seeded with +Inf/-Inf rather than the first scored value so a NaN score (e.g. a
// metric that hasn't warmed up) can never become "best": every comparison against NaN is
// false, so a pod that started as best could never be dethroned or matched by a real value.
func SelectByScore(pods []*v1.Pod, scores []float64, scored []bool, polarity types.Polarity) *v1.Pod {
	var candidates []*v1.Pod
	best := math.Inf(1)
	if polarity == types.PolarityMost {
		best = math.Inf(-1)
	}
	for i, pod := range pods {
		if !scored[i] {
			continue
		}
		s := scores[i]
		switch {
		case (polarity == types.PolarityLeast && s < best) || (polarity == types.PolarityMost && s > best):
			best = s
			candidates = []*v1.Pod{pod}
		case s == best:
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[rand.Intn(len(candidates))]
}

// RouteByScore is the shared "least-X"/"most-X" routing pattern used by single-metric
// strategies (least-request, least-kv-cache, least-busy-time, least-gpu-cache, ...): it scores
// all ready pods via scorer.ScoreAll, picks the best one per scorer.Polarity() breaking ties
// randomly, and falls back to a uniformly random ready pod when nothing could be scored.
func RouteByScore(ctx *types.RoutingContext, readyPodList types.PodList, scorer types.PodScorer) (*v1.Pod, error) {
	pods := readyPodList.All()
	scores, scored, err := scorer.ScoreAll(ctx, readyPodList)
	if err != nil {
		return nil, err
	}

	targetPod := SelectByScore(pods, scores, scored, scorer.Polarity())
	if targetPod == nil {
		return SelectRandomPodAsFallback(ctx, pods, rand.Intn)
	}

	klog.V(4).InfoS("route_by_score", "request_id", ctx.RequestID, "target_pod", targetPod.Name)
	return targetPod, nil
}
