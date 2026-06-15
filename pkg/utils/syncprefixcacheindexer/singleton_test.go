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

package syncprefixcacheindexer

import (
	"sync"
	"testing"
)

// TestGetSharedSyncPrefixHashTable verifies that the singleton pattern works correctly
func TestGetSharedSyncPrefixHashTable(t *testing.T) {
	// Get the shared instance multiple times
	instance1 := GetSharedSyncPrefixHashTable()
	instance2 := GetSharedSyncPrefixHashTable()
	instance3 := GetSharedSyncPrefixHashTable()

	// Verify they are all the same instance (same memory address)
	if instance1 != instance2 {
		t.Errorf("instance1 and instance2 should be the same")
	}
	if instance2 != instance3 {
		t.Errorf("instance2 and instance3 should be the same")
	}

	// Verify the instance is not nil
	if instance1 == nil {
		t.Errorf("shared instance should not be nil")
	}
}

// TestGetSharedSyncPrefixHashTableConcurrency verifies thread-safety of singleton initialization
func TestGetSharedSyncPrefixHashTableConcurrency(t *testing.T) {
	// Close the existing singleton instance first to avoid goroutine leak
	if sharedSyncInstance != nil {
		sharedSyncInstance.Close()
	}

	// Reset the singleton for this test (using package-level access)
	// Note: This is only for testing and should not be done in production code
	sharedSyncOnce = sync.Once{}
	sharedSyncInstance = nil

	const numGoroutines = 100
	instances := make([]*SyncPrefixHashTable, numGoroutines)
	var wg sync.WaitGroup

	// Launch multiple goroutines trying to get the instance concurrently
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			instances[index] = GetSharedSyncPrefixHashTable()
		}(i)
	}

	wg.Wait()

	// Verify all goroutines got the same instance
	firstInstance := instances[0]
	if firstInstance == nil {
		t.Fatal("first instance should not be nil")
	}

	for i := 1; i < numGoroutines; i++ {
		if instances[i] != firstInstance {
			t.Errorf("instance %d is different from first instance", i)
		}
	}
}

// TestSharedInstanceFunctionality verifies the shared instance works correctly
func TestSharedInstanceFunctionality(t *testing.T) {
	// Get shared instance
	indexer := GetSharedSyncPrefixHashTable()
	if indexer == nil {
		t.Fatal("shared indexer should not be nil")
	}

	// Test basic functionality with correct API
	modelName := "test-model"
	loraID := int64(-1)
	podName := "test-pod"
	prefixHashes := []uint64{12345, 67890}

	// Add prefixes
	err := indexer.AddPrefix(modelName, loraID, podName, prefixHashes)
	if err != nil {
		t.Fatalf("AddPrefix failed: %v", err)
	}

	// Get the same instance again and verify it's the same object
	indexer2 := GetSharedSyncPrefixHashTable()
	if indexer != indexer2 {
		t.Error("expected indexer and indexer2 to be the same instance")
	}

	// Verify functionality by calling AddPrefix again on the second reference
	err = indexer2.AddPrefix(modelName, loraID, "another-pod", prefixHashes)
	if err != nil {
		t.Fatalf("AddPrefix on second instance failed: %v", err)
	}
}
