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

type leastExpectedLatencyRouter struct {
	cache *cache.Cache
}

func NewLeastExpectedLatencyRouter() Router {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return leastExpectedLatencyRouter{
		cache: cache,
	}
}

func (r leastExpectedLatencyRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model string) (string, error) {
	var targetPodIP string
	minExpectedLatency := math.MaxFloat64

	if len(pods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	sumPromptTokens := 0.0
	sumGenerationTokens := 0.0
	cntPromt := 0
	cntGeneration := 0
	for _, pod := range pods {
		avgPromptTokens, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.AvgPromptToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		avgGenerationTokens, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.AvgGenerationToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		if avgPromptTokens.GetSimpleValue() > 0 {
			sumPromptTokens += avgPromptTokens.GetSimpleValue()
			cntPromt += 1
		}
		if avgGenerationTokens.GetSimpleValue() > 0 {
			sumGenerationTokens += avgGenerationTokens.GetSimpleValue()
			cntGeneration += 1
		}
	}
	guessPromptTokens := 10.0
	if cntPromt > 0 {
		guessPromptTokens = sumPromptTokens / float64(cntPromt)
	}
	guessGenerationTokens := 100.0
	if cntGeneration > 0 {
		guessGenerationTokens = sumGenerationTokens / float64(cntGeneration)
	}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		// expected queuing latency
		queuingLatency, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.RequestQueueTimeSeconds)
		if err != nil {
			klog.Error(err)
			continue
		}

		// expected prefill latency
		avgPromptTokens, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.AvgPromptToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		PrefillTime, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.RequestPrefillTimeSeconds)
		if err != nil {
			klog.Error(err)
			continue
		}
		prefillLatency := PrefillTime.GetHistogramValue().GetMean() / avgPromptTokens.GetSimpleValue() * guessPromptTokens

		// expected decode latency
		avgGenerationTokens, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.AvgGenerationToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		DecodeTime, err := r.cache.GetPodModelMetric(pod.Name, model, metrics.RequestDecodeTimeSeconds)
		if err != nil {
			klog.Error(err)
			continue
		}
		decodeLatency := DecodeTime.GetHistogramValue().GetMean() / avgGenerationTokens.GetSimpleValue() * guessGenerationTokens

		totalExpectedLatency := queuingLatency.GetSimpleValue() + prefillLatency + decodeLatency
		klog.V(4).Infof("pod: %v, podIP: %v, queuingLatency: %v, prefillLatency: %v, decodeLatency: %v, totalExpectedLatency: %v",
			pod.Name, pod.Status.PodIP, queuingLatency.GetSimpleValue(), prefillLatency, decodeLatency, totalExpectedLatency)

		if totalExpectedLatency <= minExpectedLatency {
			minExpectedLatency = totalExpectedLatency
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
