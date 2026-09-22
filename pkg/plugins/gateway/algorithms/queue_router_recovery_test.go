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
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

// errTestRouteRejected is what panicRecoveryRouter fails a request with.
var errTestRouteRejected = errors.New("router rejected the request")

// errTestDequeueFailed is what panicRecoveryQueue fails a Dequeue with when it is told to.
var errTestDequeueFailed = errors.New("dequeue failed")

// panicRecoveryQueue is a minimal RouterQueue that can be made to panic inside Peek or
// Dequeue, or fail a Dequeue, so the serve loop's recovery is exercised without a full SLO
// queue setup.
type panicRecoveryQueue struct {
	mu            sync.Mutex
	entries       []*types.QueueEntry
	peeks         int
	dequeues      int
	peekPanics    bool
	dequeuePanics bool
	dequeueErrors bool
}

func (q *panicRecoveryQueue) Enqueue(entry *types.QueueEntry, _ time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries = append(q.entries, entry)
	return nil
}

func (q *panicRecoveryQueue) Peek(_ time.Time, _ types.PodList) (*types.QueueEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.peeks++
	if q.peekPanics {
		panic("peek exploded")
	}
	if len(q.entries) == 0 {
		return nil, types.ErrQueueEmpty
	}
	return q.entries[0], nil
}

func (q *panicRecoveryQueue) Dequeue(_ time.Time) (*types.QueueEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dequeues++
	if q.dequeuePanics {
		panic("dequeue exploded")
	}
	if q.dequeueErrors {
		return nil, errTestDequeueFailed
	}
	if len(q.entries) == 0 {
		return nil, types.ErrQueueEmpty
	}
	entry := q.entries[0]
	q.entries = q.entries[1:]
	return entry, nil
}

func (q *panicRecoveryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

func (q *panicRecoveryQueue) counts() (peeks, dequeues int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peeks, q.dequeues
}

func (q *panicRecoveryQueue) setPeekPanics(panics bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.peekPanics = panics
}

// panicRecoveryRouter panics on the request named by panicRequestID and fails every
// other request, so tests can observe how far the serve loop got without reaching the
// cache.
type panicRecoveryRouter struct {
	mu             sync.Mutex
	calls          []string
	panicRequestID string
}

func (r *panicRecoveryRouter) Route(ctx *types.RoutingContext, _ types.PodList) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, ctx.RequestID)
	r.mu.Unlock()
	if ctx.RequestID == r.panicRequestID {
		panic("router exploded")
	}
	return "", errTestRouteRejected
}

func (r *panicRecoveryRouter) routedRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// newRecoveryTestRouter wires a queueRouter by hand: the tests route failing requests,
// so the cache is never reached and can stay nil.
func newRecoveryTestRouter(queue types.RouterQueue[*types.QueueEntry], backend types.Router) *queueRouter {
	return &queueRouter{
		router:         backend,
		queue:          queue,
		chRouteTrigger: make(chan types.PodList, 1),
	}
}

func newRecoveryTestEntry(requestID string) *types.QueueEntry {
	ctx := RouterSLO.NewContext(context.Background(), "test-model", "hello", requestID, "")
	return types.NewQueueEntry(ctx, time.Now())
}

// TestServeRecoversRoutePanicAndKeepsDraining covers the panic that starts on the serve
// goroutine while a candidate is routed: the request behind it fails, the candidate is
// dropped instead of being routed again, and the entries behind it still go through.
func TestServeRecoversRoutePanicAndKeepsDraining(t *testing.T) {
	capture := startMetricCapture()
	defer capture.restore()

	testQueue := &panicRecoveryQueue{}
	poisoned := newRecoveryTestEntry("req-poisoned")
	followUp := newRecoveryTestEntry("req-follow-up")
	require.NoError(t, testQueue.Enqueue(poisoned, time.Now()))
	require.NoError(t, testQueue.Enqueue(followUp, time.Now()))

	backend := &panicRecoveryRouter{panicRequestID: "req-poisoned"}
	router := newRecoveryTestRouter(testQueue, backend)
	go router.serve()
	router.chRouteTrigger <- nil

	require.Eventually(t, func() bool {
		return len(backend.routedRequests()) == 2 && testQueue.Len() == 0
	}, 5*time.Second, 5*time.Millisecond, "the drain loop must move on to the next candidate")

	assert.Equal(t, []string{"req-poisoned", "req-follow-up"}, backend.routedRequests())
	assert.True(t, poisoned.HasError(), "the request behind the panic must be failed")
	assert.ErrorIs(t, poisoned.GetError(), errRouteRecovered)
	assert.ErrorIs(t, followUp.GetError(), errTestRouteRejected)

	panics := capture.countersFor(metrics.GatewayRequestPanicTotal)
	require.Len(t, panics, 1, "the recovered panic must be counted once")
	assert.Equal(t, 1.0, panics[0].value)
	assert.Equal(t, metrics.GatewayPodName(), labelValue(panics[0], "pod_name"))

	// The dropped candidate and the follow-up both left the queue through an error.
	outcomes := waitForEmissions(t, func() []metricEmission {
		return capture.countersFor(metrics.GatewayQueueOutcomeTotal)
	}, 2)
	require.Len(t, outcomes, 2)
	for _, outcome := range outcomes {
		assert.Equal(t, queueOutcomeError, labelValue(outcome, "outcome"))
	}

	pending := waitForEmissions(t, func() []metricEmission {
		return capture.gaugesFor(metrics.GatewayQueuePendingRequests)
	}, 2)
	require.Len(t, pending, 2)
	assert.Equal(t, 1.0, pending[0].value, "the depth is sampled again after the candidate is dropped")
	assert.Equal(t, 0.0, pending[1].value, "the depth is 0 once the follow-up leaves the queue")
}

// TestServeRecoversDequeuePanicWithoutRetrying pins that a panic inside Dequeue stops the
// drain: the queue is at an unknown position, so the entry is not dequeued twice.
func TestServeRecoversDequeuePanicWithoutRetrying(t *testing.T) {
	capture := startMetricCapture()
	defer capture.restore()

	testQueue := &panicRecoveryQueue{dequeuePanics: true}
	request := newRecoveryTestEntry("req-1")
	require.NoError(t, testQueue.Enqueue(request, time.Now()))

	router := newRecoveryTestRouter(testQueue, &panicRecoveryRouter{})
	go router.serve()
	router.chRouteTrigger <- nil

	panics := waitForEmissions(t, func() []metricEmission {
		return capture.countersFor(metrics.GatewayRequestPanicTotal)
	}, 1)
	require.Len(t, panics, 1)

	// Give an implementation that advances after the panic time to run the Dequeue it
	// would then attempt.
	time.Sleep(50 * time.Millisecond)
	peeks, dequeues := testQueue.counts()
	assert.Equal(t, 1, peeks, "draining must stop after a panic inside Dequeue")
	assert.Equal(t, 1, dequeues, "a panic inside Dequeue must not trigger another Dequeue")
	assert.Equal(t, 1, testQueue.Len(), "the entry stays queued when Dequeue panics")
	assert.True(t, request.HasError())
	assert.Empty(t, capture.countersFor(metrics.GatewayQueueOutcomeTotal),
		"an entry that did not verifiably leave the queue is not reported as a departure")
}

// TestServeRecoversPeekPanicAndStopsDraining also checks that the goroutine survives: the
// same serve loop routes the request once the queue stops panicking.
func TestServeRecoversPeekPanicAndStopsDraining(t *testing.T) {
	capture := startMetricCapture()
	defer capture.restore()

	testQueue := &panicRecoveryQueue{peekPanics: true}
	request := newRecoveryTestEntry("req-1")
	require.NoError(t, testQueue.Enqueue(request, time.Now()))

	backend := &panicRecoveryRouter{}
	router := newRecoveryTestRouter(testQueue, backend)
	go router.serve()
	router.chRouteTrigger <- nil

	panics := waitForEmissions(t, func() []metricEmission {
		return capture.countersFor(metrics.GatewayRequestPanicTotal)
	}, 1)
	require.Len(t, panics, 1)

	time.Sleep(50 * time.Millisecond)
	peeks, dequeues := testQueue.counts()
	assert.Equal(t, 1, peeks, "draining must stop when Peek panics")
	assert.Equal(t, 0, dequeues, "nothing is dequeued when Peek panics")
	assert.False(t, request.HasError(), "the request was never peeked, so it stays queued")
	assert.Equal(t, 1, testQueue.Len())

	testQueue.setPeekPanics(false)
	router.chRouteTrigger <- nil
	require.Eventually(t, func() bool { return testQueue.Len() == 0 }, 5*time.Second, 5*time.Millisecond,
		"the serve goroutine must survive a panic that it recovered from")
	assert.Equal(t, []string{"req-1"}, backend.routedRequests())
}

// TestServeStopsDrainingWhenTheCandidateCannotBeDropped covers the recovery failing to
// drop the candidate that panicked: the entry stays queued, nothing is reported for a
// departure that did not happen, and the loop waits for the next trigger. Dropping it can
// fail with an error, or raise a second panic that is recovered as well.
func TestServeStopsDrainingWhenTheCandidateCannotBeDropped(t *testing.T) {
	cases := []struct {
		name       string
		queue      *panicRecoveryQueue
		wantPanics int
	}{
		{"dequeue returns an error", &panicRecoveryQueue{dequeueErrors: true}, 1},
		{"dequeue panics again", &panicRecoveryQueue{dequeuePanics: true}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := startMetricCapture()
			defer capture.restore()

			poisoned := newRecoveryTestEntry("req-poisoned")
			require.NoError(t, tc.queue.Enqueue(poisoned, time.Now()))

			backend := &panicRecoveryRouter{panicRequestID: "req-poisoned"}
			router := newRecoveryTestRouter(tc.queue, backend)
			go router.serve()
			router.chRouteTrigger <- nil

			panics := waitForEmissions(t, func() []metricEmission {
				return capture.countersFor(metrics.GatewayRequestPanicTotal)
			}, tc.wantPanics)
			require.Len(t, panics, tc.wantPanics)

			time.Sleep(50 * time.Millisecond)
			peeks, dequeues := tc.queue.counts()
			assert.Equal(t, 1, peeks, "draining stops when the candidate cannot be dropped")
			assert.Equal(t, 1, dequeues, "a failed drop is not retried")
			assert.Equal(t, 1, tc.queue.Len(), "the entry stays queued")
			assert.True(t, poisoned.HasError())
			assert.Empty(t, capture.countersFor(metrics.GatewayQueueOutcomeTotal),
				"a departure that did not happen is not reported")
		})
	}
}

func TestServeRoutesRequestsWithoutPanic(t *testing.T) {
	capture := startMetricCapture()
	defer capture.restore()

	testQueue := &panicRecoveryQueue{}
	request := newRecoveryTestEntry("req-1")
	require.NoError(t, testQueue.Enqueue(request, time.Now()))

	backend := &panicRecoveryRouter{}
	router := newRecoveryTestRouter(testQueue, backend)
	go router.serve()
	router.chRouteTrigger <- nil

	require.Eventually(t, func() bool { return testQueue.Len() == 0 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, []string{"req-1"}, backend.routedRequests())
	assert.True(t, request.HasError(), "the router rejected the request")
	assert.Empty(t, capture.countersFor(metrics.GatewayRequestPanicTotal))
}
