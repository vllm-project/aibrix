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
	"math"
	"math/rand"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	RouterThroughput types.RoutingAlgorithm = "throughput"
)

func init() {
	RegisterDelayedConstructor(RouterThroughput, NewThroughputRouter)
}

type throughputRouter struct {
	cache cache.Cache
}

func NewThroughputRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return throughputRouter{
		cache: c,
	}, nil
}

func (r throughputRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var targetPod *v1.Pod
	minCount := math.MaxFloat64

	readyPods := readyPodList.All()

	for _, pod := range readyPods {
		promptThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptThroughputToksPerS)
		if err != nil {
			klog.Error(err)
			continue
		}
		generationThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationThroughputToksPerS)
		if err != nil {
			klog.Error(err)
			continue
		}

		// processing prompt tokens is twice as expensive than generation tokens
		totalThroughput := 2*promptThroughput.GetSimpleValue() + generationThroughput.GetSimpleValue()
		klog.V(4).Infof("pod: %v, podIP: %v, promptThroughput: %v, generationThroughput: %v, totalThroughput: %v",
			pod.Name, pod.Status.PodIP, promptThroughput, generationThroughput, totalThroughput)

		if totalThroughput <= minCount {
			minCount = totalThroughput
			targetPod = pod
		}
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		var err error
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPods, rand.Intn)
		if err != nil {
			return "", err
		}
	}
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *throughputRouter) SubscribedMetrics() []string {
	return []string{
		metrics.AvgPromptThroughputToksPerS,
		metrics.AvgGenerationThroughputToksPerS,
	}
}
