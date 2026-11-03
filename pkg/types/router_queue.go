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
// The metadata lives here rather than on RoutingContext because that context is pooled:
// as soon as the requester is unblocked - by a route that sets its target pod, or by
// cancellation - the gateway can return the context to the pool and another request can
// reset it, while this entry is still queued. Anything a queue reports after that point
// (wait time, metric labels) reads these frozen fields, never the context.
type QueueEntry struct {
	*RoutingContext

	// EnqueuedAt is when the request entered the queue; queue wait time is measured
	// from it.
	EnqueuedAt time.Time

	// Model and BaseModel shadow the routing context's fields with the values frozen
	// at enqueue time. Reading them never touches the pooled context.
	Model     string
	BaseModel string
}

// NewQueueEntry freezes the metadata a queue reports from, before the requester can be
// unblocked and its context recycled.
func NewQueueEntry(ctx *RoutingContext, enqueuedAt time.Time) *QueueEntry {
	return &QueueEntry{
		RoutingContext: ctx,
		EnqueuedAt:     enqueuedAt,
		Model:          ctx.Model,
		BaseModel:      ctx.BaseModel,
	}
}
