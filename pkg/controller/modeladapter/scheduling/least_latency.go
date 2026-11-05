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
	selectedPod := v1.Pod{}
	podLatencyMin := math.MaxFloat64

	var lastErr error
	for _, pod := range readyPods {
		queueTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metrics.RequestQueueTimeSeconds)
		if err != nil {
			lastErr = err
			continue
		}
		queueHist := queueTime.GetHistogramValue()
		if queueHist == nil {
			lastErr = fmt.Errorf("metric is not a histogram: %s", metrics.RequestQueueTimeSeconds)
			continue
		}
		inferenceTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metrics.RequestInferenceTimeSeconds)
		if err != nil {
			lastErr = err
			continue
		}
		inferenceHist := inferenceTime.GetHistogramValue()
		if inferenceHist == nil {
			lastErr = fmt.Errorf("metric is not a histogram: %s", metrics.RequestInferenceTimeSeconds)
			continue
		}
		queueMean := cache.RecentHistogramMean(
			r.cache, pod.Name, pod.Namespace, model, metrics.RequestQueueTimeSeconds, queueHist)
		inferenceMean := cache.RecentHistogramMean(
			r.cache, pod.Name, pod.Namespace, model, metrics.RequestInferenceTimeSeconds, inferenceHist)
		podLatency := queueMean + inferenceMean
		if podLatency < podLatencyMin {
			selectedPod = pod
			podLatencyMin = podLatency
		}
	}
	if podLatencyMin == math.MaxFloat64 {
		if lastErr == nil {
			return nil, fmt.Errorf("no ready pods to schedule on")
		}
		return nil, fmt.Errorf("no ready pod has latency metrics: %w", lastErr)
	}

	klog.InfoS("pod selected with least latency", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
