// Copyright 2025 AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build zmq
// +build zmq

package cache_test

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
	"github.com/vllm-project/aibrix/pkg/utils"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

// Test that the adapter correctly implements the interfaces
func TestStoreProviderAdapter(t *testing.T) {
	// Create a test store
	store := cache.NewForTest()

	// Get the adapter
	podProvider, syncProvider := cache.NewStoreProviderAdapter(store)

	// Verify interfaces are implemented
	var _ kvevent.PodProvider = podProvider
	var _ kvevent.SyncIndexProvider = syncProvider

	// Test basic operations
	ctx := context.Background()

	// Test GetPod when pod doesn't exist
	_, exists := podProvider.GetPod(ctx, "nonexistent")
	if exists {
		t.Error("Expected pod not to exist")
	}

	// Test GetSyncIndexer when not initialized
	_, err := syncProvider.GetSyncIndexer(ctx)
	if err != kvevent.ErrIndexerNotInitialized {
		t.Errorf("Expected ErrIndexerNotInitialized, got: %v", err)
	}
}

// Test pod provider functionality
func TestStoreProviderAdapterPodOperations(t *testing.T) {
	// Create store with test data
	store := cache.NewForTest()

	// Create a test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "test-model",
			},
		},
		Status: v1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}

	// Initialize store with pod
	store = cache.InitWithPods(store, []*v1.Pod{pod}, "test-model")

	// Get adapter
	podProvider, _ := cache.NewStoreProviderAdapter(store)

	ctx := context.Background()

	// Test GetPod
	podKey := utils.GeneratePodKey("default", "test-pod")
	podInfo, exists := podProvider.GetPod(ctx, podKey)

	if !exists {
		t.Fatal("Expected pod to exist")
	}

	if podInfo.Name != "test-pod" {
		t.Errorf("Expected pod name 'test-pod', got %s", podInfo.Name)
	}

	if podInfo.Namespace != "default" {
		t.Errorf("Expected namespace 'default', got %s", podInfo.Namespace)
	}

	if podInfo.PodIP != "10.0.0.1" {
		t.Errorf("Expected pod IP '10.0.0.1', got %s", podInfo.PodIP)
	}

	if podInfo.ModelName != "test-model" {
		t.Errorf("Expected model name 'test-model', got %s", podInfo.ModelName)
	}

	// Test RangePods
	count := 0
	err := podProvider.RangePods(ctx, func(key string, pod *kvevent.PodInfo) bool {
		count++
		if key != podKey {
			t.Errorf("Expected key %s, got %s", podKey, key)
		}
		return true
	})

	if err != nil {
		t.Errorf("RangePods failed: %v", err)
	}

	if count != 1 {
		t.Errorf("Expected 1 pod in range, got %d", count)
	}
}

// Test context cancellation handling
func TestStoreProviderAdapterContextCancellation(t *testing.T) {
	store := cache.NewForTest()
	podProvider, syncProvider := cache.NewStoreProviderAdapter(store)

	// Create cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Test GetPod with cancelled context
	_, exists := podProvider.GetPod(ctx, "any-key")
	if exists {
		t.Error("Expected GetPod to fail with cancelled context")
	}

	// Test RangePods with cancelled context
	// Note: If there are no pods, the Range function won't execute the callback
	// So the context check won't happen. This is expected behavior.
	err := podProvider.RangePods(ctx, func(key string, pod *kvevent.PodInfo) bool {
		t.Fatal("Callback should not be called with cancelled context")
		return true
	})

	// With no pods, err will be nil as the Range never executes
	if err != nil {
		t.Errorf("Expected nil error with empty store, got: %v", err)
	}

	// Test GetSyncIndexer with cancelled context
	_, err = syncProvider.GetSyncIndexer(ctx)
	if err == nil || err.Error() != "context canceled" {
		t.Errorf("Expected context canceled error, got: %v", err)
	}
}

// Test context cancellation with pods present
func TestStoreProviderAdapterContextCancellationWithPods(t *testing.T) {
	store := cache.NewForTest()

	// Add a test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{constants.ModelLabelName: "test-model"},
		},
		Status: v1.PodStatus{PodIP: "10.0.0.1"},
	}
	store = cache.InitWithPods(store, []*v1.Pod{pod}, "test-model")

	podProvider, _ := cache.NewStoreProviderAdapter(store)

	// Create cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Test RangePods with cancelled context and pods present
	callbackCalled := false
	err := podProvider.RangePods(ctx, func(key string, pod *kvevent.PodInfo) bool {
		callbackCalled = true
		return true
	})

	if callbackCalled {
		t.Error("Callback should not be called with cancelled context")
	}

	if err == nil || err.Error() != "context canceled" {
		t.Errorf("Expected context canceled error, got: %v", err)
	}
}

// Test that the adapter pattern correctly decouples packages
func TestAdapterDecoupling(t *testing.T) {
	// This test verifies that the adapter successfully decouples
	// the kvevent package from the cache implementation details

	store := cache.NewForTest()
	podProvider, syncProvider := cache.NewStoreProviderAdapter(store)

	// The fact that we can use these interfaces without importing
	// any cache-specific types (except for the initial setup) proves
	// that the decoupling is working correctly

	ctx := context.Background()

	// Use only kvevent interfaces
	var pp kvevent.PodProvider = podProvider
	var sp kvevent.SyncIndexProvider = syncProvider

	// Operations only use kvevent types
	_, _ = pp.GetPod(ctx, "test")
	_, _ = sp.GetSyncIndexer(ctx)

	// Success - no compile errors means decoupling works
}

// Test that syncIndexerAdapter is not available when indexer is not initialized
func TestSyncIndexerAdapterNotInitialized(t *testing.T) {
	// Create a store without sync indexer initialized
	store := cache.NewForTest()

	// Get adapter
	_, syncProvider := cache.NewStoreProviderAdapter(store)

	ctx := context.Background()
	_, err := syncProvider.GetSyncIndexer(ctx)

	// Should return error when sync indexer is not initialized
	if err != kvevent.ErrIndexerNotInitialized {
		t.Errorf("Expected ErrIndexerNotInitialized, got: %v", err)
	}
}

// Test syncIndexerAdapter with real SyncPrefixHashTable
func TestSyncIndexerAdapterWithRealIndexer(t *testing.T) {
	// Create store with real sync indexer
	store := cache.NewForTest()
	realIndexer := syncindexer.NewSyncPrefixHashTable()
	cache.SetSyncIndexerForTest(store, realIndexer)

	// Get adapter
	_, syncProvider := cache.NewStoreProviderAdapter(store)
	syncIndexer, err := syncProvider.GetSyncIndexer(context.Background())
	if err != nil {
		t.Fatalf("Failed to get sync indexer: %v", err)
	}

	// Test that the adapter correctly passes through all operations
	ctx := context.Background()

	// Test ProcessBlockStored - verifies type conversion works
	// Note: Real indexer requires tokens length to match block hashes length
	blockStoredEvent := kvevent.BlockStoredEvent{
		BlockHashes:     []int64{5001, 5002, 5003},
		ModelName:       "real-test-model",
		LoraID:          789,
		SourcePod:       "test-ns/test-pod",
		ParentBlockHash: &[]int64{5000}[0],
		Tokens:          [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10, 11, 12}},
	}

	// Should not error with real indexer
	err = syncIndexer.ProcessBlockStored(ctx, blockStoredEvent)
	if err != nil {
		t.Errorf("ProcessBlockStored failed with real indexer: %v", err)
	}

	// Test ProcessBlockRemoved
	blockRemovedEvent := kvevent.BlockRemovedEvent{
		BlockHashes: []int64{5001},
		ModelName:   "real-test-model",
		LoraID:      789,
		SourcePod:   "test-ns/test-pod",
	}

	err = syncIndexer.ProcessBlockRemoved(ctx, blockRemovedEvent)
	if err != nil {
		t.Errorf("ProcessBlockRemoved failed with real indexer: %v", err)
	}

	// Test RemovePrefix
	err = syncIndexer.RemovePrefix(ctx, "real-test-model", 789, "test-ns/test-pod")
	if err != nil {
		t.Errorf("RemovePrefix failed with real indexer: %v", err)
	}
}
