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

	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

const RouterLeastGpuCache types.RoutingAlgorithm = "least-gpu-cache"

func init() {
	Register(RouterLeastGpuCache, NewLeastGpuCacheRouter)
}

type leastGpuCacheRouter struct {
	cache cache.Cache
}

func NewLeastGpuCacheRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastGpuCacheRouter{
		cache: c,
	}, nil
}

func (r leastGpuCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var targetPod *v1.Pod
	minGpuCache := math.MaxFloat64
	var candidatePods []*v1.Pod

	for _, pod := range readyPodList.All() {
		gpuCache, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.GPUCacheUsagePerc)
		if err != nil {
			klog.Error(err)
			continue
		}
		totalCache := gpuCache.GetSimpleValue()

		klog.V(4).Infof("pod: %v, podIP: %v, gpuCache: %v",
			pod.Name, pod.Status.PodIP, gpuCache.GetSimpleValue())

		if totalCache < minGpuCache {
			minGpuCache = totalCache
			candidatePods = []*v1.Pod{pod}
		} else if totalCache == minGpuCache {
			candidatePods = append(candidatePods, pod)
		}
	}

	if len(candidatePods) > 0 {
		targetPod = candidatePods[rand.Intn(len(candidatePods))]
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		var err error
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPodList.All(), rand.Intn)
		if err != nil {
			return "", err
		}
		klog.V(4).Infof("select targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	} else {
		klog.V(4).Infof("select targetPod: %s(%s) gpuCache: %v", targetPod.Name, targetPod.Status.PodIP, minGpuCache)
	}

	if targetPod == nil {
		return "", fmt.Errorf("no pods to forward request")
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
