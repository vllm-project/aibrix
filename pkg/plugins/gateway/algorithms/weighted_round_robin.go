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
	"context"
	"math"

	"github.com/aibrix/aibrix/pkg/cache"
	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/ratelimiter"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type weightedRoundRobinRouter struct {
	ratelimiter ratelimiter.RateLimiter
	cache       *cache.Cache
}

func NewWeightedRoundRobinRouter(ratelimiter ratelimiter.RateLimiter) Router {
	cache_fetched, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return weightedRoundRobinRouter{
		ratelimiter: ratelimiter,
		cache:       cache_fetched,
	}
}

func (r weightedRoundRobinRouter) Route(ctx context.Context, pods map[string]*v1.Pod) (string, error) {
	// weightRoundRobin需要考虑pod状态异常、 pod增减的情况
	var targetPodIP string
	roundRobinCurrentIdx := ctx.weightedRoundRobinIdx
	totalPodsNum := len(pods)

	currentDealPodIdx := 0
	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		} else {
			targetPodIP = pod.Status.PodIP
		}

		if currentDealPodIdx == roundRobinCurrentIdx {
			break
		}

		currentDealPodIdx = currentDealPodIdx + 1
	}

	roundRobinCurrentIdx = (roundRobinCurrentIdx + 1) % totalPodsNum
	ctx.setWeightedRoundRobinIdx(roundRobinCurrentIdx)
	return targetPodIP, nil
}
