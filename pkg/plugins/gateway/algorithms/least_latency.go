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

const RouterLeastLatency types.RoutingAlgorithm = "least-latency"

func init() {
	Register(RouterLeastLatency, NewLeastExpectedLatencyRouter)
}

type leastExpectedLatencyRouter struct {
	cache cache.Cache
}

func NewLeastExpectedLatencyRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return leastExpectedLatencyRouter{
		cache: c,
	}, nil
}

func (r leastExpectedLatencyRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var targetPod *v1.Pod
	minExpectedLatency := math.MaxFloat64

	sumPromptTokens := 0.0
	sumGenerationTokens := 0.0
	cntPromt := 0
	cntGeneration := 0
	for _, pod := range readyPodList.All() {
		avgPromptTokens, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		avgGenerationTokens, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationToksPerReq)
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

	for _, pod := range readyPodList.All() {
		// expected queuing latency
		queuingLatency, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestQueueTimeSeconds)
		if err != nil {
			klog.Error(err)
			continue
		}

		// expected prefill latency
		avgPromptTokens, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		PrefillTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestPrefillTimeSeconds)
		if err != nil {
			klog.Error(err)
			continue
		}
		prefillLatency := PrefillTime.GetHistogramValue().GetMean() / avgPromptTokens.GetSimpleValue() * guessPromptTokens

		// expected decode latency
		avgGenerationTokens, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationToksPerReq)
		if err != nil {
			klog.Error(err)
			continue
		}
		DecodeTime, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestDecodeTimeSeconds)
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
