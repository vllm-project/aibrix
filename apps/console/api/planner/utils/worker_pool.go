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
	DefaultWorkerQueueSize   = 1000
)

// WorkerPool manages a pool of goroutines that execute submitted functions.
// Submit() adds a function to the pool, Wait() blocks until all submitted
// functions complete.
type WorkerPool struct {
	parallelism int
	queueSize   int
	taskCh      chan func()
	wg          sync.WaitGroup
	workers     sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	lifecycleMu sync.Mutex
	mu          sync.Mutex // guards task admission and active generation fields
	submitters  sync.WaitGroup
	ready       chan struct{} // signaled when all workers are ready
	isRunning   atomic.Bool
}

// NewWorkerPool creates a new worker pool with the given parallelism.
func NewWorkerPool(parallelism int) *WorkerPool {
	if parallelism <= 0 {
		parallelism = DefaultWorkerParallelism
	}
	return NewWorkerPoolWithQueueSize(parallelism, parallelism*2)
}

// NewWorkerPoolWithQueueSize creates a worker pool with a bounded task queue.
func NewWorkerPoolWithQueueSize(parallelism, queueSize int) *WorkerPool {
	if parallelism <= 0 {
		parallelism = DefaultWorkerParallelism
	}
	if queueSize <= 0 {
		queueSize = DefaultWorkerQueueSize
	}
	return &WorkerPool{
		parallelism: parallelism,
		queueSize:   queueSize,
		taskCh:      make(chan func(), queueSize),
		ready:       make(chan struct{}),
	}
}

// Start starts the worker pool goroutines and waits for them to be ready.
func (p *WorkerPool) Start(ctx context.Context) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	if p.isRunning.Load() {
		p.mu.Unlock()
		return
	}
	p.taskCh = make(chan func(), p.queueSize)
	p.ready = make(chan struct{})
	p.isRunning.Store(true)
	p.ctx, p.cancel = context.WithCancel(ctx)
	taskCh := p.taskCh
	ready := p.ready
	workerCtx := p.ctx
	p.mu.Unlock()

	// Use a counter to track when all workers are ready
	var readyCount atomic.Int32
	p.workers.Add(p.parallelism)
	for i := 0; i < p.parallelism; i++ {
		go p.workerLoopWithReady(workerCtx, taskCh, ready, &readyCount)
	}
	// Wait for all workers to signal readiness
	<-ready
}

// Stop stops the worker pool and waits for all goroutines to exit.
func (p *WorkerPool) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.isRunning.Store(false)
	taskCh := p.taskCh
	p.cancel = nil
	p.mu.Unlock()

	// Wait until every accepted Submit has either queued its task or observed
	// cancellation. No task can enter the queue after this point.
	p.submitters.Wait()

	// Run accepted queued work with the cancelled pool context.
drain:
	for {
		select {
		case fn := <-taskCh:
			if fn != nil {
				fn()
			}
			p.wg.Done()
		default:
			break drain // breaks the for loop, not just the select
		}
	}
	// Wait for all workers to exit
	p.wg.Wait()
	p.workers.Wait()
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
	p.submitters.Add(1)
	p.wg.Add(1)
	taskCh := p.taskCh
	ctx := p.ctx
	p.mu.Unlock()
	defer p.submitters.Done()

	select {
	case taskCh <- fn:
	case <-ctx.Done():
		p.wg.Done() // Context cancelled, don't execute
	}
}

// TrySubmit submits fn without waiting for queue capacity.
func (p *WorkerPool) TrySubmit(fn func()) bool {
	if fn == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isRunning.Load() || p.ctx == nil || p.ctx.Err() != nil {
		return false
	}
	p.wg.Add(1)
	select {
	case p.taskCh <- fn:
		return true
	default:
		p.wg.Done()
		return false
	}
}

// Wait blocks until all submitted functions have completed.
func (p *WorkerPool) Wait() {
	p.wg.Wait()
}

func (p *WorkerPool) workerLoopWithReady(
	ctx context.Context,
	taskCh <-chan func(),
	ready chan struct{},
	readyCount *atomic.Int32,
) {
	defer p.workers.Done()
	// Signal readiness when entering the select loop
	count := readyCount.Add(1)
	if count == int32(p.parallelism) {
		// Last worker to start - signal readiness
		close(ready)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-taskCh:
			fn()
			p.wg.Done()
		}
	}
}
