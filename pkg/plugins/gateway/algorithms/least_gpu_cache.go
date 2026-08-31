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
	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
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

// ScoreAll fetches the GPU cache usage percentage for all ready pods in a single batch operation.
// This enables the multi-strategy aggregator to penalize or reward pods based on their available GPU cache capacity.
func (r leastGpuCacheRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		metricVal, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.GPUCacheUsagePerc)
		if err != nil {
			klog.V(4).ErrorS(err, "failed to get metrics for pod")
			continue
		}
		scores[i] = metricVal.GetSimpleValue()
		scored[i] = true
		klog.V(4).Infof("pod: %v, podIP: %v, gpu cache: %v", pod.Name, pod.Status.PodIP, scores[i])
	}

	return scores, scored, nil
}

// Polarity returns whether higher or lower score is better.
func (r leastGpuCacheRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r leastGpuCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	targetPod, err := RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
