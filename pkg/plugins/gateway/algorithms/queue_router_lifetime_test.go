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

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// Pausing before the worker reads its entry makes the cancellation window exact.
type pausedPeekQueue struct {
	fakeRouterQueue
	entered    chan struct{}
	resume     chan struct{}
	departed   chan struct{}
	peekOnce   sync.Once
	departOnce sync.Once
}

func (q *pausedPeekQueue) Peek(ts time.Time, pods types.PodList) (*types.QueueEntry, error) {
	if q.Len() == 0 {
		return nil, types.ErrQueueEmpty
	}
	q.peekOnce.Do(func() { close(q.entered); <-q.resume })
	return q.fakeRouterQueue.Peek(ts, pods)
}
func (q *pausedPeekQueue) Dequeue(ts time.Time) (*types.QueueEntry, error) {
	entry, err := q.fakeRouterQueue.Dequeue(ts)
	if err == nil {
		q.departOnce.Do(func() { close(q.departed) })
	}
	return entry, err
}

type canceledRequestBackend struct{ calls chan string }

func (r canceledRequestBackend) Route(ctx *types.RoutingContext, _ types.PodList) (string, error) {
	r.calls <- ctx.RequestID
	ctx.SetTargetPod(newQueueTestPod())
	return "10.0.0.1:8000", nil
}

// Returning a released object immediately is a legal pool execution, including
// when the same pointer is still retained by the old queue entry.
type deterministicQueueContextPool struct {
	mu       sync.Mutex
	contexts []*types.RoutingContext
	returned chan *types.RoutingContext
}

func (p *deterministicQueueContextPool) Get() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.contexts) == 0 {
		return &types.RoutingContext{}
	}
	last := len(p.contexts) - 1
	ctx := p.contexts[last]
	p.contexts = p.contexts[:last]
	return ctx
}
func (p *deterministicQueueContextPool) Put(value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := value.(*types.RoutingContext)
	p.contexts = append(p.contexts, ctx)
	if p.returned != nil {
		select {
		case p.returned <- ctx:
		default:
		}
	}
}

func TestQueueDoesNotRouteCanceledEntry(t *testing.T) {
	cache.InitForTest()
	queue := &pausedPeekQueue{entered: make(chan struct{}), resume: make(chan struct{}), departed: make(chan struct{})}
	calls := make(chan string, 1)
	router, err := NewQueueRouter(canceledRequestBackend{calls: calls}, queue)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 2)}
	ctx := types.NewRoutingContextWithPoolForTest(pool, requestCtx, "slo", "original-model", "hello", "request-a", "")
	pods := newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)
	done := make(chan error, 1)
	go func() { _, err := router.Route(ctx, pods); done <- err }()
	waitForSignal(t, queue.entered, "worker to reach the queue head")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Route() error=%v", err)
	}
	ctx.Delete() // Real Process cleanup happens before worker resumes.
	next := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "random", "other-model", "other", "request-b", "")
	defer next.Delete()
	close(queue.resume)
	waitForSignal(t, queue.departed, "canceled entry to leave queue")
	select {
	case requestID := <-calls:
		t.Fatalf("backend routed canceled queue entry %q after its caller returned", requestID)
	default:
	}
	if next.HasRouted() {
		t.Error("old queue entry changed the next request's target")
	}
	if queue.Len() != 0 {
		t.Fatal("canceled entry was not drained")
	}
	waitForReturnedContext(t, pool, ctx)
}

// An unchanged capacity signal leaves the worker asleep until cancellation wakes
// it. No later request or pod update is supplied by this test.
type capacityBlockedQueue struct {
	fakeRouterQueue
	blocked     chan struct{}
	departed    chan struct{}
	blockedOnce sync.Once
	departOnce  sync.Once
}

func (q *capacityBlockedQueue) Peek(ts time.Time, pods types.PodList) (*types.QueueEntry, error) {
	entry, err := q.fakeRouterQueue.Peek(ts, pods)
	if err != nil {
		return entry, err
	}
	if entry.Err() == nil {
		q.blockedOnce.Do(func() { close(q.blocked) })
		return nil, nil
	}
	return entry, nil
}
func (q *capacityBlockedQueue) Dequeue(ts time.Time) (*types.QueueEntry, error) {
	entry, err := q.fakeRouterQueue.Dequeue(ts)
	if err == nil {
		q.departOnce.Do(func() { close(q.departed) })
	}
	return entry, err
}

func TestQueueCancellationWakesCapacityBlockedWorker(t *testing.T) {
	cache.InitForTest()
	queue := &capacityBlockedQueue{blocked: make(chan struct{}), departed: make(chan struct{})}
	calls := make(chan string, 1)
	router, err := NewQueueRouter(canceledRequestBackend{calls: calls}, queue)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := types.NewRoutingContext(requestCtx, "slo", "original-model", "hello", "cancel-no-new-traffic", "")
	defer ctx.Delete()
	pods := newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)
	done := make(chan error, 1)
	go func() { _, err := router.Route(ctx, pods); done <- err }()
	waitForSignal(t, queue.blocked, "worker to become capacity blocked")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Route error=%v", err)
	}
	waitForSignal(t, queue.departed, "cancellation to wake idle worker without new traffic")
	select {
	case id := <-calls:
		t.Fatalf("canceled request %s reached backend", id)
	default:
	}
}

func waitForReturnedContext(t *testing.T, pool *deterministicQueueContextPool, want *types.RoutingContext) {
	t.Helper()
	select {
	case got := <-pool.returned:
		if got != want {
			t.Fatalf("returned context %p, want %p", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context was not returned after the queue and caller finished")
	}
}

func TestQueueReleasesContextAfterSuccessfulDeparture(t *testing.T) {
	cache.InitForTest()
	queue := newGatedDequeueQueue()
	router, err := NewQueueRouter(fakeBackendRouter{}, queue)
	if err != nil {
		t.Fatal(err)
	}
	pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 2)}
	ctx := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "slo", "model", "hello", "successful", "")
	pods := newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)
	done := make(chan error, 1)
	go func() { _, err := router.Route(ctx, pods); done <- err }()
	waitForSignal(t, queue.started, "worker to reach dequeue after successful routing")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx.Delete()
	select {
	case <-pool.returned:
		t.Fatal("context returned before queue departure")
	default:
	}
	close(queue.gate)
	waitForReturnedContext(t, pool, ctx)
}

type rejectingEntryQueue struct{ fakeRouterQueue }

func (*rejectingEntryQueue) Enqueue(*types.QueueEntry, time.Time) error {
	return errors.New("reject enqueue")
}

func TestQueueReleasesContextAfterEnqueueFailure(t *testing.T) {
	cache.InitForTest()
	router, err := NewQueueRouter(fakeBackendRouter{}, &rejectingEntryQueue{})
	if err != nil {
		t.Fatal(err)
	}
	pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 1)}
	ctx := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "slo", "model", "hello", "rejected", "")
	if _, err := router.Route(ctx, newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)); err == nil {
		t.Fatal("enqueue unexpectedly succeeded")
	}
	ctx.Delete()
	waitForReturnedContext(t, pool, ctx)
}

type panickingQueuedBackend struct{}

func (panickingQueuedBackend) Route(*types.RoutingContext, types.PodList) (string, error) {
	panic("test queued backend panic")
}

func TestQueueReleasesContextAfterRecoveredPanic(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		backendPanic, metricPanic bool
	}{
		{"backend", true, false},
		{"departure_metric", false, true},
		{"nested_departure_metric", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache.InitForTest()
			if tc.metricPanic {
				restore := setCounterMetricHook(func(name, help string, value float64, labelNames []string, labelValues ...string) {
					if name == metrics.GatewayQueueOutcomeTotal {
						panic("test departure metric panic")
					}
				})
				defer restore()
			}
			var backend types.Router = fakeBackendRouter{}
			if tc.backendPanic {
				backend = panickingQueuedBackend{}
			}
			router, err := NewQueueRouter(backend, &fakeRouterQueue{})
			if err != nil {
				t.Fatal(err)
			}
			pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 1)}
			ctx := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "slo", "model", "hello", tc.name, "")
			_, err = router.Route(ctx, newMockPodList([]*v1.Pod{newQueueTestPod()}, nil))
			if tc.backendPanic && !errors.Is(err, errRouteRecovered) {
				t.Fatalf("Route error=%v", err)
			}
			if !tc.backendPanic && err != nil {
				t.Fatal(err)
			}
			ctx.Delete()
			waitForReturnedContext(t, pool, ctx)
		})
	}
}
