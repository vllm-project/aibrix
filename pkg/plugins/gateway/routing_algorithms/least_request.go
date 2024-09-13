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
	"fmt"
	"math"

	"github.com/aibrix/aibrix/pkg/cache"
	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/rate_limiter"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastRequestRouter struct {
	ratelimiter ratelimiter.AccountRateLimiter
	cache       *cache.Cache
}

func NewLeastRequestRouter(ratelimiter ratelimiter.AccountRateLimiter) Router {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return leastRequestRouter{
		ratelimiter: ratelimiter,
		cache:       cache,
	}
}

func (r leastRequestRouter) Get(ctx context.Context, pods []v1.Pod) (string, error) {
	var targetPodIP string
	minCount := math.MaxInt
	podRequestCounts := r.cache.GetPodRequestCount()

	for _, pod := range pods {
		podIP := pod.Status.PodIP + ":8000"
		podRequestCount := fmt.Sprintf("%v_REQUEST_COUNT", podIP)

		reqCount := podRequestCounts[podRequestCount]
		klog.Infof("PodIP: %s, PodRequestCount: %v", podIP, reqCount)
		if reqCount <= minCount {
			minCount = reqCount
			targetPodIP = podIP
		}
	}

	return targetPodIP, nil // TODO (varun): remove static port
}
