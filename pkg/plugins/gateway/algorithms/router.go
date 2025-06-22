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
	"sync"

	"github.com/vllm-project/aibrix/pkg/types"
	"k8s.io/klog/v2"
)

var routerMu sync.RWMutex

const (
	RouterNotSet = ""
)

var routerFactory = map[types.RoutingAlgorithm]types.RouterProviderFunc{}
var routerConstructor = map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc{}

// Validate validates if user provided routing routers is supported by gateway
func Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	if _, ok := routerFactory[types.RoutingAlgorithm(algorithms)]; ok {
		return types.RoutingAlgorithm(algorithms), ok
	} else {
		return RouterNotSet, false
	}
}

// Select the user provided router provider supported by gateway, no error reported and fallback to random router
// Call Validate before this function to ensure expected behavior.
func Select(algorithms types.RoutingAlgorithm) types.RouterProviderFunc {
	if provider, ok := routerFactory[algorithms]; ok {
		return provider
	} else {
		klog.Warningf("Unsupported router strategy: %s, use %s instead.", algorithms, RouterRandom)
		return routerFactory[RouterRandom]
	}
}

func Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	routerConstructor[algorithm] = func() types.RouterProviderFunc {
		router, err := constructor()
		if err != nil {
			klog.Errorf("Failed to construct router for %s: %v", algorithm, err)
			return nil
		}
		return func(_ *types.RoutingContext) (types.Router, error) {
			return router, nil
		}
	}
}

func Init() {
	routerMu.Lock()
	defer routerMu.Unlock()
	for algorithm, constructor := range routerConstructor {
		routerFactory[algorithm] = constructor()
		klog.Infof("Registered router for %s", algorithm)
	}
}
