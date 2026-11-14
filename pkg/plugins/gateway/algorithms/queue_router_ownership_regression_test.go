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
	"sync"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestQueueRetainsContextThroughReportingAndRecovery(t *testing.T) {
	pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 3)}
	ctx := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "slo", "reporting-lifetime", "hello", "original", "")
	entry := types.NewQueueEntry(ctx, time.Now())
	queue := &fakeRouterQueue{}
	if err := queue.Enqueue(entry, time.Now()); err != nil {
		t.Fatal(err)
	}
	// An error-returning backend keeps this focused on routeNext's ownership,
	// without starting a permanent serve goroutine or touching the shared cache.
	router := newRecoveryTestRouter(queue, fakeBackendRouter{err: errTestRouteRejected})
	reporting := make(chan struct{})
	recovering := make(chan struct{})
	continueReport := make(chan struct{})
	continueRecovery := make(chan struct{})
	var reportOnce, recoveryOnce sync.Once
	resumeReport := func() { reportOnce.Do(func() { close(continueReport) }) }
	resumeRecovery := func() { recoveryOnce.Do(func() { close(continueRecovery) }) }
	restore := setCounterMetricHook(func(name, help string, value float64, labelNames []string, labelValues ...string) {
		switch name {
		case metrics.GatewayQueueOutcomeTotal:
			close(reporting)
			<-continueReport
			panic("test departure report failure")
		case metrics.GatewayRequestPanicTotal:
			close(recovering)
			<-continueRecovery
		}
	})
	t.Cleanup(restore)
	finished := make(chan struct{})
	callerReleased := false
	var laterRequests []*types.RoutingContext
	t.Cleanup(func() {
		resumeReport()
		resumeRecovery()
		waitForSignal(t, finished, "routeNext cleanup")
		if !callerReleased {
			ctx.Delete()
		}
		for _, request := range laterRequests {
			request.Delete()
		}
	})
	go func() {
		defer close(finished)
		router.routeNext(nil)
	}()

	waitForSignal(t, reporting, "departure reporting to block after dequeue")
	if queue.Len() != 0 {
		t.Fatal("the reporting checkpoint must follow a successful dequeue")
	}
	ctx.Delete() // The caller has finished while the worker is still reporting.
	callerReleased = true
	duringReport := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "random", "next-model", "next", "during-report", "")
	laterRequests = append(laterRequests, duringReport)
	if duringReport == ctx {
		t.Error("context returned to pool before departure reporting finished")
	}

	resumeReport()
	waitForSignal(t, recovering, "panic reporting to block before recovery uses the entry")
	duringRecovery := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "random", "next-model", "next", "during-recovery", "")
	laterRequests = append(laterRequests, duringRecovery)
	if duringRecovery == ctx {
		t.Error("context returned to pool before recovery finished")
	}
	resumeRecovery()
	waitForSignal(t, finished, "departure reporting and recovery to finish")

	// Early pooling lets recoverCandidate.SetError fail an unrelated request.
	// Check that observable consequence as well as pointer ownership.
	for _, request := range laterRequests {
		if request.HasError() {
			t.Errorf("old worker set an error on unrelated request %q", request.RequestID)
		}
	}
	waitForReturnedContext(t, pool, ctx)
}

func TestQueueRetainsContextWithoutConfirmedDeparture(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backendPanic bool
		dequeueError bool
		dequeuePanic bool
		mismatch     bool
	}{
		{name: "dequeue_error", dequeueError: true},
		{name: "dequeue_panic", dequeuePanic: true},
		{name: "different_entry", mismatch: true},
		{name: "recovery_dequeue_error", backendPanic: true, dequeueError: true},
		{name: "recovery_dequeue_panic", backendPanic: true, dequeuePanic: true},
		{name: "recovery_different_entry", backendPanic: true, mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &deterministicQueueContextPool{returned: make(chan *types.RoutingContext, 2)}
			ctx := types.NewRoutingContextWithPoolForTest(pool, context.Background(), "slo", "retained-model", "hello", "original", "")
			entry := types.NewQueueEntry(ctx, time.Now())
			queue := &panicRecoveryQueue{
				dequeueErrors: tc.dequeueError, dequeuePanics: tc.dequeuePanic, dequeueMismatch: tc.mismatch,
			}
			if err := queue.Enqueue(entry, time.Now()); err != nil {
				t.Fatal(err)
			}
			var backend types.Router = fakeBackendRouter{err: errTestRouteRejected}
			if tc.backendPanic {
				backend = panickingQueuedBackend{}
			}
			router := newRecoveryTestRouter(queue, backend)
			callerReleased := false
			var next *types.RoutingContext
			t.Cleanup(func() {
				if !callerReleased {
					ctx.Delete()
				}
				queue.dequeueErrors, queue.dequeuePanics, queue.dequeueMismatch = false, false, false
				if remaining, err := queue.Dequeue(time.Now()); err == nil {
					remaining.ReleaseRoutingContext()
				}
				if next != nil {
					next.Delete()
				}
			})

			// Execute one real production attempt. A serve loop retry could hide
			// premature pooling by immediately removing the still-queued entry.
			router.routeNext(nil)
			if queue.Len() != 1 {
				t.Fatal("the failed departure must leave the original entry queued")
			}
			ctx.Delete()
			callerReleased = true
			next = types.NewRoutingContextWithPoolForTest(pool, context.Background(), "random", "next-model", "next", "next-request", "")
			if next == ctx {
				t.Error("context returned to pool without a confirmed departure")
			}
			if entry.RequestID != "original" {
				t.Errorf("still-queued entry now describes request %q", entry.RequestID)
			}

			// Once departure can be confirmed, the worker must release ownership.
			queue.dequeueErrors, queue.dequeuePanics, queue.dequeueMismatch = false, false, false
			router.routeNext(nil)
			if queue.Len() != 0 {
				t.Fatal("the subsequent successful departure must remove the entry")
			}
			if next.HasError() {
				t.Error("queued worker set an error on the next request")
			}
			waitForReturnedContext(t, pool, ctx)
		})
	}
}
