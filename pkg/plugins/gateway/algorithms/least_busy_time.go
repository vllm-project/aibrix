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
	"math/rand"

	"github.com/vllm-project/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	RouterLeastBusyTime Algorithms = "least-busy-time"
)

func init() {
	Register(RouterLeastBusyTime, func() (Router, error) { return NewLeastBusyTimeRouter() })
}

type leastBusyTimeRouter struct {
	cache *cache.Cache
}

func NewLeastBusyTimeRouter() (Router, error) {
	c, err := cache.GetCache()
	if err != nil {
		return nil, err
	}

	return leastBusyTimeRouter{
		cache: c,
	}, nil
}

func (r leastBusyTimeRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error) {
	var targetPodIP string
	minBusyTimeRatio := math.MaxFloat64 // <= 1 in general

	if len(pods) == 0 {
		return "", fmt.Errorf("no available pods for request routing")
	}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		busyTimeRatio, err := r.cache.GetPodMetric(pod.Name, "gpu_busy_time_ratio") // todo: replace mock
		if err != nil {
			klog.Error(err)
			continue
		}
		busyTimeRatioValue := busyTimeRatio.GetSimpleValue()
		klog.V(4).Infof("pod: %v, podIP: %v, GPU busy time ratio: %v", pod.Name, pod.Status.PodIP, busyTimeRatioValue)

		if busyTimeRatioValue < minBusyTimeRatio {
			minBusyTimeRatio = busyTimeRatioValue
			targetPodIP = pod.Status.PodIP
		}
	}

	// Use fallback if no valid metrics
	if targetPodIP == "" {
		klog.Warning("No pods with valid metrics found; selecting a pod randomly as fallback")
		var err error
		targetPodIP, err = selectRandomPod(pods, rand.Intn)
		if err != nil {
			return "", err
		}
	}

	if targetPodIP == "" {
		return "", fmt.Errorf("no available pods for request routing")
	}

	return targetPodIP + ":" + podMetricPort, nil
}
