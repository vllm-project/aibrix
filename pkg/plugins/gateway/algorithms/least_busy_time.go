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

type leastBusyTimeRouter struct {
	ratelimiter ratelimiter.RateLimiter
	cache       *cache.Cache
}

func NewLeastBusyTimeRouter(ratelimiter ratelimiter.RateLimiter) Router {
	cacheFetched, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return leastBusyTimeRouter{
		ratelimiter: ratelimiter,
		cache:       cacheFetched,
	}
}

func (r leastBusyTimeRouter) Route(ctx context.Context, pods map[string]*v1.Pod) (string, error) {
	var targetPodIP string
	minBusyTime := math.MaxFloat64

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		busyTimeRatio, err := r.cache.GetPodMetric(pod.Name, "gpu_busy_time_ratio")
		if err != nil {
			klog.Error(err)
			continue
		}
		klog.V(4).Infof("pod: %v, podIP: %v, GPU busy time ratio: %v",
			pod.Name, pod.Status.PodIP, busyTimeRatio)

		if busyTimeRatio < minBusyTime {
			minBusyTime = busyTimeRatio
			targetPodIP = pod.Status.PodIP
		}
	}

	return targetPodIP, nil
}
