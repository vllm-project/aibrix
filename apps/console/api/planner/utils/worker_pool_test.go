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

package utils

import (
	"context"
	"testing"
	"time"
)

func TestWorkerPoolTrySubmitDoesNotBlockWhenQueueIsFull(t *testing.T) {
	pool := NewWorkerPoolWithQueueSize(1, 1)
	pool.Start(context.Background())
	defer pool.Stop()

	started := make(chan struct{})
	release := make(chan struct{})
	if !pool.TrySubmit(func() {
		close(started)
		<-release
	}) {
		t.Fatal("failed to submit active task")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start active task")
	}

	if !pool.TrySubmit(func() {}) {
		t.Fatal("failed to fill worker queue")
	}
	start := time.Now()
	if pool.TrySubmit(func() {}) {
		t.Fatal("TrySubmit accepted a task after the queue was full")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("TrySubmit blocked for %v", elapsed)
	}

	close(release)
	pool.Wait()
}

func TestWorkerPoolStopRunsAcceptedQueuedTasks(t *testing.T) {
	pool := NewWorkerPoolWithQueueSize(1, 1)
	pool.Start(context.Background())

	started := make(chan struct{})
	release := make(chan struct{})
	if !pool.TrySubmit(func() {
		close(started)
		<-release
	}) {
		t.Fatal("failed to submit active task")
	}
	<-started

	queuedRan := make(chan struct{})
	if !pool.TrySubmit(func() {
		close(queuedRan)
	}) {
		t.Fatal("failed to submit queued task")
	}

	stopDone := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while the active task was blocked")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for accepted tasks")
	}

	select {
	case <-queuedRan:
	default:
		t.Fatal("Stop dropped an accepted queued task")
	}
}

func TestWorkerPoolTrySubmitRejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := NewWorkerPoolWithQueueSize(1, 1)
	pool.Start(ctx)
	cancel()

	if pool.TrySubmit(func() {}) {
		t.Fatal("TrySubmit accepted work after context cancellation")
	}
	pool.Stop()
}

func TestWorkerPoolCanRestartAfterStop(t *testing.T) {
	pool := NewWorkerPoolWithQueueSize(1, 1)

	for attempt := 0; attempt < 2; attempt++ {
		pool.Start(context.Background())
		ran := make(chan struct{})
		if !pool.TrySubmit(func() {
			close(ran)
		}) {
			t.Fatalf("attempt %d: failed to submit task", attempt)
		}
		pool.Wait()
		select {
		case <-ran:
		default:
			t.Fatalf("attempt %d: submitted task did not run", attempt)
		}
		pool.Stop()
	}
}
