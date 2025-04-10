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
	"fmt"
	"math"
	"math/rand"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

var (
	RouterLeastBusyTime types.RoutingAlgorithm = "least-busy-time"
)

func init() {
	RegisterDelayedConstructor(RouterLeastBusyTime, NewLeastBusyTimeRouter)
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

func (r leastBusyTimeRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	var targetPod *v1.Pod
	minBusyTimeRatio := math.MaxFloat64 // <= 1 in general

	if pods.Len() == 0 {
		return "", fmt.Errorf("no available pods for request routing")
	}

	for _, pod := range pods.All() {
		if pod.Status.PodIP == "" {
			continue
		}

		busyTimeRatio, err := r.cache.GetMetricValueByPod(pod.Name, "gpu_busy_time_ratio") // todo: replace mock
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
		klog.Warning("No pods with valid metrics found; selecting a pod randomly as fallback")
		var err error
		targetPod, err = utils.SelectRandomPod(pods.All(), rand.Intn)
		if err != nil {
			return "", err
		}
	}

	if targetPod == nil {
		return "", fmt.Errorf("no available pods for request routing")
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
