/*
Copyright 2026 The Aibrix Team.

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

package routeutil

import (
	"errors"
	"math"
	"math/rand"
	"strconv"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"

	"k8s.io/klog/v2"
)

// SelectTargetPodWithLeastRequestCount picks, uniformly at random, one of the readyPods with
// the fewest running requests according to cache.
func SelectTargetPodWithLeastRequestCount(cache cache.Cache, readyPods []*v1.Pod) *v1.Pod {
	return SelectTargetPodWithLeastRequestCountFromCounts(GetRequestCounts(cache, readyPods), readyPods)
}

// SelectTargetPodWithLeastRequestCountFromCounts is SelectTargetPodWithLeastRequestCount over
// request counts the caller already holds, keyed by pod key.
func SelectTargetPodWithLeastRequestCountFromCounts(podRequestCount map[string]int, readyPods []*v1.Pod) *v1.Pod {
	var targetPod *v1.Pod
	targetPods := []string{}

	minCount := math.MaxInt32
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("selectTargetPodWithLeastRequestCount", "podRequestCount", podRequestCount)
	}
	for podKey, totalReq := range podRequestCount {
		if totalReq < minCount {
			minCount = totalReq
			targetPods = []string{podKey}
		} else if totalReq == minCount {
			targetPods = append(targetPods, podKey)
		}
	}
	if len(targetPods) > 0 {
		targetPod = PodsByKey(readyPods)[targetPods[rand.Intn(len(targetPods))]]
	}
	return targetPod
}

// SelectTargetPortForPodWithLeastRequestCount picks the least loaded port of pod, or 0 when
// pod has no known port. portsMap is keyed by pod key.
func SelectTargetPortForPodWithLeastRequestCount(cache cache.Cache, pod *v1.Pod, portsMap map[string][]int) int {
	if pod == nil {
		return 0
	}
	podPorts := portsMap[utils.GeneratePodKey(pod.Namespace, pod.Name)]
	if len(podPorts) == 0 {
		return 0
	}
	if len(podPorts) == 1 {
		return podPorts[0]
	}

	minCount := math.MaxInt32
	targetPorts := make([]int, 0, len(podPorts))
	for _, port := range podPorts {
		metricName := metrics.RealtimeNumRequestsRunning + "/" + strconv.Itoa(port)
		count := 0
		if val, err := cache.GetMetricValueByPod(pod.Name, pod.Namespace, metricName); err == nil && val != nil {
			count = int(val.GetSimpleValue())
		}
		if count < minCount {
			minCount = count
			targetPorts = []int{port}
		} else if count == minCount {
			targetPorts = append(targetPorts, port)
		}
	}

	if len(targetPorts) == 0 {
		return 0
	}
	return targetPorts[rand.Intn(len(targetPorts))]
}

// GetRequestCounts returns the live cross-gateway running request count for each
// pod, keyed by pod key (namespace/name), via GetPodsRunningRequests (one Redis round
// trip for the whole list) rather than GetMetricValueByPod(RealtimeNumRequestsRunning),
// which is a periodically synced cache that, between scrape ticks, only reflects this
// gateway's local view.
func GetRequestCounts(cache cache.Cache, readyPods []*v1.Pod) map[string]int {
	return RequestCounts(nil, cache, readyPods)
}

// RequestCounts is GetRequestCounts for a read inside routing decision ctx: during the
// decision, pods an earlier reader already read are answered from its counts instead of
// Redis (see types.RoutingContext.RunningRequests). A nil ctx reads live, like
// GetRequestCounts.
func RequestCounts(ctx *types.RoutingContext, cache cache.Cache, readyPods []*v1.Pod) map[string]int {
	counts, err := ctx.RunningRequests(readyPods, cache.GetPodsRunningRequests)
	podRequestCount := make(map[string]int, len(readyPods))
	for _, pod := range readyPods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		if err == nil && counts != nil {
			podRequestCount[podKey] = int(counts[podKey])
		} else {
			podRequestCount[podKey] = 0
		}
	}

	return podRequestCount
}

// ErrNoRunningRequestsEstimate is returned by a candidate estimate that needs running counts
// when the cache cannot answer them from memory.
var ErrNoRunningRequestsEstimate = errors.New("cache has no in-memory running-request estimate")

// ApproxRunningRequests returns the running count of each pod read from process memory only
// (see cache.ApproxRunningRequestsProvider), keyed by utils.GeneratePodKey. ok is false when c
// does not provide it. Unlike RequestCounts, it never touches the routing decision's shared
// counts: an estimate must not be served to a later reader that asked for the exact value.
func ApproxRunningRequests(c cache.Cache, pods []*v1.Pod) (map[string]int64, bool) {
	provider, ok := c.(cache.ApproxRunningRequestsProvider)
	if !ok {
		return nil, false
	}
	return provider.GetPodsRunningRequestsApprox(pods), true
}

// PodsByKey maps each pod's key (namespace/name) to the pod, so a key picked from a
// per-request map resolves to the pod it was computed for even when two candidates
// share a name.
func PodsByKey(pods []*v1.Pod) map[string]*v1.Pod {
	byKey := make(map[string]*v1.Pod, len(pods))
	for _, pod := range pods {
		byKey[utils.GeneratePodKey(pod.Namespace, pod.Name)] = pod
	}
	return byKey
}

// IsMultiPortPods reports whether any of pods is a data-parallel pod serving several ports.
func IsMultiPortPods(pods []*v1.Pod) bool {
	for _, pod := range pods {
		if utils.IsDataParallelPod(pod) {
			return true
		}
	}

	return false
}
