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

// Polarity returns the polarity for least-request strategy
func (r *leastRequestRouter) Polarity() types.Polarity {
	return types.PolarityLeast // The fewer requests, the better
}

// ScoreAll computes the raw score (current active requests) for all ready pods in a single batch operation.
// This allows the multi-strategy aggregator to normalize and weight the active load metric alongside other strategies.
func (r *leastRequestRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		runningReq, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
		if err != nil {
			// If a pod has no metrics yet, we assume it has 0 requests to absorb cold-start traffic.
			scores[i] = 0.0
			scored[i] = true
		} else {
			scores[i] = runningReq.GetSimpleValue()
			scored[i] = true
		}
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

	scores, scored, err := r.ScoreAll(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	var targetPods []string
	minCount := math.MaxFloat64

	for i, pod := range readyPods {
		if !scored[i] {
			continue
		}

		if scores[i] < minCount {
			minCount = scores[i]
			targetPods = []string{pod.Name}
		} else if scores[i] == minCount {
			targetPods = append(targetPods, pod.Name)
		}
	}

	if len(targetPods) > 0 {
		targetPod, _ = utils.FilterPodByName(targetPods[rand.Intn(len(targetPods))], readyPods)
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		targetPod, err = SelectRandomPodAsFallback(ctx, readyPods, rand.Intn)
		if err != nil {
			return "", err
		}
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

// getRequestCountsWithPort returns running request count for each pod with port tracked by gateway
func getRequestCountsWithPort(cache cache.Cache, readyPods []*v1.Pod, portsMap map[string][]int) map[string]int {
	podRequestCount := make(map[string]int)
	for _, pod := range readyPods {
		podPorts, exists := portsMap[pod.Name]
		if !exists || len(podPorts) == 0 {
			continue
		}

		for _, port := range podPorts {
			var metricName string
			var keyName string

			if len(podPorts) == 1 {
				metricName = metrics.RealtimeNumRequestsRunning
				keyName = pod.Name
			} else {
				metricName = metrics.RealtimeNumRequestsRunning + "/" + strconv.Itoa(port)
				keyName = pod.Name + "/" + strconv.Itoa(port)
			}

			var count int
			if val, err := cache.GetMetricValueByPod(pod.Name, pod.Namespace, metricName); err == nil && val != nil {
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
