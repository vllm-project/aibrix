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

var (
	RouterLeastKvCache types.RoutingAlgorithm = "least-kv-cache"
)

func init() {
	RegisterDelayedConstructor(RouterLeastKvCache, NewLeastKvCacheRouter)
}

type leastKvCacheRouter struct {
	cache cache.Cache
}

func NewLeastKvCacheRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastKvCacheRouter{
		cache: c,
	}, nil
}

func (r leastKvCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var targetPod *v1.Pod
	minKvCache := math.MaxFloat64

	for _, pod := range readyPodList.All() {
		// Due to metric refactor (pull/543) to better support lora and multi models,
		// we change to use PodModelMetrics instead of PodMetrics in some scenarios.
		// This works but doesn't look very promising, we can revisit this part later.
		gpuCache, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.GPUCacheUsagePerc)
		if err != nil {
			klog.Error(err)
			continue
		}
		cpuCache, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.CPUCacheUsagePerc)
		if err != nil {
			klog.Error(err)
			continue
		}
		totalCache := gpuCache.GetSimpleValue() + cpuCache.GetSimpleValue()

		klog.V(4).Infof("pod: %v, podIP: %v, gpuCache: %v, cpuCache: %v, kaCache: %v",
			pod.Name, pod.Status.PodIP, gpuCache.GetSimpleValue(), cpuCache.GetSimpleValue(), totalCache)

		if totalCache <= minKvCache {
			minKvCache = totalCache
			targetPod = pod
		}
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		var err error
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPodList.All(), rand.Intn)
		if err != nil {
			return "", err
		}
	}

	if targetPod == nil {
		return "", fmt.Errorf("no pods to forward request")
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
