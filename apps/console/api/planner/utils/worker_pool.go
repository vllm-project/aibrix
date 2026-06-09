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
	"sync"
	"sync/atomic"
)

const (
	DefaultWorkerParallelism = 8
)

// WorkerPool manages a pool of goroutines that execute submitted functions.
// Submit() adds a function to the pool, Wait() blocks until all submitted
// functions complete.
type WorkerPool struct {
	parallelism int
	taskCh      chan func()
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex    // guards Submit/Stop synchronization
	ready       chan struct{} // signaled when all workers are ready
	isRunning   atomic.Bool
}

// NewWorkerPool creates a new worker pool with the given parallelism.
func NewWorkerPool(parallelism int) *WorkerPool {
	if parallelism <= 0 {
		parallelism = DefaultWorkerParallelism
	}
	return &WorkerPool{
		parallelism: parallelism,
		taskCh:      make(chan func(), parallelism*2), // buffered for burst submits
		ready:       make(chan struct{}),
	}
}

// Start starts the worker pool goroutines and waits for them to be ready.
func (p *WorkerPool) Start(ctx context.Context) {
	if p.isRunning.Load() {
		return
	}
	p.isRunning.Store(true)
	p.ctx, p.cancel = context.WithCancel(ctx)

	// Use a counter to track when all workers are ready
	var readyCount atomic.Int32
	for i := 0; i < p.parallelism; i++ {
		go p.workerLoopWithReady(&readyCount)
	}
	// Wait for all workers to signal readiness
	<-p.ready
}

// Stop stops the worker pool and waits for all goroutines to exit.
func (p *WorkerPool) Stop() {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.isRunning.Store(false)
	p.mu.Unlock()

	// Drain remaining tasks so workers can exit
drain:
	for {
		select {
		case fn := <-p.taskCh:
			// Drop task - pool is shutting down
			// Must decrement WaitGroup since Submit incremented it
			p.wg.Done()
			if fn != nil {
				// Execute the function to ensure cleanup (optional)
				// This ensures any deferred cleanup in submitted functions runs
				fn()
			}
		default:
			break drain // breaks the for loop, not just the select
		}
	}
	// Wait for all workers to exit
	p.wg.Wait()
}

// Submit submits a function to be executed by a worker goroutine.
// Submit increments the WaitGroup before sending to the channel,
// ensuring Wait() can track completion.
func (p *WorkerPool) Submit(fn func()) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	running := p.isRunning.Load()
	if !running {
		p.mu.Unlock()
		return
	}
	p.wg.Add(1)
	p.mu.Unlock()

	select {
	case p.taskCh <- fn:
	case <-p.ctx.Done():
		p.wg.Done() // Context cancelled, don't execute
	}
}

// Wait blocks until all submitted functions have completed.
func (p *WorkerPool) Wait() {
	p.wg.Wait()
}

func (p *WorkerPool) workerLoopWithReady(readyCount *atomic.Int32) {
	// Signal readiness when entering the select loop
	count := readyCount.Add(1)
	if count == int32(p.parallelism) {
		// Last worker to start - signal readiness
		close(p.ready)
	}

	for {
		select {
		case <-p.ctx.Done():
			return
		case fn := <-p.taskCh:
			fn()
			p.wg.Done()
		}
	}
}
