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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/queue"
	"github.com/vllm-project/aibrix/pkg/types"
)

const (
	// Default routing algorithm for slo-family algorithms, setting to RouterSLOLeastLoadPulling.
	RouterSLO types.RoutingAlgorithm = "slo"

	// SLO-aware routing algorithm that using SLOQueue and packLoadRouter as the backend.
	RouterSLOPackLoad types.RoutingAlgorithm = "slo-pack-load"

	// SLO-aware routing algorithm that using SLOQueue and leastLoadRouter (push mode) as the backend.
	RouterSLOLeastLoad types.RoutingAlgorithm = "slo-least-load"

	// SLO-aware routing algorithm that using SLOQueue and leastLoadRouter (pull mode) as the backend.
	RouterSLOLeastLoadPulling types.RoutingAlgorithm = "slo-least-load-pulling"
)

var (
	routerSLOProvider types.RouterProviderFunc = func(ctx *types.RoutingContext) (types.Router, error) {
		c, err := cache.Get()
		if err != nil {
			return nil, err
		}

		return c.GetRouter(ctx)
	}
)

func init() {
	RegisterProvider(RouterSLO, routerSLOProvider)
	RegisterProvider(RouterSLOPackLoad, routerSLOProvider)
	RegisterProvider(RouterSLOLeastLoad, routerSLOProvider)
	RegisterProvider(RouterSLOLeastLoadPulling, routerSLOProvider)
}

// SLORouter is a router that add FallbackRouter mechanism to the queue.
type SLORouter struct {
	FallbackRouter
	*queue.SLOQueue
}

func (r *SLORouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	// Ctx is not routed if no profiles is found during Peek.
	if !ctx.HasRouted() && r.SLOQueue.LastError() == nil {
		return r.FallbackRouter.Route(ctx, pods)
	}
	return r.SLOQueue.Route(ctx, pods)
}

func NewSLORouter(modelName string) (types.QueueRouter, error) {
	loadProvider, err := cache.NewPendingLoadProvider()
	if err != nil {
		return nil, err
	}

	rm := NewRouterManager()
	defaultRouter, _ := NewLeastLoadPullingRouter(loadProvider)
	defaultProvider := func(_ *types.RoutingContext) (types.Router, error) { return defaultRouter, nil }
	rm.RegisterProvider(RouterSLO, defaultProvider)
	rm.RegisterProvider(RouterSLOLeastLoadPulling, defaultProvider)
	rm.Register(RouterSLOPackLoad, func() (types.Router, error) { return NewPackLoadRouter(loadProvider) })
	rm.Register(RouterSLOLeastLoad, func() (types.Router, error) { return NewLeastLoadRouter(loadProvider) })
	rm.Init()

	sloQueue, err := queue.NewSLOQueue(rm.Select, modelName)
	if err != nil {
		return nil, err
	}
	router := &SLORouter{SLOQueue: sloQueue}
	if err := SetFallback(router, RouterLeastRequest); err != nil {
		return nil, err
	}
	return NewQueueRouter(router, sloQueue)
}
