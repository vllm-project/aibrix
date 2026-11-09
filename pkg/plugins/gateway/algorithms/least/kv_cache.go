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
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
	klog "k8s.io/klog/v2"
)

const RouterLeastKvCache types.RoutingAlgorithm = "least-kv-cache"

// RouterLeastGpuCache is a deprecated alias for RouterLeastKvCache. The standalone least-gpu-cache
// router scored the legacy gpu_cache_usage_perc metric, while every other KV-aware strategy
// (least-kv-cache, load-balance, pd) reads kv_cache_usage_perc. The name stays registered so
// existing routing-strategy headers and config profiles keep working, but it resolves to
// least-kv-cache.
const RouterLeastGpuCache types.RoutingAlgorithm = "least-gpu-cache"

type leastKvCacheRouter struct {
	cache          cache.Cache
	now            func() time.Time
	staleThreshold time.Duration
}

func NewLeastKvCacheRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return newLeastKvCacheRouter(c), nil
}

// NewLeastKvCacheRouterWithCache constructs least-kv-cache with an explicit cache.
func NewLeastKvCacheRouterWithCache(c cache.Cache) (types.Router, error) {
	return newLeastKvCacheRouter(c), nil
}

// NewLeastKvCacheScorer returns the least-kv-cache strategy as a types.PodScorer around an
// existing cache handle, for callers that only want its scores (e.g. load-balance using it as a
// tie-breaker) rather than a registered router.
func NewLeastKvCacheScorer(c cache.Cache) types.PodScorer {
	return newLeastKvCacheRouter(c)
}

// newLeastKvCacheRouter builds a leastKvCacheRouter around an existing cache handle, so
// construction stays in one place if this struct's fields change.
func newLeastKvCacheRouter(c cache.Cache) leastKvCacheRouter {
	return leastKvCacheRouter{cache: c, staleThreshold: cache.RoutingMetricStaleThreshold()}
}

// ScoreAll computes the combined GPU and CPU cache usage percentage for all ready pods in a single batch operation.
// This combined metric allows the multi-strategy aggregator to evaluate the overall KV cache pressure on each pod.
func (r leastKvCacheRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	threshold := r.staleThreshold
	if threshold <= 0 {
		threshold = cache.RoutingMetricStaleThreshold()
	}

	for i, pod := range pods {
		gpu, gpuOK := routeutil.FreshKVUsage(r.cache, pod, ctx.Model, metrics.KVCacheUsagePerc, now, threshold)
		if !gpuOK {
			continue
		}
		cpu, cpuOK := routeutil.FreshKVUsage(r.cache, pod, ctx.Model, metrics.CPUCacheUsagePerc, now, threshold)
		if !cpuOK {
			continue
		}
		scores[i] = gpu + cpu
		scored[i] = true
		if klog.V(4).Enabled() {
			klog.V(4).Infof("pod: %v, podIP: %v, total cache: %v", pod.Name, pod.Status.PodIP, scores[i])
		}
	}

	return scores, scored, nil
}

// Polarity returns whether higher or lower score is better.
func (r leastKvCacheRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r leastKvCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	targetPod, err := routeutil.RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
