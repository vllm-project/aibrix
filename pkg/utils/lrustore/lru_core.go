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

package cache

import "time"

// UnlockedLRU implements the core capacity- and TTL-bounded LRU structure
// without internal locking. It is NOT safe for concurrent use; callers must
// synchronize all operations externally (for example, LRUStore wraps it with a
// sync.RWMutex, and sharded caches can guard one instance per shard).
//
// Put sets each entry's lastAccessTime to the supplied timestamp and moves it
// to the head of the recency list; Get does not mutate lastAccessTime or list
// order. As long as Put timestamps are non-decreasing (the case for time.Now()
// and monotonic clocks), the list stays ordered newest-at-head to oldest-at-tail,
// allowing O(1) expiry checks and O(expired) tail-walk eviction. If Put ever
// observes a timestamp earlier than the previous Put (e.g. a non-monotonic
// custom test clock), eviction automatically falls back to scanning the map so
// TTL semantics remain exact.
type UnlockedLRU[K comparable, V any] struct {
	freeTable   map[K]*entry[K, V]
	lruList     *list[K, V]
	cap         int
	ttl         time.Duration
	lastPutTime time.Time
	hasLastPut  bool
	ordered     bool
}

// NewUnlockedLRU constructs an unlocked LRU cache with the given entry capacity
// and TTL.
func NewUnlockedLRU[K comparable, V any](cap int, ttl time.Duration) *UnlockedLRU[K, V] {
	l := &list[K, V]{head: &entry[K, V]{}, tail: &entry[K, V]{}}
	l.head.next = l.tail
	l.tail.prev = l.head
	return &UnlockedLRU[K, V]{
		freeTable: make(map[K]*entry[K, V]),
		lruList:   l,
		cap:       cap,
		ttl:       ttl,
		ordered:   true,
	}
}

// Put inserts or updates key with value at timestamp now, moving it to the head
// of the recency list. It returns true if adding a new key caused a capacity
// eviction from the tail.
func (u *UnlockedLRU[K, V]) Put(key K, value V, now time.Time) bool {
	if u.ordered {
		if u.hasLastPut && now.Before(u.lastPutTime) {
			u.ordered = false
		} else {
			u.lastPutTime = now
			u.hasLastPut = true
		}
	}

	if ent, exists := u.freeTable[key]; exists {
		ent.lastAccessTime = now
		ent.Value = value
		u.lruList.moveToHead(ent)
		return false
	}

	ent := &entry[K, V]{Key: key, Value: value, lastAccessTime: now}
	u.lruList.addToHead(ent)
	u.freeTable[key] = ent
	if len(u.freeTable) > u.cap {
		removed := u.lruList.removeTail()
		if removed == nil {
			return false
		}
		delete(u.freeTable, removed.Key)
		return true
	}
	return false
}

// Get returns the value stored for key without mutating access time or order.
func (u *UnlockedLRU[K, V]) Get(key K) (V, bool) {
	if ent, exists := u.freeTable[key]; exists {
		return ent.Value, true
	}
	var zero V
	return zero, false
}

// Len returns the number of entries currently in the cache.
func (u *UnlockedLRU[K, V]) Len() int {
	return len(u.freeTable)
}

// Range calls f for each key-value pair in the cache; iteration stops if f
// returns false.
func (u *UnlockedLRU[K, V]) Range(f func(key K, value V) bool) {
	for k, ent := range u.freeTable {
		if !f(k, ent.Value) {
			return
		}
	}
}

// HasExpired reports whether at least one entry satisfies
// now.Sub(entry.lastAccessTime) > ttl. When Put timestamps are monotonic, this
// is an O(1) check of the tail entry; otherwise it scans until the first
// expired entry is found.
func (u *UnlockedLRU[K, V]) HasExpired(now time.Time) bool {
	if u.ordered {
		tail := u.lruList.tail.prev
		return tail != u.lruList.head && now.Sub(tail.lastAccessTime) > u.ttl
	}
	for _, ent := range u.freeTable {
		if now.Sub(ent.lastAccessTime) > u.ttl {
			return true
		}
	}
	return false
}

// EvictBatch removes up to maxBatch entries satisfying
// now.Sub(entry.lastAccessTime) > ttl (or all expired entries when
// maxBatch <= 0). It returns the number of removed entries and whether
// additional expired entries may remain.
func (u *UnlockedLRU[K, V]) EvictBatch(now time.Time, maxBatch int) (evicted int, more bool) {
	if u.ordered {
		for maxBatch <= 0 || evicted < maxBatch {
			tail := u.lruList.tail.prev
			if tail == u.lruList.head || now.Sub(tail.lastAccessTime) <= u.ttl {
				return evicted, false
			}
			u.lruList.remove(tail)
			delete(u.freeTable, tail.Key)
			evicted++
		}
		tail := u.lruList.tail.prev
		return evicted, tail != u.lruList.head && now.Sub(tail.lastAccessTime) > u.ttl
	}

	for key, ent := range u.freeTable {
		if now.Sub(ent.lastAccessTime) > u.ttl {
			u.lruList.remove(ent)
			delete(u.freeTable, key)
			evicted++
			if maxBatch > 0 && evicted >= maxBatch {
				return evicted, true
			}
		}
	}
	return evicted, false
}

type entry[K comparable, V any] struct {
	Key            K
	Value          V
	prev           *entry[K, V]
	next           *entry[K, V]
	lastAccessTime time.Time
}

type list[K comparable, V any] struct {
	head *entry[K, V]
	tail *entry[K, V]
}

func (l *list[K, V]) addToHead(e *entry[K, V]) {
	e.prev = l.head
	e.next = l.head.next
	l.head.next.prev = e
	l.head.next = e
}

func (l *list[K, V]) moveToHead(e *entry[K, V]) {
	l.remove(e)
	l.addToHead(e)
}

func (l *list[K, V]) remove(e *entry[K, V]) {
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	e.prev = nil
	e.next = nil
}

func (l *list[K, V]) removeTail() *entry[K, V] {
	if l.tail.prev == l.head {
		return nil
	}
	ent := l.tail.prev
	l.remove(ent)
	return ent
}
