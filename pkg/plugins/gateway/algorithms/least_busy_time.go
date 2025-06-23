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
	klog "k8s.io/klog/v2"
)

var (
	RouterLeastBusyTime types.RoutingAlgorithm = "least-busy-time"
)

func init() {
	Register(RouterLeastBusyTime, NewLeastBusyTimeRouter)
}

type leastBusyTimeRouter struct {
	cache cache.Cache
}

func NewLeastBusyTimeRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastBusyTimeRouter{
		cache: c,
	}, nil
}

func (r leastBusyTimeRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var targetPod *v1.Pod
	minBusyTimeRatio := math.MaxFloat64 // <= 1 in general

	for _, pod := range readyPodList.All() {
		busyTimeRatio, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.GPUBusyTimeRatio) // todo: replace mock
		if err != nil {
			klog.Error(err)
			continue
		}
		busyTimeRatioValue := busyTimeRatio.GetSimpleValue()
		klog.V(4).Infof("pod: %v, podIP: %v, GPU busy time ratio: %v", pod.Name, pod.Status.PodIP, busyTimeRatioValue)

		if busyTimeRatioValue < minBusyTimeRatio {
			minBusyTimeRatio = busyTimeRatioValue
			targetPod = pod
		}
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		var err error
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPodList.All(), rand.Intn)
		if err != nil {
			return "", err
		}
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
