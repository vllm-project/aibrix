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

package scheduling

import (
	"context"
	"fmt"
	"math"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastThroughputScheduler struct {
	cache cache.Cache
}

func NewLeastThroughputScheduler(c cache.Cache) Scheduler {
	return leastThroughputScheduler{
		cache: c,
	}
}

func (r leastThroughputScheduler) SelectPod(ctx context.Context, model string, readyPods []v1.Pod) (*v1.Pod, error) {
	selectedPod := v1.Pod{}
	podThroughputMin := math.MaxFloat64
	scored := false

	for _, pod := range readyPods {
		// Both throughputs are pod-scoped PromQL results, so read them where the
		// PromQL worker stores them and take the number from the query result.
		promptThroughput, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.AvgPromptThroughputToksPerMinPod)
		if err != nil {
			continue
		}
		generationThroughput, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.AvgGenerationThroughputToksPerMinPod)
		if err != nil {
			continue
		}
		promptTokens, promptOK := metrics.NumericValue(promptThroughput)
		generationTokens, generationOK := metrics.NumericValue(generationThroughput)
		if !promptOK || !generationOK {
			continue
		}
		// Processing prompt tokens is twice as expensive as generating them.
		podThroughput := 2*promptTokens + generationTokens
		if podThroughput < podThroughputMin {
			selectedPod = pod
			podThroughputMin = podThroughput
			scored = true
		}
	}

	if !scored {
		return nil, fmt.Errorf("no pod reports %s and %s", metrics.AvgPromptThroughputToksPerMinPod, metrics.AvgGenerationThroughputToksPerMinPod)
	}

	klog.InfoS("pod selected with least throughput", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
