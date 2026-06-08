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
	"container/heap"
	"sync"
)

// pqEntry represents an entry in the priority queue.
type pqEntry[T any] struct {
	item     T
	priority int64
	sequence int64 // monotonically increasing sequence number for FIFO ordering
	index    int   // index in the heap, needed for update
}

// priorityQueueHeap implements heap.Interface for a max-heap.
// Higher priority values come first.
type priorityQueueHeap[T any] []*pqEntry[T]

func (h priorityQueueHeap[T]) Len() int { return len(h) }

func (h priorityQueueHeap[T]) Less(i, j int) bool {
	// First compare priority (higher priority comes first)
	if h[i].priority != h[j].priority {
		return h[i].priority > h[j].priority
	}
	// If priority is the same, use sequence number for stable FIFO ordering
	// Lower sequence number means earlier insertion, so it comes first
	return h[i].sequence < h[j].sequence
}

func (h priorityQueueHeap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *priorityQueueHeap[T]) Push(x interface{}) {
	n := len(*h)
	item := x.(*pqEntry[T])
	item.index = n
	*h = append(*h, item)
}

func (h *priorityQueueHeap[T]) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*h = old[0 : n-1]
	return item
}

type PriorityQueueItem interface {
	Key() string
}

type PriorityQueue[T PriorityQueueItem] interface {
	Keys() []string
	Values() []T
	Push(item T, priority int64)
	Pop(n int) []T
	Remove(keys ...string)
	UpdatePriority(key string, priority int64) bool
	Len() int
	// ForEach iterates over all items in priority order (highest first)
	// while holding the lock. The callback returns true to continue,
	// false to break. Items are visited in priority order.
	ForEach(fn func(item T) bool)
}

// PriorityQueueImpl is a heap-based priority queue that supports
// dynamic priority updates and reordering.
//
// Higher priority values come first (max-heap behavior).
type PriorityQueueImpl[T PriorityQueueItem] struct {
	mu          sync.RWMutex
	h           priorityQueueHeap[T]
	lookup      map[string]*pqEntry[T]
	nextSequence int64 // monotonically increasing counter for FIFO ordering
}

// NewPriorityQueue creates a new priority queue.
func NewPriorityQueue[T PriorityQueueItem]() PriorityQueue[T] {
	pq := &PriorityQueueImpl[T]{
		h:      make(priorityQueueHeap[T], 0),
		lookup: make(map[string]*pqEntry[T]),
	}
	heap.Init(&pq.h)
	return pq
}

// Push adds an item with the given priority.
func (pq *PriorityQueueImpl[T]) Push(item T, priority int64) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Already in queue - update priority instead
	if entry, exists := pq.lookup[item.Key()]; exists {
		entry.priority = priority
		heap.Fix(&pq.h, entry.index)
		return
	}

	entry := &pqEntry[T]{
		item:     item,
		priority: priority,
		sequence: pq.nextSequence,
	}
	pq.nextSequence++
	heap.Push(&pq.h, entry)
	pq.lookup[item.Key()] = entry
}

// Pop removes and returns the highest priority items.
func (pq *PriorityQueueImpl[T]) Pop(n int) []T {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if pq.h.Len() == 0 {
		return nil
	}

	result := make([]T, 0, n)
	for range n {
		if pq.h.Len() == 0 {
			break
		}
		entry := heap.Pop(&pq.h).(*pqEntry[T])
		delete(pq.lookup, entry.item.Key())
		result = append(result, entry.item)
	}
	return result
}

// UpdatePriority changes an item's priority and reorders the heap.
// Returns true if the item was found and updated.
func (pq *PriorityQueueImpl[T]) UpdatePriority(key string, newPriority int64) bool {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	entry, exists := pq.lookup[key]
	if !exists {
		return false
	}

	if entry.priority == newPriority {
		return true
	}

	entry.priority = newPriority
	heap.Fix(&pq.h, entry.index)
	return true
}

// Remove removes items from the queue.
func (pq *PriorityQueueImpl[T]) Remove(keys ...string) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for _, key := range keys {
		entry, exists := pq.lookup[key]
		if !exists {
			continue
		}

		heap.Remove(&pq.h, entry.index)
		delete(pq.lookup, key)
	}
}

// Len returns the number of items in the queue.
func (pq *PriorityQueueImpl[T]) Len() int {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	return pq.h.Len()
}

// Keys returns all keys in the queue.
func (pq *PriorityQueueImpl[T]) Keys() []string {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	result := make([]string, 0, pq.h.Len())
	for _, entry := range pq.h {
		result = append(result, entry.item.Key())
	}
	return result
}

// List returns all items in priority order (highest first).
func (pq *PriorityQueueImpl[T]) Values() []T {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	result := make([]T, 0, pq.h.Len())
	for _, entry := range pq.h {
		result = append(result, entry.item)
	}
	return result
}

// ForEach iterates over all items in priority order (highest first)
// while holding the lock. The callback fn returns true to continue,
// false to break. Iteration stops immediately when fn returns false.
func (pq *PriorityQueueImpl[T]) ForEach(fn func(item T) bool) {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	for _, entry := range pq.h {
		if !fn(entry.item) {
			break
		}
	}
}
