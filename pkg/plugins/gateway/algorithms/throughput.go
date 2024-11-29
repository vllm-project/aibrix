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

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type throughputRouter struct {
	cache *cache.Cache
}

func NewThroughputRouter() Router {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return throughputRouter{
		cache: cache,
	}
}

func (r throughputRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model string) (string, error) {
	var targetPodIP string
	minCount := math.MaxFloat64

	if len(pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		promptThroughput, err := r.cache.GetPodMetric(pod.Name, metrics.AvgPromptThroughputToksPerS)
		if err != nil {
			klog.Error(err)
			continue
		}
		generationThroughput, err := r.cache.GetPodMetric(pod.Name, metrics.AvgGenerationThroughputToksPerS)
		if err != nil {
			klog.Error(err)
			continue
		}

		// processing prompt tokens is twice as expensive than generation tokens
		totalThroughput := 2*promptThroughput.GetSimpleValue() + generationThroughput.GetSimpleValue()
		klog.V(4).Infof("pod: %v, podIP: %v, promptThroughput: %v, generationThroughput: %v, totalThroughput: %v",
			pod.Name, pod.Status.PodIP, promptThroughput, generationThroughput, totalThroughput)

		if totalThroughput <= minCount {
			minCount = totalThroughput
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
		return "", fmt.Errorf("no pods to forward request")
	}

	return targetPodIP + ":" + podMetricPort, nil
}

func (r *throughputRouter) SubscribedMetrics() []string {
	return []string{
		metrics.AvgPromptThroughputToksPerS,
		metrics.AvgGenerationThroughputToksPerS,
	}
}
