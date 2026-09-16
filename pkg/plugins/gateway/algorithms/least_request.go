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
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"

	"k8s.io/klog/v2"
)

const RouterLeastRequest types.RoutingAlgorithm = "least-request"

func init() {
	Register(RouterLeastRequest, NewLeastRequestRouter)
}

type leastRequestRouter struct {
	cache cache.Cache
}

func NewLeastRequestRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	return &leastRequestRouter{
		cache: c,
	}, nil
}

// NewLeastRequestRouterWithCache constructs least-request with an explicit cache.
func NewLeastRequestRouterWithCache(c cache.Cache) (types.Router, error) {
	return &leastRequestRouter{cache: c}, nil
}

// Polarity returns the polarity for least-request strategy
func (r *leastRequestRouter) Polarity() types.Polarity {
	return types.PolarityLeast // The fewer requests, the better
}

// ScoreAll computes the raw score (current active requests) for all ready pods in a single batch operation.
// This allows the multi-strategy aggregator to normalize and weight the active load metric alongside other strategies.
//
// Uses GetPodsRunningRequests (the live cross-gateway count, one Redis round trip for
// every candidate), not GetMetricValueByPod(RealtimeNumRequestsRunning): that metric
// slot is a periodically synced cache and, between scrape ticks, only reflects this
// gateway's local view -- not safe for a routing decision.
func (r *leastRequestRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	counts, err := r.cache.GetPodsRunningRequests(pods)
	for i, pod := range pods {
		// If a pod has no count yet (missing from counts, or the batch call itself
		// failed), we assume it has 0 requests to absorb cold-start traffic.
		if err == nil {
			if podKey := utils.GeneratePodKey(pod.Namespace, pod.Name); counts != nil {
				scores[i] = float64(counts[podKey])
			}
		}
		scored[i] = true
	}
	return scores, scored, nil
}

// Route request based of least active request among input ready pods
func (r *leastRequestRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	// Use distributed DP-level API server routing when pods have multiple ports
	if isMultiPortPods(readyPods) {
		return r.apiServerRoute(ctx, readyPods, readyPodList.ListPortsForPod())
	}

	targetPod, err := RouteByScore(ctx, readyPodList, r)
	if err != nil {
		return "", err
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *leastRequestRouter) apiServerRoute(ctx *types.RoutingContext, readyPods []*v1.Pod, portsMap map[string][]int) (string, error) {
	targetPod, targetPort := selectTargetPodAndPortWithLeastRequestCount(r.cache, readyPods, portsMap)
	if targetPod == nil {
		return "", fmt.Errorf("no target pod selected")
	}

	if targetPort == 0 {
		return "", fmt.Errorf("target pod does not have a port")
	}
	ctx.SetTargetPod(targetPod)
	ctx.SetTargetPort(targetPort)
	return ctx.TargetAddress(), nil
}

func (r *leastRequestRouter) SubscribedMetrics() []string {
	return []string{
		metrics.RealtimeNumRequestsRunning,
	}
}

func selectTargetPodWithLeastRequestCount(cache cache.Cache, readyPods []*v1.Pod) *v1.Pod {
	return selectTargetPodWithLeastRequestCountFromCounts(getRequestCounts(cache, readyPods), readyPods)
}

func selectTargetPodWithLeastRequestCountFromCounts(podRequestCount map[string]int, readyPods []*v1.Pod) *v1.Pod {
	var targetPod *v1.Pod
	targetPods := []string{}

	minCount := math.MaxInt32
	klog.V(4).InfoS("selectTargetPodWithLeastRequestCount", "podRequestCount", podRequestCount)
	for podname, totalReq := range podRequestCount {
		if totalReq < minCount {
			minCount = totalReq
			targetPods = []string{podname}
		} else if totalReq == minCount {
			targetPods = append(targetPods, podname)
		}
	}
	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
	}
	return targetPod
}

func selectTargetPodAndPortWithLeastRequestCount(cache cache.Cache, readyPods []*v1.Pod, portsMap map[string][]int) (*v1.Pod, int) {
	readyPodsMap := make(map[string]*v1.Pod, len(readyPods))
	for _, pod := range readyPods {
		readyPodsMap[pod.Name] = pod
	}

	minCount := math.MaxInt32

	var targetApiServers []string
	podRequestCount := getRequestCountsWithPort(cache, readyPods, portsMap)
	if len(podRequestCount) == 0 {
		return nil, 0
	}

	klog.V(4).InfoS("selectTargetPodAndPortWithLeastRequestCount", "podRequestCount", podRequestCount)
	for servername, totalReq := range podRequestCount {
		if totalReq < minCount {
			minCount = totalReq
			targetApiServers = []string{servername}
		} else if totalReq == minCount {
			targetApiServers = append(targetApiServers, servername)
		}
	}

	if len(targetApiServers) == 0 {
		return nil, 0
	}

	// Random selection among candidates
	selectedServer := targetApiServers[rand.Intn(len(targetApiServers))]
	parts := strings.Split(selectedServer, "/")
	if len(parts) != 2 {
		klog.ErrorS(nil, "Invalid server name format", "serverName", selectedServer)
		return nil, 0
	}

	podName := parts[0]
	portStr := parts[1]

	targetPod, found := readyPodsMap[podName]
	if !found {
		klog.ErrorS(nil, "Selected pod not found in ready pods list", "podName", podName)
		return nil, 0
	}

	targetPort, err := strconv.Atoi(portStr)
	if err != nil {
		klog.ErrorS(err, "Failed to parse port", "port", portStr)
		return targetPod, 0
	}

	return targetPod, targetPort
}

func selectTargetPortForPodWithLeastRequestCount(cache cache.Cache, pod *v1.Pod, portsMap map[string][]int) int {
	if pod == nil {
		return 0
	}
	podPorts := portsMap[pod.Name]
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

// getRequestCounts returns the live cross-gateway running request count for each
// pod, via GetPodsRunningRequests (one Redis round trip for the whole list) rather
// than GetMetricValueByPod(RealtimeNumRequestsRunning), which is a periodically
// synced cache that, between scrape ticks, only reflects this gateway's local view.
func getRequestCounts(cache cache.Cache, readyPods []*v1.Pod) map[string]int {
	counts, err := cache.GetPodsRunningRequests(readyPods)
	podRequestCount := make(map[string]int, len(readyPods))
	for _, pod := range readyPods {
		if err == nil && counts != nil {
			podRequestCount[pod.Name] = int(counts[utils.GeneratePodKey(pod.Namespace, pod.Name)])
		} else {
			podRequestCount[pod.Name] = 0
		}
	}

	return podRequestCount
}

// getRequestCountsWithPort returns running request count for each pod with port tracked by gateway.
// Single-port pods use the live cross-gateway count (GetPodsRunningRequests, see
// getRequestCounts); the running-requests counter is pod-level only, with no per-port
// dimension, so a genuinely multi-port pod still reads its per-port metric slot via
// GetMetricValueByPod as before.
func getRequestCountsWithPort(c cache.Cache, readyPods []*v1.Pod, portsMap map[string][]int) map[string]int {
	singlePort := make([]*v1.Pod, 0, len(readyPods))
	for _, pod := range readyPods {
		if podPorts, exists := portsMap[pod.Name]; exists && len(podPorts) == 1 {
			singlePort = append(singlePort, pod)
		}
	}
	liveCounts, err := c.GetPodsRunningRequests(singlePort)

	podRequestCount := make(map[string]int)
	for _, pod := range readyPods {
		podPorts, exists := portsMap[pod.Name]
		if !exists || len(podPorts) == 0 {
			continue
		}

		for _, port := range podPorts {
			if len(podPorts) == 1 {
				count := 0
				if err == nil && liveCounts != nil {
					count = int(liveCounts[utils.GeneratePodKey(pod.Namespace, pod.Name)])
				}
				podRequestCount[pod.Name] = count
				continue
			}

			metricName := metrics.RealtimeNumRequestsRunning + "/" + strconv.Itoa(port)
			keyName := pod.Name + "/" + strconv.Itoa(port)
			var count int
			if val, err := c.GetMetricValueByPod(pod.Name, pod.Namespace, metricName); err == nil && val != nil {
				count = int(val.GetSimpleValue())
			}
			podRequestCount[keyName] = count
		}
	}

	return podRequestCount
}

func isMultiPortPods(pods []*v1.Pod) bool {
	for _, pod := range pods {
		if utils.IsDataParallelPod(pod) {
			return true
		}
	}

	return false
}
