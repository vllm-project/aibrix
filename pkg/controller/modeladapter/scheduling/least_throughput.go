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

	var lastErr error
	for _, pod := range readyPods {
		// Both throughputs are pod-scoped PromQL metrics: the cache keeps them per pod, not per
		// pod and model, and holds query results, whose simple value is always 0.
		promptThroughput, err := r.podThroughput(pod, metrics.AvgPromptThroughputToksPerMinPod)
		if err != nil {
			lastErr = err
			continue
		}
		generationThroughput, err := r.podThroughput(pod, metrics.AvgGenerationThroughputToksPerMinPod)
		if err != nil {
			lastErr = err
			continue
		}
		podThroughput := promptThroughput*2 + generationThroughput
		if podThroughput < podThroughputMin {
			selectedPod = pod
			podThroughputMin = podThroughput
		}
	}
	if podThroughputMin == math.MaxFloat64 {
		return nil, fmt.Errorf("no ready pod has throughput metrics: %w", lastErr)
	}

	klog.InfoS("pod selected with least throughput", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}

// podThroughput reads one of the pod's throughput metrics, failing when the cache has no finite
// value for it.
func (r leastThroughputScheduler) podThroughput(pod v1.Pod, metricName string) (float64, error) {
	metricValue, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metricName)
	if err != nil {
		return 0, err
	}
	value, ok := metrics.NumericValue(metricValue)
	if !ok {
		return 0, fmt.Errorf("pod %s/%s has no numeric %s", pod.Namespace, pod.Name, metricName)
	}
	return value, nil
}
