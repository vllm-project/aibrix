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
package utils

import (
	"sync"
	"sync/atomic"
)

type SyncMap[K any, V any] struct {
	m   sync.Map
	len int32
}

func (sm *SyncMap[K, V]) Delete(key K) {
	sm.LoadAndDelete(key)
}

func (sm *SyncMap[K, V]) Load(key K) (typedVal V, ok bool) {
	value, ok := sm.m.Load(key)
	if ok {
		typedVal = value.(V)
	}
	return
}

func (sm *SyncMap[K, V]) LoadAndDelete(key K) (typedVal V, loaded bool) {
	value, loaded := sm.m.LoadAndDelete(key)
	if loaded {
		typedVal = value.(V)
		atomic.AddInt32(&sm.len, -1)
	}
	return
}

func (sm *SyncMap[K, V]) LoadOrStore(key K, value V) (V, bool) {
	actual, loaded := sm.m.LoadOrStore(key, value)
	if !loaded {
		atomic.AddInt32(&sm.len, 1)
	}
	return actual.(V), loaded
}

func (sm *SyncMap[K, V]) Range(f func(key K, value V) bool) {
	sm.m.Range(func(key, value any) bool {
		return f(key.(K), value.(V))
	})
}

func (sm *SyncMap[K, V]) Keys() []K {
	k := make([]K, 0, sm.Len())
	sm.m.Range(func(key, value any) bool {
		k = append(k, key.(K))
		return true
	})
	return k
}

func (sm *SyncMap[K, V]) Values() []V {
	v := make([]V, 0, sm.Len())
	sm.m.Range(func(key, value any) bool {
		v = append(v, value.(V))
		return true
	})
	return v
}

func (sm *SyncMap[K, V]) Store(key K, value V) {
	sm.Swap(key, value)
}

func (sm *SyncMap[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	if deleted = sm.m.CompareAndDelete(key, old); deleted {
		atomic.AddInt32(&sm.len, -1)
	}
	return
}

func (sm *SyncMap[K, V]) CompareAndSwap(key K, old V, new V) bool {
	return sm.m.CompareAndSwap(key, old, new)
}

func (sm *SyncMap[K, V]) Swap(key K, value V) (V, bool) {
	old, loaded := sm.m.Swap(key, value)
	if !loaded {
		var ret V
		atomic.AddInt32(&sm.len, 1)
		return ret, loaded
	}
	return old.(V), loaded
}

func (sm *SyncMap[K, V]) Len() int {
	return int(atomic.LoadInt32(&sm.len))
}
