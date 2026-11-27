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

package least

import (
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
)

const RouterLeastLatency types.RoutingAlgorithm = "least-latency"

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

// NewLeastLatencyRouterWithCache constructs least-latency with an explicit cache.
func NewLeastLatencyRouterWithCache(c cache.Cache) (types.Router, error) {
	return leastExpectedLatencyRouter{cache: c}, nil
}

// Polarity returns the polarity for least-latency strategy
func (r leastExpectedLatencyRouter) Polarity() types.Polarity {
	return types.PolarityLeast // The lower the expected latency, the better
}

// CandidateScores implements [types.CandidateScorer]. ScoreAll reads only scraped metrics held
// in memory, so it already is the estimate.
func (r leastExpectedLatencyRouter) CandidateScores(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	return r.ScoreAll(ctx, readyPodList)
}

// ScoreAll computes the expected latency for all pods
func (r leastExpectedLatencyRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	// First, compute the average prompt/generation tokens across all pods to guess
	// if metrics for the current pod are missing
	sumPromptTokens := 0.0
	sumGenerationTokens := 0.0
	cntPromt := 0
	cntGeneration := 0
	for _, p := range pods {
		// The token averages are PromQL results; their simple value is always 0.
		if avgPromptTokens, err := r.cache.GetMetricValueByPodModel(p.Name, p.Namespace, ctx.Model, metrics.AvgPromptToksPerReq); err == nil {
			if v, ok := metrics.NumericValue(avgPromptTokens); ok && v > 0 {
				sumPromptTokens += v
				cntPromt += 1
			}
		}
		if avgGenerationTokens, err := r.cache.GetMetricValueByPodModel(p.Name, p.Namespace, ctx.Model, metrics.AvgGenerationToksPerReq); err == nil {
			if v, ok := metrics.NumericValue(avgGenerationTokens); ok && v > 0 {
				sumGenerationTokens += v
				cntGeneration += 1
			}
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

	for i, pod := range pods {
		// Calculate latency components
		queuingLatencyMetric, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestQueueTimeSeconds)
		if err != nil {
			scored[i] = false
			continue
		}
		// request_queue_time_seconds is a histogram; its simple value is always 0.
		queuingLatencyHistogram := queuingLatencyMetric.GetHistogramValue()
		if queuingLatencyHistogram == nil {
			scored[i] = false
			continue
		}

		avgPromptTokensMetric, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgPromptToksPerReq)
		if err != nil {
			scored[i] = false
			continue
		}
		avgPromptTokens := guessPromptTokens
		if v, ok := metrics.NumericValue(avgPromptTokensMetric); ok && v > 0 {
			avgPromptTokens = v
		}

		prefillTimeMetric, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestPrefillTimeSeconds)
		if err != nil {
			scored[i] = false
			continue
		}
		prefillTimeHistogram := prefillTimeMetric.GetHistogramValue()
		if prefillTimeHistogram == nil {
			scored[i] = false
			continue
		}
		prefillTime := cache.RecentHistogramMean(r.cache, pod.Name, pod.Namespace, ctx.Model, metrics.RequestPrefillTimeSeconds, prefillTimeHistogram)
		prefillLatency := prefillTime / avgPromptTokens * guessPromptTokens

		avgGenTokensMetric, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationToksPerReq)
		if err != nil {
			scored[i] = false
			continue
		}
		avgGenerationTokens := guessGenerationTokens
		if v, ok := metrics.NumericValue(avgGenTokensMetric); ok && v > 0 {
			avgGenerationTokens = v
		}

		decodeTimeMetric, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.RequestDecodeTimeSeconds)
		if err != nil {
			scored[i] = false
			continue
		}
		decodeTimeHistogram := decodeTimeMetric.GetHistogramValue()
		if decodeTimeHistogram == nil {
			scored[i] = false
			continue
		}
		decodeTime := cache.RecentHistogramMean(r.cache, pod.Name, pod.Namespace, ctx.Model, metrics.RequestDecodeTimeSeconds, decodeTimeHistogram)
		decodeLatency := decodeTime / avgGenerationTokens * guessGenerationTokens

		queuingLatency := cache.RecentHistogramMean(r.cache, pod.Name, pod.Namespace, ctx.Model, metrics.RequestQueueTimeSeconds, queuingLatencyHistogram)
		scores[i] = queuingLatency + prefillLatency + decodeLatency
		scored[i] = true
	}

	return scores, scored, nil
}

func (r leastExpectedLatencyRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	targetPod, err := routeutil.RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
