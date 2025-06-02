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

// SLORouter is a router that add FallbackRouter mechanism to the queue.
type SLORouter struct {
	FallbackRouter
	*queue.SLOQueue
}

func (r *SLORouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	// Ctx is not routed if no profiles is found during Peek.
	if !ctx.HasRouted() && r.LastError() == nil {
		return r.FallbackRouter.Route(ctx, pods)
	}
	return r.SLOQueue.Route(ctx, pods)
}

func NewPackSLORouter(modelName string) (types.Router, error) {
	loadProvider, err := cache.NewPendingLoadProvider()
	if err != nil {
		return nil, err
	}

	loadRouter, _ := NewPackLoadRouter(loadProvider)
	sloQueue, err := queue.NewSLOQueue(loadRouter, modelName)
	if err != nil {
		return nil, err
	}
	router := &SLORouter{SLOQueue: sloQueue}
	if err := SetFallback(router, RouterLeastRequest); err != nil {
		return nil, err
	}
	return NewQueueRouter(router, sloQueue)
}

func NewLeastLoadSLORouter(modelName string) (types.Router, error) {
	loadProvider, err := cache.NewPendingLoadProvider()
	if err != nil {
		return nil, err
	}

	loadRouter, _ := NewLeastLoadPullingRouter(loadProvider)
	sloQueue, err := queue.NewSLOQueue(loadRouter, modelName)
	if err != nil {
		return nil, err
	}
	router := &SLORouter{SLOQueue: sloQueue}
	if err := SetFallback(router, RouterLeastRequest); err != nil {
		return nil, err
	}
	return NewQueueRouter(router, sloQueue)
}
