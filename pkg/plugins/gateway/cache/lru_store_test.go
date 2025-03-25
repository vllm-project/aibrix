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
	"fmt"
	"testing"
	"time"
)

func TestLRUStore_PutAndGet(t *testing.T) {
	store := NewLRUStore[string, string](2, 5*time.Second, 1*time.Second)

	// Test adding and retrieving items
	store.Put("key1", "value1")
	store.Put("key2", "value2")

	if val, ok := store.Get("key1"); !ok || val != "value1" {
		t.Errorf("expected value1, got %v", val)
	}

	if val, ok := store.Get("key2"); !ok || val != "value2" {
		t.Errorf("expected value2, got %v", val)
	}

	store.Put("key3", "value3")
	if _, ok := store.Get("key1"); ok {
		t.Errorf("expected key1 to be evicted")
	}

	if val, ok := store.Get("key3"); !ok || val != "value3" {
		t.Errorf("expected value3, got %v", val)
	}
}

func TestLRUStore_TTL(t *testing.T) {
	store := NewLRUStore[string, string](2, 2*time.Second, 1*time.Second)

	// Test TTL expiration
	store.Put("key1", "value1")
	time.Sleep(3 * time.Second) // Wait for TTL to expire

	if _, ok := store.Get("key1"); ok {
		t.Errorf("expected key1 to be expired")
	}
}

func TestLRUStore_UpdateExistingKey(t *testing.T) {
	store := NewLRUStore[string, string](2, 5*time.Second, 1*time.Second)

	// Test updating an existing key
	store.Put("key1", "value1")
	store.Put("key1", "value2")

	if val, ok := store.Get("key1"); !ok || val != "value2" {
		t.Errorf("expected value2, got %v", val)
	}
}

func TestLRUStore_ConcurrentEvictions(t *testing.T) {
	store := NewLRUStore[string, string](5, 5*time.Second, 1*time.Second) // Small capacity to force evictions

	const numGoroutines = 10
	const numOperations = 20
	done := make(chan error, numGoroutines)
	defer close(done)

	for i := range numGoroutines {
		go func(id int) {
			for j := range numOperations {
				key := fmt.Sprintf("goroutine%d_key%d", id, j)
				value := fmt.Sprintf("value%d", j)

				store.Put(key, value)

				if store.Len() > store.cap {
					done <- fmt.Errorf("store exceeded capacity: expected at most %d, got %d", store.cap, len(store.freeTable))
					break
				}
			}
			done <- nil
		}(i)
	}

	for range numGoroutines {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
