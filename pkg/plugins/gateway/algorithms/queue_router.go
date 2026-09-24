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
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
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
	queue          types.RouterQueue[*types.QueueEntry]
	cache          cache.Cache
	chRouteTrigger chan types.PodList
	// pendingMu serializes sampling the queue depth and publishing it. Both the
	// request path (after Enqueue) and serve (after Dequeue) report the gauge; without
	// it a sample taken before a drain can be published after the drain's own, leaving
	// the gauge stuck at the stale depth.
	pendingMu sync.Mutex
}

func NewQueueRouter(backend types.Router, queue types.RouterQueue[*types.QueueEntry]) (types.QueueRouter, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	router := &queueRouter{
		router:         backend,
		queue:          queue,
		cache:          c,
		chRouteTrigger: make(chan types.PodList, 1), // One buffer is needed for thread safety.
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

	now := time.Now()
	// Freeze the residency metadata before the request becomes visible to the serve
	// goroutine: it can pick the entry up as soon as it is enqueued, hand it to the
	// router, and the requester can then be unblocked and recycle its context while
	// the entry is still queued. The departure metrics therefore read the entry, not
	// the context.
	entry := types.NewQueueEntry(ctx, now)
	if err := r.queue.Enqueue(entry, now); err != nil {
		return "", err
	}
	r.updateQueuePendingMetric(entry)

	r.tryRoute(pods) // Simply trigger a possible dequeue

	targetPod := ctx.TargetPod() // Will wait
	if targetPod != nil {
		klog.V(4).Infof("targetPod for routing: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	}

	// Call AddRequestCount again after routing completes so that pod stats (e.g.
	// RealtimeNormalizedPendings) are guaranteed to be updated before Route()
	// returns. The serve() goroutine calls AddRequestCount concurrently; CanAddStats
	// uses a CAS so addPodStats runs exactly once regardless of which caller wins.
	r.cache.AddRequestCount(ctx, ctx.RequestID, ctx.Model)

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
	for {
		pods := <-r.chRouteTrigger

		// Drain until there is nothing routable, or until a recovered panic left the
		// queue at a position this loop must not advance from. routeNext routes one
		// candidate and reports whether the loop may pick the next one right away.
		for r.routeNext(pods) {
		}
	}
}

// Steps of routeNext, named so a recovered panic reports the stage it hit. The recovery
// stage covers a panic raised while a recovered one is handled.
const (
	routeStepPeek     = "peek"
	routeStepRoute    = "route"
	routeStepDequeue  = "dequeue"
	routeStepRecovery = "recovery"
)

// errRouteRecovered is what the request behind a recovered panic reports. It names no
// internal detail: the panic value and its stack stay in the log.
var errRouteRecovered = errors.New("gateway routing recovered from an internal error")

// routeNext routes at most one queued request: peek, hand the entry to the backend
// router, then dequeue it. It reports whether the drain loop may pick the next candidate
// right away; false means there is nothing routable now, or that a recovered panic left
// the queue in a state this loop must not advance from.
//
// Every step runs inside this function so that a panic cannot end the serve goroutine.
// Backend routing only signals that goroutine through chRouteTrigger, so a panic raised
// while peeking, routing or dequeuing would otherwise end the gateway process and leave
// every queued request waiting for a target pod that never comes.
func (r *queueRouter) routeNext(pods types.PodList) (advance bool) {
	var (
		entry *types.QueueEntry
		err   error
	)
	step := routeStepPeek
	defer func() {
		if v := recover(); v != nil {
			advance = r.recoverCandidate(v, entry, step)
		}
	}()

	entry, err = r.queue.Peek(time.Now(), pods)
	if err != nil && err != types.ErrQueueEmpty {
		klog.Errorf("error on peek request queue: %v", err)
		return false
	} else if entry == nil {
		// Nothing to route, this happens if the queue is not empty, but no pod is available to be routed.
		// A pod can be unavailable if:
		// 1. The pod is not ready.
		// 2. The pod has reached its max capacity.
		return false
	}

	step = routeStepRoute
	ctx := entry.RoutingContext
	_, routeErr := r.router.Route(ctx, pods)
	if routeErr != nil {
		// Necessary if Router has not set the error. No harm to set twice.
		ctx.SetError(routeErr)
	} else {
		// Add request count here to make real-time metrics update and read serial.
		// Noted, AddRequestCount should implement the idempotence.
		r.cache.AddRequestCount(ctx, ctx.RequestID, ctx.Model)
	}

	step = routeStepDequeue
	// req.SetTargetPod() should have called in Route()
	dequeued, dequeueErr := r.queue.Dequeue(time.Now())
	if dequeueErr != nil {
		klog.Errorf("error on dequeue request queue: %v", dequeueErr)
	} else if dequeued != entry {
		klog.Error("unexpected request dequeued")
	} else {
		// Report the departure only once the request actually left the queue: a
		// failed Dequeue leaves it enqueued, so the next Peek would route the same
		// entry again and emitting here would count that departure twice. The
		// emission reads the entry's frozen metadata: the requester may already
		// have been unblocked, and its context recycled, by the time the router
		// above set the target pod.
		emitQueueOutcomeMetrics(entry, routeErr)
	}
	r.updateQueuePendingMetric(entry)
	return true
}

// recoverCandidate handles a panic recovered from one step of routeNext. It reports the
// panic, fails the request that was being routed so its waiter returns instead of waiting
// for a pod the queue can no longer hand it, and, when the queue is at a position the loop
// can move on from, drops the candidate that raised it. It reports whether the drain loop
// may pick the next candidate right away.
//
// Every action below calls back into the queue or the metrics pipeline, which is the code
// that raised the first panic, so all of them run under one recover here: a second panic
// must not take the serve goroutine down either.
func (r *queueRouter) recoverCandidate(v any, entry *types.QueueEntry, step string) (advance bool) {
	defer func() {
		if nested := recover(); nested != nil {
			reportRoutePanic(nested, entry, routeStepRecovery)
		}
	}()

	reportRoutePanic(v, entry, step)
	if entry == nil {
		// The panic hit before a candidate was picked: there is no request to fail and
		// nothing was left behind, so the loop waits for the next trigger.
		return false
	}
	// The queue is pluggable, and an entry it hands back may carry no routing context:
	// failing the request is best effort, and a nil dereference here would only add a
	// second panic to the recovery path.
	if entry.RoutingContext != nil {
		entry.SetError(errRouteRecovered)
	}

	switch step {
	case routeStepRoute:
		// Dequeue has not run yet, so the entry is still the queue head: drop it, or the
		// next iteration hands it to the router again and hits the same panic. Its
		// departure is reported like any other failed route.
		dropped, err := r.queue.Dequeue(time.Now())
		if err != nil {
			klog.Errorf("error on dequeue request queue after a recovered panic: %v", err)
			return false
		}
		if dropped != entry {
			// A different entry came back: the queue no longer matches what the loop
			// peeked, so draining stops rather than routing the rest from a position
			// the queue disagrees with. The departure stays unreported, because the
			// candidate did not verifiably leave the queue.
			klog.Error("unexpected request dequeued after a recovered panic")
			r.updateQueuePendingMetric(entry)
			return false
		}
		emitQueueOutcomeMetrics(entry, errRouteRecovered)
		r.updateQueuePendingMetric(entry)
		return true
	case routeStepDequeue:
		// A panic inside Dequeue leaves the queue at an unknown position, so draining
		// stops. The depth is sampled again to keep the gauge in step with what is left.
		r.updateQueuePendingMetric(entry)
	}
	return false
}

// reportRoutePanic logs a panic recovered in the serve loop together with its stack, and
// counts it. The stack is the payload of the log; the panic value never reaches the
// failing request, which only gets a generic internal error.
func reportRoutePanic(v any, entry *types.QueueEntry, step string) {
	requestID := "(no request)"
	if entry != nil && entry.RoutingContext != nil {
		requestID = entry.RequestID
	}
	klog.Errorf("gateway queue router recovered from a panic on %s during %s: %v\n%s", requestID, step, v, debug.Stack())
	metrics.EmitMetricToPrometheus(
		nil,
		nil,
		metrics.GatewayRequestPanicTotal,
		&metrics.SimpleMetricValue{Value: 1},
		map[string]string{"pod_name": metrics.GatewayPodName()},
	)
}

// Outcome label values for gateway_queue_outcome_total. slo_failure and
// capacity_reached are the two conclusions the SLO queue reaches when it rejects a
// request early; error covers everything else, such as a router selection failure.
const (
	queueOutcomeRouted          = "routed"
	queueOutcomeSLOFailure      = "slo_failure"
	queueOutcomeCapacityReached = "capacity_reached"
	queueOutcomeError           = "error"
)

// queueWaitBucketLabel buckets how long a request waited in the queue. Queue waits run
// past the request-path duration buckets (a request can sit in the queue for tens of
// seconds), so the bounds are coarser than durationBucketLabel's.
func queueWaitBucketLabel(d time.Duration) string {
	return msBucketLabel(d.Milliseconds(), []int64{10, 50, 100, 500, 1000, 5000, 10000, 30000, 60000})
}

// msBucketLabel renders ms into the house "low-highms" bucket labels. It mirrors the
// helper of the same name in the gateway package, which owns the label format; here the
// bounds are supplied by each caller.
func msBucketLabel(ms int64, bounds []int64) string {
	if ms < 0 {
		ms = 0
	}
	low := int64(0)
	for _, b := range bounds {
		if ms < b {
			return fmt.Sprintf("%d-%dms", low, b)
		}
		low = b
	}
	return fmt.Sprintf("%dms+", low)
}

// queueOutcomeLabel classifies how a request left the queue.
func queueOutcomeLabel(err error) string {
	switch {
	case err == nil:
		return queueOutcomeRouted
	case errors.Is(err, cache.ErrorSLOFailureRequest):
		return queueOutcomeSLOFailure
	case errors.Is(err, cache.ErrorLoadCapacityReached):
		return queueOutcomeCapacityReached
	default:
		return queueOutcomeError
	}
}

// emitQueueOutcomeMetrics records how a request left the queue and how long it waited
// before that. Both signals are emitted with the queue's low-cardinality labels (model,
// adapter, gateway pod): the queue is a per-model resource, and the pod a request lands
// on is already covered by the request-path metrics.
//
// The labels and the wait start come from the queue entry, not from its routing
// context: the requester may have been unblocked already, and its context recycled.
func emitQueueOutcomeMetrics(entry *types.QueueEntry, routeErr error) {
	labels := entryMetricLabels(entry)
	metrics.EmitMetricToPrometheus(labels, nil, metrics.GatewayQueueWaitTimeBucketTotal,
		&metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"bucket": queueWaitBucketLabel(time.Since(entry.EnqueuedAt))})
	metrics.EmitMetricToPrometheus(labels, nil, metrics.GatewayQueueOutcomeTotal,
		&metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"outcome": queueOutcomeLabel(routeErr)})
}

// entryMetricLabels returns an emission-only context carrying the entry's frozen model
// labels, so departing-entry metrics never read the pooled routing context back.
func entryMetricLabels(entry *types.QueueEntry) *types.RoutingContext {
	return &types.RoutingContext{Model: entry.Model, BaseModel: entry.BaseModel}
}

// emitQueuePendingMetric reports the queue's current depth for the model. It is set (not
// incremented) so a request path that errors between enqueue and dequeue cannot drift
// the gauge.
func emitQueuePendingMetric(entry *types.QueueEntry, pending int) {
	metrics.EmitMetricToPrometheus(entryMetricLabels(entry), nil, metrics.GatewayQueuePendingRequests,
		&metrics.SimpleMetricValue{Value: float64(pending)}, nil)
}

// updateQueuePendingMetric samples the queue depth and publishes it as one serialized
// sample+set pair, so a staler sample cannot overwrite a fresher one when the request
// path and serve report concurrently.
func (r *queueRouter) updateQueuePendingMetric(entry *types.QueueEntry) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	emitQueuePendingMetric(entry, r.queue.Len())
}
