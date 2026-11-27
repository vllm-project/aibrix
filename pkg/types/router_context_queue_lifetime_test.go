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

package types

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The pool deliberately returns the most recently released object. This is a
// permitted sync.Pool execution without depending on a particular P or a GC cycle.
type reuseContextPool struct {
	mu       sync.Mutex
	contexts []*RoutingContext
}

func (p *reuseContextPool) Get() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.contexts) == 0 {
		return &RoutingContext{}
	}
	last := len(p.contexts) - 1
	ctx := p.contexts[last]
	p.contexts = p.contexts[:last]
	return ctx
}
func (p *reuseContextPool) Put(value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contexts = append(p.contexts, value.(*RoutingContext))
}

func TestQueuedRoutingContextSurvivesCallerCancellation(t *testing.T) {
	pool := &reuseContextPool{}
	requestCtx, cancel := context.WithCancel(context.Background())
	ctx := newRoutingContextFromPool(pool, requestCtx, "slo", "model-a", "hello", "request-a", "")
	entry := NewQueueEntry(ctx, time.Now())
	defer entry.ReleaseRoutingContext()
	cancel()
	if ctx.TargetPod() != nil {
		t.Fatal("canceled request unexpectedly routed")
	}
	ctx.Delete() // Process cleanup while the entry has not left the queue.

	next := newRoutingContextFromPool(pool, context.Background(), "slo", "model-b", "other", "request-b", "")
	defer next.Delete()
	if next == entry.RoutingContext {
		t.Errorf("queue still owns request-a, but pool handed its context to request-b")
	}
	if entry.RequestID != "request-a" || entry.RoutingContext.Model != "model-a" {
		t.Errorf("queued request-a now reads requestID=%q model=%q", entry.RequestID, entry.RoutingContext.Model)
	}
}

func TestQueuedContextReturnsAfterBothOwners(t *testing.T) {
	for _, callerFirst := range []bool{false, true} {
		name := "queue_first"
		if callerFirst {
			name = "caller_first"
		}
		t.Run(name, func(t *testing.T) {
			pool := &reuseContextPool{}
			ctx := newRoutingContextFromPool(pool, context.Background(), "slo", "model", "", "request", "")
			entry := NewQueueEntry(ctx, time.Now())
			if callerFirst {
				ctx.Delete()
			} else {
				entry.ReleaseRoutingContext()
			}
			if len(pool.contexts) != 0 {
				t.Fatal("context returned before both owners finished")
			}
			if callerFirst {
				entry.ReleaseRoutingContext()
			} else {
				ctx.Delete()
			}
			if len(pool.contexts) != 1 || pool.contexts[0] != ctx {
				t.Fatal("context not returned exactly once")
			}
			entry.ReleaseRoutingContext()
			if len(pool.contexts) != 1 {
				t.Fatal("duplicate queue release returned context twice")
			}
		})
	}
}

func TestQueuedContextWaitsForEveryEntry(t *testing.T) {
	pool := &reuseContextPool{}
	ctx := newRoutingContextFromPool(pool, context.Background(), "slo", "model", "", "request", "")
	first := NewQueueEntry(ctx, time.Now())
	second := NewQueueEntry(ctx, time.Now())
	ctx.Delete()
	first.ReleaseRoutingContext()
	if len(pool.contexts) != 0 {
		t.Fatal("context returned while second entry still owns it")
	}
	second.ReleaseRoutingContext()
	if len(pool.contexts) != 1 {
		t.Fatal("context was not returned after the last entry")
	}
}

func TestRetiredQueueEntryCannotRecycleNewIncarnation(t *testing.T) {
	pool := &reuseContextPool{}
	ctx := newRoutingContextFromPool(pool, context.Background(), "slo", "old-model", "", "old", "")
	entry := NewQueueEntry(ctx, time.Now())
	ctx.Delete()
	RecycleRoutingContextForTest(ctx, context.Background(), "random", "new-model", "", "new", "")
	entry.ReleaseRoutingContext()
	if len(pool.contexts) != 0 {
		t.Fatal("retired queue entry returned a new incarnation to the pool")
	}
	ctx.Delete()
	if len(pool.contexts) != 1 {
		t.Fatal("new incarnation did not return through its own caller")
	}
}

func TestQueuedContextConcurrentFinalRelease(t *testing.T) {
	pool := &reuseContextPool{}
	ctx := newRoutingContextFromPool(pool, context.Background(), "slo", "model", "", "concurrent", "")
	entry := NewQueueEntry(ctx, time.Now())
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(2)
	go func() { defer done.Done(); <-start; ctx.Delete() }()
	go func() { defer done.Done(); <-start; entry.ReleaseRoutingContext() }()
	close(start)
	done.Wait()
	if len(pool.contexts) != 1 || pool.contexts[0] != ctx {
		t.Fatal("concurrent owners did not return context exactly once")
	}
	entry.ReleaseRoutingContext()
	if len(pool.contexts) != 1 {
		t.Fatal("duplicate release returned context twice")
	}
}
