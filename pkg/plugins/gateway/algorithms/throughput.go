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
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const RouterThroughput types.RoutingAlgorithm = "throughput"

func init() {
	Register(RouterThroughput, NewThroughputRouter)
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

// Polarity returns the polarity for throughput strategy
func (r throughputRouter) Polarity() types.Polarity {
	return types.PolarityLeast // Lower throughput (load) is better
}

// ScoreAll fetches the average prompt and generation tokens per request for all ready pods in a single batch operation.
// It combines these values to provide a unified throughput score for the multi-strategy aggregator.
func (r throughputRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		promptThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptToksPerReq)
		if err != nil {
			scored[i] = false
			continue
		}
		generationThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationToksPerReq)
		if err != nil {
			scored[i] = false
			continue
		}

		// processing prompt tokens is twice as expensive than generation tokens
		scores[i] = 2*promptThroughput.GetSimpleValue() + generationThroughput.GetSimpleValue()
		scored[i] = true
	}
	return scores, scored, nil
}

func (r throughputRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	scores, scored, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	var targetPods []string
	minCount := math.MaxFloat64 // we want to select the pod with the LEAST total weighted tokens processed
	pods := readyPodList.All()

	for i, pod := range pods {
		if !scored[i] {
			continue
		}

		klog.V(4).Infof("pod: %v, podIP: %v, totalThroughput: %v",
			pod.Name, pod.Status.PodIP, scores[i])

		if scores[i] < minCount {
			minCount = scores[i]
			targetPods = []string{pod.Name}
		} else if scores[i] == minCount {
			targetPods = append(targetPods, pod.Name)
		}
	}

	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], pods)
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		targetPod, err = SelectRandomPodAsFallback(ctx, pods, rand.Intn)
		if err != nil {
			return "", err
		}
	}
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *throughputRouter) SubscribedMetrics() []string {
	return []string{
		metrics.AvgPromptToksPerReq,
		metrics.AvgGenerationToksPerReq,
	}
}
