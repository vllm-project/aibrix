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

package types

import (
	"errors"
	"sync/atomic"
	"time"
)

const DefaultQueueCapacity = 1024

var (
	ErrQueueEmpty = errors.New("queue is empty")
)

type RouterQueue[V comparable] interface {
	Enqueue(V, time.Time) error
	Peek(time.Time, PodList) (V, error)
	Dequeue(time.Time) (V, error)
	Len() int
}

// QueueEntry is one request's residency in a RouterQueue: the routing context a queue
// hands a router, plus the metadata the queue reports from.
//
// The entry retains its pooled RoutingContext until ReleaseRoutingContext.
// Reporting metadata is frozen separately so it remains valid even after the
// queue has released that ownership and the context has been reused.
type QueueEntry struct {
	*RoutingContext

	// EnqueuedAt is when the request entered the queue; queue wait time is measured
	// from it.
	EnqueuedAt time.Time

	// Model and BaseModel shadow the routing context's fields with the values frozen
	// at enqueue time. Reading them never touches the pooled context.
	Model     string
	BaseModel string
	lifetime  atomic.Pointer[routingContextQueueLifetime]
}

// NewQueueEntry retains the context and freezes reporting metadata before the
// entry becomes visible to the queue worker.
func NewQueueEntry(ctx *RoutingContext, enqueuedAt time.Time) *QueueEntry {
	entry := &QueueEntry{
		RoutingContext: ctx,
		EnqueuedAt:     enqueuedAt,
		Model:          ctx.Model,
		BaseModel:      ctx.BaseModel,
	}
	entry.lifetime.Store(ctx.retainForQueue())
	return entry
}

// ReleaseRoutingContext releases the queue's ownership once it has finished all
// routing and accounting work on this entry. Repeated calls are harmless. Frozen
// entry metadata remains readable afterwards, but its RoutingContext may be reused.
func (e *QueueEntry) ReleaseRoutingContext() {
	if lifetime := e.lifetime.Swap(nil); lifetime != nil {
		lifetime.release()
	}
}
