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
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type Algorithms string

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	Model   string
	Message string
	// Additional fields can be added here to expand the routing context.
}

// Router defines the interface for routing logic to select target pods.
type Router interface {
	// Route returns the target pod
	Route(ctx context.Context, pods map[string]*v1.Pod, routingCtx *RoutingContext) (string, error)
}

// Validate validates if user provided routing routers is supported by gateway
func Validate(algorithms Algorithms) bool {
	_, ok := routerRegistry[algorithms]
	return ok
}

// Select the user provided routing routers is supported by gateway
func Select(algorithms Algorithms) routerFunc {
	if !Validate(algorithms) {
		algorithms = RouterRandom
	}
	return routerRegistry[algorithms]
}

func Register(algorithms Algorithms, router routerFunc) {
	if router == nil {
		return
	}

	routerRegistry[algorithms] = router
	klog.Infof("Registered router for %s", algorithms)
}

func RegisterDelayed(algorithms Algorithms, router routerDelayedFunc) {
	routerDelayedRegistry[algorithms] = router
}

func RegisterDelayedConstructor(algorithms Algorithms, router routerConstructor) {
	routerDelayedRegistry[algorithms] = func() routerFunc {
		router, err := router()
		if err != nil {
			klog.Errorf("Failed to construct router for %s: %v", algorithms, err)
			return nil
		}
		return func(_ *RoutingContext) (Router, error) {
			return router, nil
		}
	}
}

func Init() {
	for key, delayed := range routerDelayedRegistry {
		Register(key, delayed())
	}
}

var routerRegistry = map[Algorithms]routerFunc{}
var routerDelayedRegistry = map[Algorithms]routerDelayedFunc{}

// routerConstructor is used to construct router instance.
type routerConstructor func() (Router, error)

// routerFunc is used to get router instance.
type routerFunc func(*RoutingContext) (Router, error)

// routerDelayedFunc is used to get router instance in delayed way.
type routerDelayedFunc func() routerFunc
