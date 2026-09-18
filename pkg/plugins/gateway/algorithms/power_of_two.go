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
	"math/rand"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const RouterPowerOfTwo types.RoutingAlgorithm = "power-of-two"

func init() {
	Register(RouterPowerOfTwo, NewPowerOfTwoRouter)
}

// PowerOfTwoRouter implements the power of two choices algorithm: it samples two distinct
// ready pods at random and routes to the one with fewer running requests.
//
// The router keeps no counters of its own. The running-request count of a pod is the one
// the cache maintains for every routed request (see Cache.GetPodsRunningRequests): the
// gateway increments it when a request is dispatched and decrements it when the request
// completes, whichever router chose the pod. When Redis is configured the count is
// aggregated across gateway replicas; otherwise it is this gateway's own count.
type PowerOfTwoRouter struct {
	cache cache.Cache
}

// NewPowerOfTwoRouter creates a Power of Two router backed by the global cache.
func NewPowerOfTwoRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return NewPowerOfTwoRouterWithCache(c), nil
}

// NewPowerOfTwoRouterWithCache creates a Power of Two router that reads load from the given cache.
func NewPowerOfTwoRouterWithCache(c cache.Cache) *PowerOfTwoRouter {
	return &PowerOfTwoRouter{cache: c}
}

// Route implements [types.Router] using power of two choices algorithm.
// It randomly selects two pods and routes to the one with fewer running requests. For a
// data-parallel pod that serves several ports, the request goes to the port with the fewest
// running requests.
func (p *PowerOfTwoRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	readyPods := readyPodList.All()
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available")
	}

	target := readyPods[0]
	if len(readyPods) > 1 {
		// Two distinct pods, uniformly: draw the second from the remaining n-1 slots.
		idx1 := rand.Intn(len(readyPods))
		idx2 := rand.Intn(len(readyPods) - 1)
		if idx2 >= idx1 {
			idx2++
		}
		pod1, pod2 := readyPods[idx1], readyPods[idx2]

		count1, count2 := p.getRequestCounts(pod1, pod2)
		klog.V(4).InfoS("power_of_two_selection",
			"request_id", ctx.RequestID,
			"candidate1", pod1.Name,
			"count1", count1,
			"candidate2", pod2.Name,
			"count2", count2)

		// Choose the one with fewer requests
		if count1 <= count2 {
			target = pod1
		} else {
			target = pod2
		}
	}

	targetPort := selectTargetPortForPodWithLeastRequestCount(p.cache, target, readyPodList.ListPortsForPod())
	klog.V(4).InfoS("power_of_two_route",
		"request_id", ctx.RequestID,
		"target_pod", target.Name,
		"target_port", targetPort)

	ctx.SetTargetPod(target)
	if targetPort != 0 {
		ctx.SetTargetPort(targetPort)
	}
	return ctx.TargetAddress(), nil
}

// getRequestCounts returns the live running-request count of both pods from a single cache
// read. A pod without a count, or a failed read, counts as 0: routing falls back to a random
// choice between the two pods rather than failing the request.
func (p *PowerOfTwoRouter) getRequestCounts(pod1, pod2 *v1.Pod) (count1, count2 int64) {
	counts, err := p.cache.GetPodsRunningRequests([]*v1.Pod{pod1, pod2})
	if err != nil {
		klog.V(4).ErrorS(err, "failed to get running requests, treating candidates as idle",
			"candidate1", pod1.Name, "candidate2", pod2.Name)
		return 0, 0
	}
	return counts[utils.GeneratePodKey(pod1.Namespace, pod1.Name)], counts[utils.GeneratePodKey(pod2.Namespace, pod2.Name)]
}

// SubscribedMetrics implements [types.Router].
func (p *PowerOfTwoRouter) SubscribedMetrics() []string {
	// The per-port count of a data-parallel pod is read from the realtime running-requests metric.
	return []string{metrics.RealtimeNumRequestsRunning}
}

var _ types.Router = (*PowerOfTwoRouter)(nil)
