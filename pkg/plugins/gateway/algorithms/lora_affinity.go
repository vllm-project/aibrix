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
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
)

const (
	// Default threshold for LoRA affinity selection probability
	// When both affinity and available pods exist, this probability determines
	// how often we select from affinity pods vs available pods for load balancing
	defaultLoraAffinityThreshold = 0.999
)

var (
	LoraAffinity types.RoutingAlgorithm = "lora-affinity"
	// Load LoRA affinity threshold from environment or use default
	loraAffinityThreshold = utils.LoadEnvFloat("AIBRIX_LORA_AFFINITY_THRESHOLD", defaultLoraAffinityThreshold)
)

func init() {
	Register(LoraAffinity, NewLoraAffinityRouter)
}

type loraAffinityRouter struct {
	cache cache.Cache
}

func NewLoraAffinityRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return loraAffinityRouter{
		cache: c,
	}, nil
}

// Route implements a pod selection strategy that prioritizes pods with existing LoRA model affinity
// while allowing for load balancing through randomization.
//
// The function works by:
// 1. Separating pods into two groups: those with target model affinity and those with available capacity
// 2. Using a probability threshold to sometimes select from non-affinity pods to enable load balancing
// 3. Falling back to whatever group has pods if one group is empty
func (r loraAffinityRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no pods available for routing")
	}

	// Pre-allocate slices with estimated capacity
	filteredAffinity := make([]*v1.Pod, 0, len(readyPods))
	filteredAvailable := make([]*v1.Pod, 0, len(readyPods))

	// Categorize pods based on affinity and availability
	for _, pod := range readyPods {
		hasAffinity, hasCapacity := r.evaluatePodForLoraAffinity(pod, ctx.Model)

		if hasAffinity {
			filteredAffinity = append(filteredAffinity, pod)
		} else if hasCapacity {
			filteredAvailable = append(filteredAvailable, pod)
		}
	}

	// Use crypto/rand for better randomization in production environments
	randSource := rand.NewSource(time.Now().UnixNano())
	randGen := rand.New(randSource)

	var targetPods []*v1.Pod

	// If both groups have pods, use probability to select which group to use
	switch {
	case len(filteredAffinity) > 0 && len(filteredAvailable) > 0:
		if randGen.Float64() < loraAffinityThreshold {
			targetPods = filteredAffinity
			klog.V(4).Infof("Selected affinity pods for model %s, count: %d", ctx.Model, len(filteredAffinity))
		} else {
			targetPods = filteredAvailable
			klog.V(4).Infof("Selected available pods for load balancing, model %s, count: %d", ctx.Model, len(filteredAvailable))
		}
	case len(filteredAffinity) > 0:
		targetPods = filteredAffinity
		klog.V(4).Infof("Only affinity pods available for model %s, count: %d", ctx.Model, len(filteredAffinity))
	case len(filteredAvailable) > 0:
		targetPods = filteredAvailable
		klog.V(4).Infof("Only available pods for model %s, count: %d", ctx.Model, len(filteredAvailable))
	}

	var targetPod *v1.Pod
	var err error

	// Select a LeastRequest pod from the chosen group
	if len(targetPods) > 0 {
		targetPod = selectTargetPodWithLeastRequestCount(r.cache, targetPods)
	} else {
		targetPod, err = utils.SelectRandomPod(readyPods, rand.Intn)
		if targetPod == nil {
			return "", fmt.Errorf("no pods to forward request")
		}
		if err != nil {
			return "", err
		}
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

// evaluatePodForLoraAffinity evaluates whether a pod has affinity for the target model
// and whether it has available capacity for new LoRA adapters.
// Returns (hasAffinity, hasCapacity)
func (r loraAffinityRouter) evaluatePodForLoraAffinity(pod *v1.Pod, targetModel string) (bool, bool) {
	// Get LoRA metrics for the pod
	runningAdapters, runningErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RunningLoraAdapters)
	waitingAdapters, waitingErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.WaitingLoraAdapters)
	maxAdapters, maxErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.WaitingLoraAdapters)

	// If we can't get metrics, consider the pod as having capacity but no affinity
	if runningErr != nil || waitingErr != nil || maxErr != nil {
		klog.V(4).Infof("Could not get LoRA metrics for pod %s: running=%v, waiting=%v, max=%v.",
			pod.Name, runningErr, waitingErr, maxErr)
		return false, true
	}

	runningModels := sets.NewString(strings.Split(runningAdapters.GetLabelValue(), ",")...)
	waitingModels := sets.NewString(strings.Split(waitingAdapters.GetLabelValue(), ",")...)
	maxCount, _ := strconv.Atoi(maxAdapters.GetLabelValue())

	// Check if the pod has affinity (target model is active or waiting)
	hasAffinity := r.checkModelAffinity(targetModel, runningModels, waitingModels)

	// Check if the pod has available capacity
	totalActiveAdapters := runningModels.Len() + waitingModels.Len()
	hasCapacity := totalActiveAdapters < maxCount

	klog.V(4).Infof("Pod %s LoRA evaluation: running=%d, waiting=%d, max=%d, hasAffinity=%v, hasCapacity=%v",
		pod.Name, runningModels.Len(), waitingModels.Len(), maxCount, hasAffinity, hasCapacity)

	return hasAffinity, hasCapacity
}

// checkModelAffinity checks if the pod has the target model active or waiting
func (r loraAffinityRouter) checkModelAffinity(targetModel string, runningModels, waitingModels sets.String) bool {
	return runningModels.Has(targetModel) || waitingModels.Has(targetModel)
}
