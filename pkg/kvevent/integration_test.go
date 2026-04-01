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

package kvevent_test

import (
	"context"
	"encoding/binary"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

// Helper function to convert int32 slice to bytes (big-endian)
func int32SliceToBytes(tokens []int32) []byte {
	result := make([]byte, len(tokens)*4)
	for i, token := range tokens {
		binary.BigEndian.PutUint32(result[i*4:], uint32(token))
	}
	return result
}

// TestIntegrationEventHandlerWithRealComponents tests event handling with real components
// This ensures we're not just testing mocks but actual integration between components
func TestIntegrationEventHandlerWithRealComponents(t *testing.T) {
	// Skip if not in integration test mode
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create a real store with real sync indexer
	store := cache.NewForTest()

	// Initialize with real sync prefix indexer (not a mock!)
	syncIndexer := syncindexer.NewSyncPrefixHashTable()

	// Use reflection to set the private field (for testing only)
	// In real code, this would be set by InitSyncResources
	cache.SetSyncIndexerForTest(store, syncIndexer)

	// Add a test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "test-model",
				constants.LoraIDLabel:    "123",
			},
		},
		Status: v1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}
	store = cache.InitWithPods(store, []*v1.Pod{pod}, "test-model")

	// Create adapters
	podProvider, syncProvider := cache.NewStoreProviderAdapter(store)

	// Create real manager with real components
	manager := kvevent.NewManager(podProvider, syncProvider)

	// Create handler through internal API
	handler := kvevent.NewEventHandlerForTest(manager, "default/test-pod", "test-model", 123)

	// Test BlockStored event
	// Note: tokens must match block hashes length
	storedEvent := &kvcache.BlockStoredEvent{
		BlockHashes:     []int64{1001, 1002, 1003},
		ParentBlockHash: &[]int64{1000}[0],
		TokenIDs: [][]byte{
			int32SliceToBytes([]int32{1, 2, 3}),
			int32SliceToBytes([]int32{4, 5, 6}),
			int32SliceToBytes([]int32{7, 8, 9}),
		},
	}

	err := handler.HandleEvent(storedEvent)
	if err != nil {
		t.Errorf("HandleEvent failed with real components: %v", err)
	}

	// Verify the event was actually processed by checking the indexer state
	// This would require adding getter methods or checking side effects

	// Test BlockRemoved event
	removedEvent := &kvcache.BlockRemovedEvent{
		BlockHashes: []int64{1001},
	}

	err = handler.HandleEvent(removedEvent)
	if err != nil {
		t.Errorf("HandleEvent failed for removal: %v", err)
	}

	// Test that the real sync indexer received and processed the events
	// In a real integration test, we would verify the state changes
}

// TestIntegrationSyncIndexerAdapterWithRealIndexer tests the adapter with real indexer
func TestIntegrationSyncIndexerAdapterWithRealIndexer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create real components
	store := cache.NewForTest()
	realIndexer := syncindexer.NewSyncPrefixHashTable()
	cache.SetSyncIndexerForTest(store, realIndexer)

	// Get adapter
	_, syncProvider := cache.NewStoreProviderAdapter(store)

	ctx := context.Background()
	syncIndexer, err := syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		t.Fatalf("Failed to get sync indexer: %v", err)
	}

	// Test real type conversion and processing
	blockStoredEvent := kvevent.BlockStoredEvent{
		BlockHashes:     []int64{2001, 2002, 2003},
		ModelName:       "integration-test-model",
		LoraID:          456,
		SourcePod:       "default/integration-pod",
		ParentBlockHash: &[]int64{2000}[0],
		Tokens:          [][]byte{{0, 0, 0, 10}, {0, 0, 0, 20}, {0, 0, 0, 30}},
	}

	// This will go through the adapter to the real indexer
	err = syncIndexer.ProcessBlockStored(ctx, blockStoredEvent)
	if err != nil {
		t.Errorf("ProcessBlockStored failed with real indexer: %v", err)
	}

	// Test removal
	blockRemovedEvent := kvevent.BlockRemovedEvent{
		BlockHashes: []int64{2001},
		ModelName:   "integration-test-model",
		LoraID:      456,
		SourcePod:   "default/integration-pod",
	}

	err = syncIndexer.ProcessBlockRemoved(ctx, blockRemovedEvent)
	if err != nil {
		t.Errorf("ProcessBlockRemoved failed with real indexer: %v", err)
	}

	// Test prefix removal
	err = syncIndexer.RemovePrefix(ctx, "integration-test-model", 456, "default/integration-pod")
	if err != nil {
		t.Errorf("RemovePrefix failed with real indexer: %v", err)
	}
}

// TestIntegrationFullEventFlow tests the complete event flow from pod to indexer
func TestIntegrationFullEventFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Enable KV sync for this test
	t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
	t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")

	// Create real store with real components
	store := cache.NewForTest()
	realIndexer := syncindexer.NewSyncPrefixHashTable()
	cache.SetSyncIndexerForTest(store, realIndexer)

	// Add test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flow-test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName:       "flow-test-model",
				constants.KVEventsEnabledLabel: "true",
				constants.LoraIDLabel:          "789",
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.2",
		},
	}

	// Initialize store with pod
	store = cache.InitWithPods(store, []*v1.Pod{pod}, "flow-test-model")

	// Create KV event manager using the old API for compatibility
	kvManager := cache.NewKVEventManager(store)

	// Start the manager
	err := kvManager.Start()
	if err != nil {
		t.Fatalf("Failed to start KV event manager: %v", err)
	}
	defer kvManager.Stop()

	// Simulate pod lifecycle
	kvManager.OnPodAdd(pod)

	// Update pod (IP change)
	updatedPod := pod.DeepCopy()
	updatedPod.Status.PodIP = "10.0.0.3"
	kvManager.OnPodUpdate(pod, updatedPod)

	// Delete pod
	kvManager.OnPodDelete(updatedPod)

	// Verify the complete flow worked without errors
	// In a real system, we would verify side effects in Redis or state changes
}
