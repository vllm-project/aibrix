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

	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/rate_limiter"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type throughputRouter struct {
	ratelimiter ratelimiter.AccountRateLimiter
}

func NewThroughputRouter(ratelimiter ratelimiter.AccountRateLimiter) Router {
	return throughputRouter{
		ratelimiter: ratelimiter,
	}
}

func (r throughputRouter) Get(ctx context.Context, pods []v1.Pod) (string, error) {
	var targetPodIP string
	minCount := math.MaxInt

	for _, pod := range pods {
		podIP := pod.Status.PodIP + ":8000"
		reqCount, err := r.ratelimiter.Get(ctx, fmt.Sprintf("%v_THROUGHPUT", podIP))
		if err != nil {
			return "", err
		}
		klog.Infof("PodIP: %s, PodThroughput: %v", podIP, reqCount)
		if reqCount <= int64(minCount) {
			minCount = int(reqCount)
			targetPodIP = podIP
		}
	}

	return targetPodIP, nil // TODO (varun): remove static port
}
