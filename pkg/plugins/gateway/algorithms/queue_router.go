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
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"k8s.io/klog/v2"
)

// queueRouter implements a request routing algorithm that uses a queue-based approach.
// It accepts a backend stateless Router and a RouterQueue as inputs.
// It manages incoming requests by:
//
//  1. Enqueuing requests instead of doing routing in Route() call.
//  2. Peeking queue for finding next routing candidate.
//  3. Passing the candidate to backend Router.
//  4. Dequeuing requests when the Router successfully routes, suggesting pods become available (by calling.
//  5. Alternatively, Route() will be called with nil request to trigger routing if pods become available
//     before new request's arrival.
//
// # Noting that for atomicity concern, backend Router and RouterQueue may coordinating with an underlying router for actual pod selection in both step 2 and 3.
//
// It is the backend RouterQueue's responsibility to maintaining requests' dispatching order,
// specificially, FIFO or reordering as necessary (as in the SLOQueue).
//
// Backend Router, in the other hand, ensures fair request distribution across pods.
//
// queueRouter is provisioned by cache by model and instances can be get from cache using the model identifier.
type queueRouter struct {
	router         types.Router
	queue          types.RouterQueue[*types.RoutingContext]
	cache          cache.Cache
	chRouteTrigger chan types.PodList
	// maxWaitWhenQueued is the max time to block when the queue has items but no new request arrived.
	maxWaitWhenQueued time.Duration
}

func NewQueueRouter(backend types.Router, queue types.RouterQueue[*types.RoutingContext]) (types.QueueRouter, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	router := &queueRouter{
		router:            backend,
		queue:             queue,
		cache:             c,
		chRouteTrigger:    make(chan types.PodList, 1), // One buffer is needed for thread safety.
		maxWaitWhenQueued: 10 * time.Millisecond,
	}

	go router.serve()

	return router, nil
}

func (r *queueRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	if pods.Len() == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}

	if ctx == nil {
		r.tryRoute(pods) // Simply trigger a possible dequeue
		return "", nil   // Result is irrelevant
	}

	// Ensure the request being counted even the request might not be counted.
	// Noted, AddRequestCount should implement the idempotence for trace count.
	r.cache.AddRequestCount(ctx, ctx.RequestID, ctx.Model)
	if err := r.queue.Enqueue(ctx, time.Now()); err != nil {
		return "", err
	}

	r.tryRoute(pods) // Simply trigger a possible dequeue

	targetPod := ctx.TargetPod() // Will wait
	if targetPod != nil {
		klog.V(4).Infof("targetPod for routing: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	}

	return ctx.TargetAddress(), ctx.GetError()
}

func (r *queueRouter) Len() int {
	return r.queue.Len()
}

func (r *queueRouter) tryRoute(pods types.PodList) {
	select {
	case r.chRouteTrigger <- pods:
	default:
		// ignore
	}
}

func (r *queueRouter) serve() {
	var lastPods types.PodList
	for {
		var pods types.PodList
		if r.queue.Len() > 0 && lastPods != nil {
			select {
			case pods = <-r.chRouteTrigger:
				lastPods = pods
			case <-time.After(r.maxWaitWhenQueued):
				pods = lastPods
			}
		} else {
			pods = <-r.chRouteTrigger
			lastPods = pods
		}

		for {
			ctx, err := r.queue.Peek(time.Now(), pods)
			if err != nil && err != types.ErrQueueEmpty {
				klog.Errorf("error on peek request queue: %v", err)
				break
			} else if ctx == nil {
				// Nothing to route, this happens if the queue is not empty, but no pod is available to be routed.
				// A pod can be unavailable if:
				// 1. The pod is not ready.
				// 2. The pod has reached its max capacity.
				break
			}

			_, err = r.router.Route(ctx, pods)
			if err != nil {
				// Necessary if Router has not set the error. No harm to set twice.
				ctx.SetError(err)
			} else {
				// Add request count here to make real-time metrics update and read serial.
				// Noted, AddRequestCount should implement the idempotence.
				r.cache.AddRequestCount(ctx, ctx.RequestID, ctx.Model)
			}
			// req.SetTargetPod() should have called in Route()
			dequeued, err := r.queue.Dequeue(time.Now())
			if err != nil {
				klog.Errorf("error on dequeue request queue: %v", err)
			} else if dequeued != ctx {
				klog.Error("unexpected request dequeued")
			}
		}
	}
}
