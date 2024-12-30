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
	"math"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type leastLatencyScheduler struct {
	cache *cache.Cache
}

func NewLeastLatencyScheduler(c *cache.Cache) Scheduler {
	return leastLatencyScheduler{
		cache: c,
	}
}

func (r leastLatencyScheduler) SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error) {
	selectedPod := v1.Pod{}
	podLatencyMin := math.MaxFloat64

	for _, pod := range pods {
		queueTime, err := r.cache.GetPodMetric(pod.Name, metrics.RequestQueueTimeSeconds)
		if err != nil {
			return nil, err
		}
		inferenceTime, err := r.cache.GetPodMetric(pod.Name, metrics.RequestInferenceTimeSeconds)
		if err != nil {
			return nil, err
		}
		podLatency := queueTime.GetHistogramValue().GetMean() + inferenceTime.GetHistogramValue().GetMean()
		if podLatency < podLatencyMin {
			selectedPod = pod
			podLatencyMin = podLatency
		}
	}

	klog.InfoS("pod selected with least latency", "pod", klog.KObj(&selectedPod))
	return &selectedPod, nil
}
