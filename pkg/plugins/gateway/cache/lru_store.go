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

type LRUStore[K comparable, V any] struct {
	sync.RWMutex
	freeTable map[K]*entry[K, V]
	lruList   *list[K, V]
	cap       int

	interval time.Duration
	ttl      time.Duration
}

func NewLRUStore[K comparable, V any](cap int, ttl, interval time.Duration) *LRUStore[K, V] {
	store := &LRUStore[K, V]{
		freeTable: make(map[K]*entry[K, V]),
		lruList:   &list[K, V]{head: &entry[K, V]{}, tail: &entry[K, V]{}},
		cap:       cap,
		ttl:       ttl,
		interval:  interval,
	}
	store.lruList.head.next = store.lruList.tail
	store.lruList.tail.prev = store.lruList.head

	go store.startEviction()
	return store
}

func (e *LRUStore[K, V]) startEviction() {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for range ticker.C {
		e.evict(time.Now())
	}
}

func (e *LRUStore[K, V]) Put(key K, value V) bool {
	e.Lock()
	defer e.Unlock()

	if entry, exists := e.freeTable[key]; exists {
		entry.lastAccessTime = time.Now()
		entry.Value = value
		e.lruList.moveToHead(entry)
		return false
	}

	entry := &entry[K, V]{Key: key, Value: value, lastAccessTime: time.Now()}
	e.lruList.addToHead(entry)
	e.freeTable[key] = entry
	if len(e.freeTable) > e.cap {
		removed := e.lruList.removeTail()
		if removed == nil {
			return false
		}
		delete(e.freeTable, removed.Key)
		return true
	}
	return false
}

func (e *LRUStore[K, V]) Get(key K) (V, bool) {
	e.Lock()
	defer e.Unlock()

	if entry, exists := e.freeTable[key]; exists {
		entry.lastAccessTime = time.Now()
		e.lruList.moveToHead(entry)
		return entry.Value, true
	}
	var zero V
	return zero, false
}

func (e *LRUStore[K, V]) Len() int {
	e.RLock()
	defer e.RUnlock()
	return len(e.freeTable)
}

func (e *LRUStore[K, V]) evict(now time.Time) {
	var keysToEvict []K

	e.Lock()
	for key, entry := range e.freeTable {
		if now.Sub(entry.lastAccessTime) > e.ttl {
			keysToEvict = append(keysToEvict, key)
		}
	}
	e.Unlock()

	for _, key := range keysToEvict {
		e.Lock()
		if entry, exists := e.freeTable[key]; exists && now.Sub(entry.lastAccessTime) > e.ttl {
			e.lruList.remove(entry)
			delete(e.freeTable, key)
		}
		e.Unlock()
	}
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
	entry := l.tail.prev
	l.remove(entry)
	return entry
}
