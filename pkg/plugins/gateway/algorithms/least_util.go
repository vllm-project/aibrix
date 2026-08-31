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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	klog "k8s.io/klog/v2"
)

const RouterUtil types.RoutingAlgorithm = "least-utilization"

func init() {
	Register(RouterUtil, NewLeastUtilRouter)
}

type leastUtilRouter struct {
	cache cache.Cache
}

func NewLeastUtilRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastUtilRouter{
		cache: c,
	}, nil
}

// ScoreAll fetches the engine utilization metric for all ready pods in a single batch operation.
// This float value is used by the multi-strategy aggregator to evaluate the current utilization load of each pod.
func (r leastUtilRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		metricVal, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.EngineUtilization)
		if err != nil {
			klog.V(4).ErrorS(err, "failed to get metrics for pod")
			continue
		}
		scores[i] = metricVal.GetSimpleValue()
		scored[i] = true
		klog.V(4).Infof("pod: %v, podIP: %v, engine utilization: %v", pod.Name, pod.Status.PodIP, scores[i])
	}

	return scores, scored, nil
}

// Polarity returns whether higher or lower score is better.
func (r leastUtilRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r leastUtilRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	targetPod, err := RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
