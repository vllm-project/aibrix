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

package types

import v1 "k8s.io/api/core/v1"

// Router defines the interface for routing logic to select target pods.
type Router interface {
	// Route selects a target pod from the provided list of pods.
	// The input pods is guaranteed to be non-empty and contain only routable pods.
	Route(ctx *RoutingContext, readyPodList PodList) (string, error)
}

// SingleCandidateBypasser lets a router declare whether Gateway may use its
// zero-network single-candidate fast path. Routers that do not implement this
// interface retain the existing bypass behavior.
type SingleCandidateBypasser interface {
	BypassSingleCandidate() bool
}

// CandidateSeeder is implemented by routers whose choice depends on particular pods, such as
// the pods holding a request's prefix. When the gateway routes over a bounded candidate set
// (AIBRIX_ROUTING_CANDIDATE_K), it adds the pods SeedCandidates returns to that set, so a
// candidate stage never hides them from the router.
type CandidateSeeder interface {
	// SeedCandidates returns at most max pods from readyPods, best first. It must not read
	// per-pod state from Redis, because the candidate stage exists to avoid reads over every
	// ready pod.
	SeedCandidates(ctx *RoutingContext, readyPods PodList, max int) []*v1.Pod
}

// QueueRouter defines the interface for routers that contains built-in queue and
// offers queue status query.
type QueueRouter interface {
	Router

	Len() int
}

// FallbackRouter enables router chaining by set a fallback router.
type FallbackRouter interface {
	Router

	// SetFallback sets the fallback router
	SetFallback(RoutingAlgorithm, RouterProviderFunc)
}

// RouterProvider provides a stateful way to get a router, allowing a struct to provide the router by strategy and model.
type RouterProvider interface {
	// GetRouter returns the router
	GetRouter(ctx *RoutingContext) (Router, error)
}

// RouterProviderFunc provides a stateless way to get a router
type RouterProviderFunc func(*RoutingContext) (Router, error)

// RouterProviderRegistrationFunc provides a way to register RouterProviderFunc
type RouterProviderRegistrationFunc func() RouterProviderFunc

// RouterConstructor defines a constructor for a router.
type RouterConstructor func() (Router, error)
