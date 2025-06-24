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
	"github.com/vllm-project/aibrix/pkg/types"
)

const DefaultFallbackAlgorithm types.RoutingAlgorithm = RouterRandom

var DefaultFallbackRouter types.RouterProviderFunc = RandomRouterProviderFunc

type FallbackRouter struct {
	fallbackAlgorithm types.RoutingAlgorithm
	fallbackProvider  types.RouterProviderFunc
}

func (r *FallbackRouter) SetFallback(fallback types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	r.fallbackAlgorithm = fallback
	r.fallbackProvider = provider
}

func (r *FallbackRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	if r.fallbackProvider == nil {
		r.fallbackAlgorithm = DefaultFallbackAlgorithm
		r.fallbackProvider = DefaultFallbackRouter
	}
	router, err := r.fallbackProvider(ctx)
	if err != nil {
		return "", err
	}
	ctx.Algorithm = r.fallbackAlgorithm
	return router.Route(ctx, pods)
}
