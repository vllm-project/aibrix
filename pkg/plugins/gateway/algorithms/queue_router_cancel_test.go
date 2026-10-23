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

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

// gatedPeekQueue holds the serve goroutine at its first Peek until the gate opens, so
// a test can cancel a request while it is still queued.
type gatedPeekQueue struct {
	fakeRouterQueue
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newGatedPeekQueue() *gatedPeekQueue {
	return &gatedPeekQueue{gate: make(chan struct{}), started: make(chan struct{})}
}

func (q *gatedPeekQueue) Peek(ts time.Time, pods types.PodList) (*types.QueueEntry, error) {
	q.once.Do(func() { close(q.started) })
	<-q.gate
	return q.fakeRouterQueue.Peek(ts, pods)
}

// recordingBackendRouter records the request ID of every context it routes and sets
// a target pod on it.
type recordingBackendRouter struct {
	mu     sync.Mutex
	routed []string
}

func (r *recordingBackendRouter) Route(ctx *types.RoutingContext, _ types.PodList) (string, error) {
	r.mu.Lock()
	r.routed = append(r.routed, ctx.RequestID)
	r.mu.Unlock()
	ctx.SetTargetPod(newQueueTestPod())
	return "10.0.0.1:8000", nil
}

func (r *recordingBackendRouter) routedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.routed...)
}

// TestQueueDropsRequestThatStoppedWaiting cancels a request (or lets its deadline
// pass) while it waits in the queue, then recycles its pooled routing context for a
// new request, as the gateway does once the first request returns. The queue must
// drop the entry instead of routing it: routing it would hand the new request a
// target pod chosen for the old one.
func TestQueueDropsRequestThatStoppedWaiting(t *testing.T) {
	cases := []struct {
		name        string
		newContext  func() (context.Context, context.CancelFunc)
		stopWaiting func(cancel context.CancelFunc)
		wantErr     error
		wantOutcome string
	}{
		{
			name:        "client canceled",
			newContext:  func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			stopWaiting: func(cancel context.CancelFunc) { cancel() },
			wantErr:     context.Canceled,
			wantOutcome: queueOutcomeCanceled,
		},
		{
			name: "deadline passed",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			stopWaiting: func(context.CancelFunc) {},
			wantErr:     context.DeadlineExceeded,
			wantOutcome: queueOutcomeDeadlineExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache.InitForTest()
			capture := startMetricCapture()
			defer capture.restore()

			queue := newGatedPeekQueue()
			backend := &recordingBackendRouter{}
			router, err := NewQueueRouter(backend, queue)
			if err != nil {
				t.Fatalf("NewQueueRouter() error = %v", err)
			}

			requestCtx, cancel := tc.newContext()
			defer cancel()
			ctx := types.NewRoutingContext(requestCtx, types.RoutingAlgorithm("slo"), "m", "hello world", "req-gone", "")
			pods := newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)

			routeDone := make(chan error, 1)
			go func() {
				_, err := router.Route(ctx, pods)
				routeDone <- err
			}()

			waitForSignal(t, queue.started, "serve to reach Peek")
			tc.stopWaiting(cancel)
			if err := <-routeDone; !errors.Is(err, tc.wantErr) {
				t.Fatalf("Route() error = %v, want %v", err, tc.wantErr)
			}

			types.RecycleRoutingContextForTest(ctx, context.Background(), types.RoutingAlgorithm("slo"), "m", "hello world", "req-next", "")

			close(queue.gate)

			outcomes := waitForEmissions(t, func() []metricEmission {
				return capture.countersFor(metrics.GatewayQueueOutcomeTotal)
			}, 1)
			if got := labelValue(outcomes[0], "outcome"); got != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, tc.wantOutcome)
			}
			if n := queue.Len(); n != 0 {
				t.Fatalf("queue length = %d, want 0: the entry was not dropped", n)
			}
			if got := backend.routedIDs(); len(got) != 0 {
				t.Fatalf("backend routed %v, want nothing: the request that stopped waiting was routed", got)
			}
			if ctx.HasRouted() {
				t.Fatal("recycled context got a target pod from the dropped entry")
			}
		})
	}
}
