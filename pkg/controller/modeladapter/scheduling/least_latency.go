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

type leastLatencyScheduler struct {
	cache cache.Cache
}

func NewLeastLatencyScheduler(c cache.Cache) Scheduler {
	return leastLatencyScheduler{
		cache: c,
	}
}

func (r leastLatencyScheduler) SelectPod(ctx context.Context, model string, readyPods []v1.Pod) (*v1.Pod, error) {
	if len(readyPods) == 0 {
		return nil, fmt.Errorf("no ready pods to schedule on")
	}

	selectedPod := v1.Pod{}
	podLatencyMin := math.MaxFloat64

	var lastErr error
	for _, pod := range readyPods {
		podLatency, err := r.podLatency(pod, model)
		if err != nil {
			lastErr = err
			klog.V(4).InfoS("Skipping pod without usable latency metrics",
				"pod", klog.KObj(&pod), "model", model, "reason", err)
			continue
		}
		if podLatency < podLatencyMin {
			selectedPod = pod
			podLatencyMin = podLatency
		}
	}
	if podLatencyMin == math.MaxFloat64 {
		if lastErr == nil {
			return nil, fmt.Errorf("no ready pod has latency metrics")
		}
		return nil, fmt.Errorf("no ready pod has latency metrics: %w", lastErr)
	}

	klog.InfoS("pod selected with least latency", "pod", klog.KObj(&selectedPod), "model", model)
	return &selectedPod, nil
}

// podLatency returns the pod's recent queue + inference mean, failing when either
// metric is missing, is not a histogram, or the total is not finite.
func (r leastLatencyScheduler) podLatency(pod v1.Pod, model string) (float64, error) {
	queueTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metrics.RequestQueueTimeSeconds)
	if err != nil {
		return 0, err
	}
	if queueTime == nil {
		return 0, fmt.Errorf("pod %s/%s has no %s", pod.Namespace, pod.Name, metrics.RequestQueueTimeSeconds)
	}
	queueHist := queueTime.GetHistogramValue()
	if queueHist == nil {
		return 0, fmt.Errorf("metric is not a histogram: %s", metrics.RequestQueueTimeSeconds)
	}

	inferenceTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metrics.RequestInferenceTimeSeconds)
	if err != nil {
		return 0, err
	}
	if inferenceTime == nil {
		return 0, fmt.Errorf("pod %s/%s has no %s", pod.Namespace, pod.Name, metrics.RequestInferenceTimeSeconds)
	}
	inferenceHist := inferenceTime.GetHistogramValue()
	if inferenceHist == nil {
		return 0, fmt.Errorf("metric is not a histogram: %s", metrics.RequestInferenceTimeSeconds)
	}

	queueMean := cache.RecentHistogramMean(
		r.cache, pod.Name, pod.Namespace, model, metrics.RequestQueueTimeSeconds, queueHist)
	inferenceMean := cache.RecentHistogramMean(
		r.cache, pod.Name, pod.Namespace, model, metrics.RequestInferenceTimeSeconds, inferenceHist)
	total := queueMean + inferenceMean
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, fmt.Errorf("pod %s/%s has non-finite latency: %v", pod.Namespace, pod.Name, total)
	}
	return total, nil
}
