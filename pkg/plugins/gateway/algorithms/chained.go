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

package routingalgorithms

import (
	"fmt"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const RouterChained types.RoutingAlgorithm = "chained"

func init() {
	RegisterProvider(RouterChained, ChainedRouterProviderFunc)
}

var ChainedRouterProviderFunc = func(ctx *types.RoutingContext) (types.Router, error) {
	return NewChainedRouter(ctx.Algorithms)
}

type chainedRouter struct {
	algorithms []types.RoutingAlgorithm
	// routerManager is the router manager used to select routers
	routerManager *RouterManager
}

func NewChainedRouter(algorithms []types.RoutingAlgorithm) (types.Router, error) {
	return &chainedRouter{
		algorithms:    algorithms,
		routerManager: defaultRM,
	}, nil
}

func (r *chainedRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	if len(r.algorithms) == 0 {
		klog.Warningf("No algorithms configured for chained router, using random selection")
		return r.selectRandomFromCandidates(ctx, readyPodList.All())
	}

	// Start with all ready pods as candidates
	candidatePods := readyPodList.All()
	klog.V(4).InfoS("chained_router_start", "request_id", ctx.RequestID, "algorithms", r.algorithms, "initial_pod_count", len(candidatePods))

	for _, algorithm := range r.algorithms {
		if len(candidatePods) == 0 {
			return "", fmt.Errorf("no candidate pods remaining after algorithm %s", algorithm)
		}

		if len(candidatePods) == 1 {
			// If only one candidate left, no need to continue with other algorithms
			klog.V(4).InfoS("chained_router_single_candidate", "request_id", ctx.RequestID, "algorithm", algorithm, "pod", candidatePods[0].Name)
			ctx.SetTargetPod(candidatePods[0])
			return ctx.TargetAddress(), nil
		}

		// Apply the current algorithm to narrow down candidates
		newCandidates, err := r.applyAlgorithm(ctx, algorithm, candidatePods)
		if err != nil {
			klog.Warningf("Failed to apply algorithm %s: %v, skipping to next algorithm", algorithm, err)
			continue
		}

		if len(newCandidates) > 0 {
			// Update candidate list for next algorithm
			candidatePods = newCandidates
			klog.V(4).InfoS("chained_router_filter", "request_id", ctx.RequestID, "algorithm", algorithm, "filtered_pod_count", len(candidatePods), "candidates", r.getPodNames(candidatePods))
		}
	}

	if len(candidatePods) == 0 {
		return "", fmt.Errorf("no candidate pods remaining after applying all algorithms")
	}

	if len(candidatePods) == 1 {
		ctx.SetTargetPod(candidatePods[0])
		return ctx.TargetAddress(), nil
	}

	klog.V(4).InfoS("chained_router_multiple_candidates", "request_id", ctx.RequestID, "remaining_pod_count", len(candidatePods), "using_random_selection")
	return r.selectRandomFromCandidates(ctx, candidatePods)
}

func (r *chainedRouter) applyAlgorithm(ctx *types.RoutingContext, algorithm types.RoutingAlgorithm, candidatePods []*v1.Pod) ([]*v1.Pod, error) {
	// Create a new routing context for this algorithm
	user := ""
	if ctx.User != nil {
		user = *ctx.User
	}

	routingCtx := types.NewRoutingContext(ctx.Context, algorithm, ctx.Model, ctx.Message, ctx.RequestID, user)
	defer routingCtx.Delete()

	routingCtx.ReqHeaders = ctx.ReqHeaders
	routingCtx.ReqBody = ctx.ReqBody
	routingCtx.ReqPath = ctx.ReqPath

	routingCtx.CandidatePods = candidatePods
	router, err := r.routerManager.Select(&types.RoutingContext{Algorithm: algorithm})
	if err != nil {
		return nil, err
	}

	podList := &utils.PodArray{Pods: candidatePods}
	_, err = router.Route(routingCtx, podList)
	if err != nil {
		return nil, err
	}

	// If the algorithm set CandidatePods, it means it found multiple candidates
	// and wants to pass them to the next algorithm in the chain
	if len(routingCtx.CandidatePods) > 0 {
		return routingCtx.CandidatePods, nil
	}

	selectedPod := routingCtx.TargetPod()
	if selectedPod == nil {
		return nil, fmt.Errorf("router returned nil pod")
	}

	return []*v1.Pod{selectedPod}, nil
}

func (r *chainedRouter) selectRandomFromCandidates(ctx *types.RoutingContext, candidatePods []*v1.Pod) (string, error) {
	podList := &utils.PodArray{Pods: candidatePods}
	router, err := r.routerManager.Select(&types.RoutingContext{Algorithm: RouterRandom})
	if err != nil {
		return "", err
	}
	return router.Route(ctx, podList)
}

func (r *chainedRouter) getPodNames(pods []*v1.Pod) []string {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	return names
}
