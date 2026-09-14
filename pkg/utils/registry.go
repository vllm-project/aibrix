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

// Registry is a generic hash set focus on storing values with string keys.
// Array output is optimized by offering a cached copy.
// The cached copy is published as an immutable snapshot, so readers can load it
// without the lock: a snapshot is never modified once published, writers always
// replace it with a new one.
type Registry[V any] struct {
	registry map[string]V
	values   atomic.Pointer[[]V] // Pods cache for quick iteration, nil if invalid
	mu       sync.RWMutex
}

// CustomizedRegistry extends Registry to provide a customized array provider.
type CustomizedRegistry[V any, A comparable] struct {
	Registry[V]
	values         atomic.Pointer[A] // Pods cache for quick iteration, nil if invalid
	valuesProvider func([]V) A
}

func NewRegistry[V any]() *Registry[V] {
	reg := &Registry[V]{}
	reg.values.Store(&[]V{})
	return reg
}

func NewRegistryWithArrayProvider[V any, A comparable](provider func([]V) A) *CustomizedRegistry[V, A] {
	return &CustomizedRegistry[V, A]{
		Registry:       Registry[V]{},
		valuesProvider: provider,
	}
}

func (reg *Registry[V]) Delete(key string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if reg.registry == nil {
		return
	}

	delete(reg.registry, key)
	// Check stale
	if arr := reg.values.Load(); arr != nil && len(*arr) != len(reg.registry) {
		reg.values.Store(nil) // invalidate and wait regenerate
	}
}

func (reg *CustomizedRegistry[V, A]) Delete(key string) {
	reg.Registry.Delete(key)
	// Invalidate after the registry is updated, so a concurrent Array() can not
	// publish a snapshot that misses this update.
	reg.values.Store(nil)
}

func (reg *Registry[V]) Load(key string) (value V, ok bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	value, ok = reg.registry[key]
	return
}

func (reg *Registry[V]) Store(key string, value V) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if reg.registry == nil {
		reg.registry = make(map[string]V, 1)
	}

	_, exist := reg.registry[key]
	reg.registry[key] = value
	if arr := reg.values.Load(); arr != nil && !exist {
		// Copy on write, the published snapshot is never appended in place
		updated := make([]V, len(*arr), len(*arr)+1)
		copy(updated, *arr)
		updated = append(updated, value)
		reg.values.Store(&updated)
	} else {
		// clear and wait regenerate
		reg.values.Store(nil)
	}
}

func (reg *CustomizedRegistry[V, A]) Store(key string, value V) {
	reg.Registry.Store(key, value)
	// Invalidate after the registry is updated, so a concurrent Array() can not
	// publish a snapshot that misses this update.
	reg.values.Store(nil)
}

func (reg *Registry[V]) Array() (arr []V) {
	if reg == nil {
		return nil
	}

	if cached := reg.values.Load(); cached != nil {
		return *cached
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()

	// Reconstruct array
	arr, _ = reg.updateArrayLocked()
	return arr
}

func (reg *CustomizedRegistry[V, A]) Array() (arr A) {
	if reg == nil {
		return
	}

	if cached := reg.values.Load(); cached != nil {
		return *cached
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()

	// Reconstruct array
	return reg.updateArrayLocked()
}

func (reg *Registry[V]) Len() int {
	if reg == nil {
		return 0
	}

	if cached := reg.values.Load(); cached != nil {
		return len(*cached)
	}

	reg.mu.RLock()
	defer reg.mu.RUnlock()

	return len(reg.registry)
}

func (reg *CustomizedRegistry[V, A]) Len() int {
	if reg == nil {
		return 0
	}

	return reg.Registry.Len()
}

func (reg *Registry[V]) updateArrayLocked() ([]V, bool) {
	if cached := reg.values.Load(); cached != nil {
		return *cached, false
	}
	if reg.registry == nil {
		return nil, false
	}

	// Size the snapshot exactly, so no caller can write into it by appending
	arr := make([]V, 0, len(reg.registry))
	for _, pod := range reg.registry {
		arr = append(arr, pod)
	}
	reg.values.Store(&arr)

	return arr, true
}

func (reg *CustomizedRegistry[V, A]) updateArrayLocked() (val A) {
	values, reconstructed := reg.Registry.updateArrayLocked()
	cached := reg.values.Load()
	// Unlike slice: nil can be treated as empty []V, we always create empty A even values is nil
	if !reconstructed && cached != nil {
		return *cached
	}

	// Return the local copy: a concurrent Store may invalidate the cache right
	// after it is published, that must not turn into a nil array here.
	val = reg.valuesProvider(values)
	reg.values.Store(&val)
	return val
}
