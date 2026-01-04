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

// Route request based of least active request among input ready pods
func (r *leastRequestRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	// Use distributed DP-level API server routing when pods have multiple ports
	ports := readyPodList.ListPortsForPod()
	if len(ports) > 1 {
		return r.apiServerRoute(ctx, readyPods, ports)
	}
	// Use default Pod-level routing
	targetPod := selectTargetPodWithLeastRequestCount(r.cache, readyPods)

	// Use fallback if no valid metrics
	if targetPod == nil {
		var err error
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPods, rand.Intn)
		if err != nil {
			return "", err
		}
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *leastRequestRouter) apiServerRoute(ctx *types.RoutingContext, readyPods []*v1.Pod, ports map[string][]int) (string, error) {
	targetPod, targetPort := selectTargetPodAndPortWithLeastRequestCount(r.cache, readyPods, ports)
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
	var targetPod *v1.Pod
	targetPods := []string{}

	minCount := math.MaxInt32
	podRequestCount := getRequestCounts(cache, readyPods)
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

func selectTargetPodAndPortWithLeastRequestCount(cache cache.Cache, readyPods []*v1.Pod, ports map[string][]int) (*v1.Pod, int) {
	minCount := math.MaxInt32

	targetApiServers := []string{}
	podRequestCount := getRequestCountsWithPort(cache, readyPods, ports)
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

	targetPod, _ := utils.FilterPodByName(podName, readyPods)
	targetPort, err := strconv.Atoi(portStr)
	if err != nil {
		klog.ErrorS(err, "Failed to parse port", "port", portStr)
		return targetPod, 0
	}

	return targetPod, targetPort
}

// getRequestCounts returns running request count for each pod tracked by gateway.
// Note: Currently, gateway instance tracks active running request counts for each pod locally,
// if multiple gateway instances are active then state is not shared across them.
// It is advised to run on leader gateway instance.
// TODO: Support stateful information sync across gateway instances: https://github.com/vllm-project/aibrix/issues/761
func getRequestCounts(cache cache.Cache, readyPods []*v1.Pod) map[string]int {
	podRequestCount := make(map[string]int, len(readyPods))
	for _, pod := range readyPods {
		runningReq, err := cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			runningReq = &metrics.SimpleMetricValue{Value: 0}
		}
		podRequestCount[pod.Name] = int(runningReq.GetSimpleValue())
	}

	return podRequestCount
}

// getRequestCountsWithPort returns running request count for each pod with port tracked by gateway.
// Note: Currently, gateway instance tracks active running request counts for each pod locally,
// if multiple gateway instances are active then state is not shared across them.
// It is advised to run on leader gateway instance.
func getRequestCountsWithPort(cache cache.Cache, readyPods []*v1.Pod, ports map[string][]int) map[string]int {
	podRequestCount := make(map[string]int)
	for _, pod := range readyPods {
		podPorts, exists := ports[pod.Name]
		if !exists || len(podPorts) == 0 {
			continue
		}

		for _, port := range podPorts {
			runningReq, err := cache.GetMetricValueByPodWithPort(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning, port)
			if err != nil {
				runningReq = &metrics.SimpleMetricValue{Value: 0}
			}
			keyName := fmt.Sprintf("%s/%d", pod.Name, port)
			podRequestCount[keyName] = int(runningReq.GetSimpleValue())
		}
	}

	return podRequestCount
}
