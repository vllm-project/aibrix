/*
Copyright 2025 The Aibrix Team.

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

package cache

import (
	"sync"
	"time"
)

const defaultEvictBatchSize = 256

type getCurrentTime func() time.Time

var DefaultGetCurrentTime = func() time.Time {
	return time.Now()
}

type LRUStore[K comparable, V any] struct {
	sync.RWMutex
	core *UnlockedLRU[K, V]

	getCurrentTime
	interval  time.Duration
	stop      chan struct{}
	closeOnce sync.Once
}

var _ Store[uint64, struct{}] = (*LRUStore[uint64, struct{}])(nil)

// NewLRUStore creates a thread-safe LRUStore with the given capacity, TTL,
// background eviction interval, and clock function f. Supplying a monotonic
// clock (such as DefaultGetCurrentTime / time.Now) keeps eviction on the
// O(1)/O(expired) tail-walk path; non-monotonic clocks fall back to scanning
// until surviving entries are ordered again.
func NewLRUStore[K comparable, V any](cap int, ttl, interval time.Duration, f getCurrentTime) *LRUStore[K, V] {
	store := &LRUStore[K, V]{
		core:           NewUnlockedLRU[K, V](cap, ttl),
		interval:       interval,
		getCurrentTime: f,
		stop:           make(chan struct{}),
	}

	// time.NewTicker panics when the interval is not positive. A
	// non-positive interval disables background eviction. Put and Get
	// still work, and they do not check TTL, so entries do not expire.
	if interval > 0 {
		go store.startEviction()
	}
	return store
}

// Close stops the background eviction goroutine. It is safe to call more than once.
func (e *LRUStore[K, V]) Close() {
	e.closeOnce.Do(func() {
		close(e.stop)
	})
}

func (e *LRUStore[K, V]) startEviction() {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.Evict(e.getCurrentTime())
		case <-e.stop:
			return
		}
	}
}

func (e *LRUStore[K, V]) Put(key K, value V) bool {
	e.Lock()
	defer e.Unlock()

	return e.core.Put(key, value, e.getCurrentTime())
}

func (e *LRUStore[K, V]) Get(key K) (V, bool) {
	e.RLock()
	defer e.RUnlock()

	return e.core.Get(key)
}

func (e *LRUStore[K, V]) Len() int {
	e.RLock()
	defer e.RUnlock()

	return e.core.Len()
}

// Range calls f for each key-value pair in the store; iteration stops if f returns false.
func (e *LRUStore[K, V]) Range(f func(key K, value V) bool) {
	e.RLock()
	defer e.RUnlock()

	e.core.Range(f)
}

// Evict removes entries expired at now (those with now.Sub(lastAccessTime) > ttl)
// and returns the number of entries evicted. When access timestamps are monotonic,
// the fast path checks only the tail under RLock in O(1), and expired entries are
// removed from the tail in bounded batches so large expiration bursts release the
// write lock between batches.
func (e *LRUStore[K, V]) Evict(now time.Time) int {
	e.RLock()
	if !e.core.HasExpired(now) {
		e.RUnlock()
		return 0
	}
	// Bound total evictions to the store's current size so a concurrent writer
	// inserting already-expired entries (e.g., negative TTL or skewed clock)
	// cannot keep a single Evict call looping indefinitely.
	limit := e.core.Len()
	e.RUnlock()

	total := 0
	for total < limit {
		batch := defaultEvictBatchSize
		if rem := limit - total; rem < batch {
			batch = rem
		}
		e.Lock()
		evicted, more := e.core.EvictBatch(now, batch)
		e.Unlock()
		total += evicted
		if !more || evicted == 0 {
			break
		}
	}
	return total
}
