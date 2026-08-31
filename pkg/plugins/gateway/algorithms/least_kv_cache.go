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
	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

const RouterLeastKvCache types.RoutingAlgorithm = "least-kv-cache"

func init() {
	Register(RouterLeastKvCache, NewLeastKvCacheRouter)
}

type leastKvCacheRouter struct {
	cache cache.Cache
}

func NewLeastKvCacheRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return newLeastKvCacheRouter(c), nil
}

// newLeastKvCacheRouter builds a leastKvCacheRouter around an existing cache handle, for
// callers that already hold one (e.g. load_balance.go reusing it purely as a types.PodScorer
// tie-breaker) rather than fetching their own via cache.Get(). Keeping construction here means
// a future change to this struct's fields only needs updating in this file.
func newLeastKvCacheRouter(c cache.Cache) leastKvCacheRouter {
	return leastKvCacheRouter{cache: c}
}

// cpuCacheUsage returns the pod's CPU KV-cache usage, or 0 when the engine does not report
// it. vLLM's V1 engine dropped cpu_cache_usage_perc along with KV swapping (it recomputes
// preempted requests instead), so on V1 the metric is simply absent and GPU usage alone
// describes the pod's KV pressure. Treating that as fatal would mark every pod unscored and
// drop this router to its random fallback, which is worse than the load-aware choice this
// router exists for.
func cpuCacheUsage(c cache.Cache, pod *v1.Pod, model string) float64 {
	cpuCache, err := c.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metrics.CPUCacheUsagePerc)
	if err != nil {
		return 0
	}
	return cpuCache.GetSimpleValue()
}

// ScoreAll computes the combined GPU and CPU cache usage percentage for all ready pods in a single batch operation.
// This combined metric allows the multi-strategy aggregator to evaluate the overall KV cache pressure on each pod.
func (r leastKvCacheRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		gpuCache, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.KVCacheUsagePerc)
		if err != nil {
			klog.V(4).ErrorS(err, "failed to get GPU cache metrics")
			continue
		}
		scores[i] = gpuCache.GetSimpleValue() + cpuCacheUsage(r.cache, pod, ctx.Model)
		scored[i] = true
		klog.V(4).Infof("pod: %v, podIP: %v, total cache: %v", pod.Name, pod.Status.PodIP, scores[i])
	}

	return scores, scored, nil
}

// Polarity returns whether higher or lower score is better.
func (r leastKvCacheRouter) Polarity() types.Polarity {
	return types.PolarityLeast
}

func (r leastKvCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	targetPod, err := RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}
