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

var RouterLoadBalance types.RoutingAlgorithm = "load-balance"

func init() {
	Register(RouterLoadBalance, NewLoadBalanceRouter)
}

type loadBalanceRouter struct {
	cache cache.Cache
}

func NewLoadBalanceRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return &loadBalanceRouter{cache: c}, nil
}

// ScoreAll returns pending_time = request_count / capacity for each pod.
// Lower pending_time means the pod has more headroom to accept this request.
func (r *loadBalanceRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		reqCount := 0.0
		if v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning); err == nil {
			reqCount = v.GetSimpleValue()
		}
		scores[i] = reqCount / r.capacityOf(pod)
		scored[i] = true
	}

	klog.V(4).InfoS("load_balance_scores",
		"request_id", ctx.RequestID,
		"model", ctx.Model,
		"pod_count", len(pods))

	return scores, scored, nil
}

// Polarity indicates lower pending_time is better.
func (r *loadBalanceRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

// Route selects the pod with minimum pending_time, breaking ties randomly.
func (r *loadBalanceRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", ErrorNoAvailablePod
	}

	scores, _, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	minScore := math.MaxFloat64
	var candidates []string
	for i, pod := range pods {
		s := scores[i]
		if s < minScore {
			minScore = s
			candidates = []string{pod.Name}
		} else if s == minScore {
			candidates = append(candidates, pod.Name)
		}
	}

	if len(candidates) == 0 {
		return "", ErrorNoAvailablePod
	}

	targetPod, _ := utils.FilterPodByName(candidates[rand.Intn(len(candidates))], pods)
	if targetPod == nil {
		return "", ErrorNoAvailablePod
	}

	klog.V(4).InfoS("load_balance_selected",
		"request_id", ctx.RequestID,
		"target_pod", targetPod.Name,
		"pending_time", minScore)

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

// capacityOf returns the throughput capacity of a pod.
// Uses observed drain rate from cache when available; falls back to 1.0 (uniform).
func (r *loadBalanceRouter) capacityOf(pod *v1.Pod) float64 {
	if v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeRunningRequestsDrainRate1m); err == nil {
		if dr := v.GetSimpleValue(); dr > 0 {
			return dr
		}
	}
	return 1.0
}
