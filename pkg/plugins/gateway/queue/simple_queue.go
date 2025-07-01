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

package queue

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
)

var (
	ErrZeroValueNotSupported = errors.New("zero value not supported")
)

type SimpleQueue[V comparable] struct {
	mu            sync.RWMutex
	queue         []V
	enqueueCursor int64 // Atomic, logic address
	dequeueCursor int64 // Atomic, logic address
	baseCursor    int64 // Used for logic <-> physical address mapping

	// expansion footprints
	zeroout []V
}

func NewSimpleQueue[V comparable](initialCapacity int) *SimpleQueue[V] {
	if initialCapacity < 1 {
		initialCapacity = types.DefaultQueueCapacity
	}
	queue := &SimpleQueue[V]{
		queue: make([]V, initialCapacity),
	}
	if initialCapacity >= types.DefaultQueueCapacity {
		queue.zeroout = make([]V, initialCapacity)
	}
	return queue
}

func (q *SimpleQueue[V]) Enqueue(value V, _ time.Time) error {
	var zero V
	if value == zero {
		return ErrZeroValueNotSupported
	}

	q.mu.RLock()
	defer q.mu.RUnlock()

	cursor := atomic.AddInt64(&q.enqueueCursor, 1) - 1
	pos := q.physicalPosRLocked(cursor)
	if pos >= int64(len(q.queue)) {
		q.mu.RUnlock()
		pos = q.expand(cursor, pos)
		q.mu.RLock()
	}
	q.queue[pos] = value
	return nil
}

func (q *SimpleQueue[V]) Peek(_ time.Time, _ types.PodList) (c V, err error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	nilVal := c
	for {
		dequeueCursor := atomic.LoadInt64(&q.dequeueCursor)
		enqueueCursor := atomic.LoadInt64(&q.enqueueCursor)

		if dequeueCursor >= enqueueCursor {
			return c, types.ErrQueueEmpty
		}

		c = q.queue[q.physicalPosRLocked(dequeueCursor)]
		if c == nilVal {
			// Must unlock to give expand() change to acquire lock.
			q.mu.RUnlock()
			runtime.Gosched()
			q.mu.RLock()
			continue
			// return c, types.ErrQueueEmpty
		}
		return c, nil
	}

}

func (q *SimpleQueue[V]) Dequeue(_ time.Time) (c V, err error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	nilVal := c
	for {
		dequeueCursor := atomic.LoadInt64(&q.dequeueCursor)
		enqueueCursor := atomic.LoadInt64(&q.enqueueCursor)

		if dequeueCursor >= enqueueCursor {
			return c, types.ErrQueueEmpty
		}

		// Like enqueue, dequeuePos can out of bound along with enqueuePos
		dequeuePos := q.physicalPosRLocked(dequeueCursor)
		if dequeuePos >= int64(len(q.queue)) {
			q.mu.RUnlock()
			dequeuePos = q.expand(dequeueCursor, dequeuePos)
			q.mu.RLock()
		}

		// Make sure value was completely enqueued, wait if not.
		c = q.queue[dequeuePos]
		if c == nilVal {
			// Must unlock to give expand() change to acquire lock.
			q.mu.RUnlock()
			runtime.Gosched()
			q.mu.RLock()
			continue
			// return c, types.ErrQueueEmpty
		}
		// We must move dequeueCursor forward after the expand check and confirmed dequeue position, or we may lose the undequed value during expand check.
		if atomic.CompareAndSwapInt64(&q.dequeueCursor, dequeueCursor, dequeueCursor+1) {
			// We don't release reference here, leave expand clear them in the lock.
			// q.queue[dequeuePos] = nilVal
			return c, nil
		} else {
			// reset
			c = nilVal
		}
	}
}

func (q *SimpleQueue[V]) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()

	return int(atomic.LoadInt64(&q.enqueueCursor) - atomic.LoadInt64(&q.dequeueCursor))
}

func (q *SimpleQueue[V]) Cap() int {
	q.mu.RLock()
	defer q.mu.RUnlock()

	return cap(q.queue)
}

func (q *SimpleQueue[V]) physicalPosRLocked(pos int64) int64 {
	return pos - q.baseCursor
}

// Return new position in the queue.
func (q *SimpleQueue[V]) expand(triggerCursor int64, triggerPos int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()

	if triggerPos < int64(len(q.queue)) {
		return q.physicalPosRLocked(triggerCursor)
	}

	oldCapacity := int64(cap(q.queue))
	dequeuePos := q.physicalPosRLocked(q.dequeueCursor) // position before packing/expansion
	used := triggerPos - dequeuePos

	// Determine new capacity
	newQueue := q.queue
	if used > oldCapacity/2 {
		// Expand capacity
		newQueue = make([]V, oldCapacity*2)
		// Pack existing elements
		copy(newQueue, q.queue[dequeuePos:])
	} else {
		// Pack existing elements
		copy(newQueue, q.queue[dequeuePos:])
		// Zero out old elements
		start := len(q.queue) - int(dequeuePos)
		if q.zeroout != nil {
			for start < len(newQueue) {
				copy(newQueue[start:], q.zeroout)
				start += len(q.zeroout)
			}
		} else {
			var nilVal V
			for start < len(newQueue) {
				newQueue[start] = nilVal
				start++
			}
		}
	}
	q.queue, q.baseCursor = newQueue, q.baseCursor+dequeuePos
	return q.physicalPosRLocked(triggerCursor)
}
