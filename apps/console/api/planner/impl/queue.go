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

package impl

import (
	"context"
	"errors"
	"sync"
)

var errQueueClosed = errors.New("queue closed")

const queueCapacity = 256

// pendingQueue is the planner's buffer of pending job IDs.
type pendingQueue interface {
	// Push returns ctx.Err() on caller cancel, errQueueClosed on shutdown.
	Push(ctx context.Context, jobID string) error
	// Pop returns ctx.Err() on caller cancel, errQueueClosed on shutdown.
	Pop(ctx context.Context) (string, error)
	Close()
}

// fifoPendingQueue preserves enqueue order for pending job IDs.
type fifoPendingQueue struct {
	ch        chan string
	done      chan struct{}
	closeOnce sync.Once
}

func newFIFOPendingQueue(capacity int) *fifoPendingQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &fifoPendingQueue{
		ch:   make(chan string, capacity),
		done: make(chan struct{}),
	}
}

func (q *fifoPendingQueue) Push(ctx context.Context, jobID string) error {
	select {
	case q.ch <- jobID:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return errQueueClosed
	}
}

func (q *fifoPendingQueue) Pop(ctx context.Context) (string, error) {
	select {
	case jobID := <-q.ch:
		return jobID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-q.done:
		return "", errQueueClosed
	}
}

func (q *fifoPendingQueue) Close() {
	q.closeOnce.Do(func() {
		close(q.done)
	})
}
