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
)

// Registry is a generic hash set focus on storing values with string keys.
// Array output is optimized by offering a cached read-only snapshot.
// Mutations invalidate the cache without modifying previously returned slices.
type Registry[V any] struct {
	registry map[string]V
	values   []V  // Pods cache for quick iteration
	valid    bool // If value valid?
	mu       sync.RWMutex
}

// CustomizedRegistry extends Registry to provide a customized array provider.
type CustomizedRegistry[V any, A comparable] struct {
	Registry[V]
	values         A // Pods cache for quick iteration
	valuesProvider func([]V) A
}

func NewRegistry[V any]() *Registry[V] {
	return &Registry[V]{
		valid: true,
	}
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

	reg.deleteLocked(key)
}

func (reg *Registry[V]) deleteLocked(key string) bool {
	if _, exists := reg.registry[key]; !exists {
		return false
	}
	delete(reg.registry, key)
	reg.values, reg.valid = nil, false
	return true
}

func (reg *CustomizedRegistry[V, A]) Delete(key string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if !reg.deleteLocked(key) {
		return
	}
	var nilVal A
	reg.values = nilVal
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

	reg.storeLocked(key, value)
}

func (reg *Registry[V]) storeLocked(key string, value V) {
	if reg.registry == nil {
		reg.registry = make(map[string]V, 1)
	}

	reg.registry[key] = value
	reg.values, reg.valid = nil, false
}

func (reg *CustomizedRegistry[V, A]) Store(key string, value V) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var nilVal A
	reg.values = nilVal
	reg.storeLocked(key, value)
}

func (reg *Registry[V]) Array() (arr []V) {
	if reg == nil {
		return nil
	}

	reg.mu.RLock()
	arr, valid := reg.values, reg.valid
	reg.mu.RUnlock()
	if valid {
		return arr
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

	reg.mu.RLock()
	ret := reg.values
	reg.mu.RUnlock()
	if ret != arr { // ret != nil value
		return ret
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
	reconstructed := false
	if !reg.valid {
		// A previous snapshot can still be in use by a routing request.
		// Always allocate a new backing array when rebuilding the cache.
		reg.values = make([]V, 0, len(reg.registry))
		for _, value := range reg.registry {
			reg.values = append(reg.values, value)
		}
		reconstructed = true
		reg.valid = true
	}

	return reg.values, reconstructed
}

func (reg *CustomizedRegistry[V, A]) updateArrayLocked() (val A) {
	values, reconstructed := reg.Registry.updateArrayLocked()
	// Unlike slice: nil can be treated as empty []V, we always create empty A even values is nil
	if reconstructed || reg.values == val { // reg.values == nil val
		reg.values = reg.valuesProvider(values)
	}
	return reg.values
}
