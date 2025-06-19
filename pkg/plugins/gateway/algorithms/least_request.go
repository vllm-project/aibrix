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
)

var (
	RouterLeastRequest types.RoutingAlgorithm = "least-request"
)

func init() {
	Register(RouterLeastRequest, NewLeastRequestRouter)
}

type leastRequestRouter struct {
	cache cache.Cache
}

func NewLeastRequestRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastRequestRouter{
		cache: c,
	}, nil
}

// Route request based of least active request among input ready pods
func (r leastRequestRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	targetPod := selectTargetPodWithLeastRequestCount(r.cache, readyPods)

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

func (r *leastRequestRouter) SubscribedMetrics() []string {
	return []string{
		metrics.RealtimeNumRequestsRunning,
	}
}

func selectTargetPodWithLeastRequestCount(cache cache.Cache, readyPods []*v1.Pod) *v1.Pod {
	var targetPod *v1.Pod
	targetPods := []string{}

	minCount := math.MaxInt32
	podRequestCount := getRequestCounts(cache, readyPods)
	for _, totalReq := range podRequestCount {
		if totalReq <= minCount {
			minCount = totalReq
		}
	}
	for podname, totalReq := range podRequestCount {
		if totalReq == minCount {
			targetPods = append(targetPods, podname)
		}
	}
	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
	}
	return targetPod
}

// getRequestCounts returns running request count for each pod tracked by gateway.
// Note: Currently, gateway instance tracks active running request counts for each pod locally,
// if multiple gateway instances are active then state is not shared across them.
// It is advised to run on leader gateway instance.
// TODO: Support stateful information sync across gateway instances: https://github.com/vllm-project/aibrix/issues/761
func getRequestCounts(cache cache.Cache, readyPods []*v1.Pod) map[string]int {
	podRequestCount := map[string]int{}
	for _, pod := range readyPods {
		runningReq, err := cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			runningReq = &metrics.SimpleMetricValue{Value: 0}
		}
		podRequestCount[pod.Name] = int(runningReq.GetSimpleValue())
	}

	return podRequestCount
}
