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
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Test constants
const (
	testModelName = "test-model"
	testPod1Name  = "pod1"
)

// makeTokens creates a byte slice of the specified length filled with sequential values
func makeTokens(length int) []byte {
	tokens := make([]byte, length)
	for i := 0; i < length; i++ {
		tokens[i] = byte(i % 256)
	}
	return tokens
}

func TestNewSyncPrefixHashTable(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	if table.seed == 0 {
		t.Error("seed should not be zero")
	}

	if table.maxContexts != maxContexts {
		t.Errorf("expected maxContexts %d, got %d", maxContexts, table.maxContexts)
	}

	if table.contextCount.Load() != 0 {
		t.Error("initial context count should be 0")
	}
}

func TestGetPrefixHashes(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	testCases := []struct {
		name     string
		tokens   []byte
		expected int
	}{
		{
			name:     "empty tokens",
			tokens:   []byte{},
			expected: 0,
		},
		{
			name:     "single block",
			tokens:   makeTokens(16), // 16 bytes = 1 block with default block size
			expected: 1,
		},
		{
			name:     "multiple blocks",
			tokens:   makeTokens(48), // 48 bytes = 3 blocks with block size 16
			expected: 3,
		},
		{
			name:     "incomplete last block",
			tokens:   makeTokens(23), // 23 bytes = 1 complete block, incomplete block ignored
			expected: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hashes := table.GetPrefixHashes(tc.tokens)
			if len(hashes) != tc.expected {
				t.Errorf("expected %d hashes, got %d", tc.expected, len(hashes))
			}

			// Verify hashes are deterministic
			hashes2 := table.GetPrefixHashes(tc.tokens)
			if !reflect.DeepEqual(hashes, hashes2) {
				t.Error("hashes should be deterministic")
			}
		})
	}
}

func TestPrefixHashChaining(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Test that prefix hashes are chained correctly
	// With block size 16, we need 32 bytes for 2 blocks
	tokens := makeTokens(32)
	hashes := table.GetPrefixHashes(tokens)

	if len(hashes) != 2 {
		t.Fatalf("expected 2 hashes, got %d", len(hashes))
	}

	// Verify that changing early tokens affects all subsequent hashes
	tokens2 := make([]byte, 32)
	copy(tokens2, tokens)
	tokens2[0] = tokens2[0] + 1 // Changed first byte
	hashes2 := table.GetPrefixHashes(tokens2)

	if hashes[0] == hashes2[0] {
		t.Error("changing first block should change its hash")
	}

	if hashes[1] == hashes2[1] {
		t.Error("changing first block should affect second block hash due to chaining")
	}
}

func TestMatchPrefix(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(-1)
	tokens := makeTokens(32) // 32 bytes = 2 blocks with block size 16
	podName := testPod1Name

	// Initially, no matches
	readyPods := map[string]struct{}{podName: {}}
	matches, hashes := table.MatchPrefix(modelName, loraID, tokens, readyPods)
	if len(matches) != 0 {
		t.Error("should have no matches initially")
	}
	if len(hashes) != 2 {
		t.Errorf("expected 2 hashes, got %d", len(hashes))
	}

	// Add prefixes
	err := table.AddPrefix(modelName, loraID, podName, hashes)
	if err != nil {
		t.Fatalf("failed to add prefix: %v", err)
	}

	// Now should match
	matches, _ = table.MatchPrefix(modelName, loraID, tokens, readyPods)
	if len(matches) != 1 {
		t.Errorf("expected 1 match, got %d", len(matches))
	}
	if matches[podName] != 100 {
		t.Errorf("expected 100%% match, got %d%%", matches[podName])
	}

	// Test partial match - first block only
	partialTokens := makeTokens(16) // 16 bytes = 1 block
	partialHashes := table.GetPrefixHashes(partialTokens)
	matches, returnedHashes := table.MatchPrefix(modelName, loraID, partialTokens, readyPods)

	// Debug output
	t.Logf("Original tokens: %v (len=%d), hashes: %v", tokens, len(tokens), hashes)
	t.Logf("Partial tokens: %v (len=%d), hashes: %v", partialTokens, len(partialTokens), partialHashes)
	t.Logf("Returned hashes from MatchPrefix: %v", returnedHashes)
	t.Logf("Match percentage: %d%%", matches[podName])

	// Since partialTokens is 16 bytes (1 block) and that block is stored,
	// we expect 100% match (all blocks in the partial request matched)
	if matches[podName] != 100 {
		t.Errorf("expected 100%% match for partial tokens (all blocks matched), got %d%%", matches[podName])
	}
}

func TestProcessBlockStored(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(123)
	sourcePod := testPod1Name

	// Test validation
	t.Run("empty event", func(t *testing.T) {
		err := table.ProcessBlockStored(BlockStored{})
		if err != nil {
			t.Error("should handle empty event gracefully")
		}
	})

	t.Run("mismatched lengths", func(t *testing.T) {
		event := BlockStored{
			BlockHashes: []int64{1, 2},
			Tokens:      [][]byte{{1, 2, 3, 4}},
		}
		err := table.ProcessBlockStored(event)
		if err == nil {
			t.Error("should return error for mismatched lengths")
		}
	})

	// Create test event
	tokens1 := []byte{1, 2, 3, 4}
	tokens2 := []byte{5, 6, 7, 8}
	parentHash := int64(999)

	event := BlockStored{
		BlockHashes:     []int64{1001, 1002},
		ParentBlockHash: &parentHash,
		Tokens:          [][]byte{tokens1, tokens2},
		ModelName:       modelName,
		LoraID:          loraID,
		SourcePod:       sourcePod,
	}

	// Process event
	err := table.ProcessBlockStored(event)
	if err != nil {
		t.Fatalf("failed to process block stored: %v", err)
	}

	// Verify context was created
	if table.contextCount.Load() != 1 {
		t.Error("context count should be 1")
	}

	// Verify blocks were stored
	ctx := ModelContext{ModelName: modelName, LoraID: loraID}
	value, exists := table.contextMap.Load(ctx)
	if !exists {
		t.Fatal("context should exist")
	}

	contextData := value.(*ContextData)

	// Check hash mapping
	contextData.mappingMu.RLock()
	if len(contextData.hashMapping.engineToAibrix) != 2 {
		t.Errorf("expected 2 hash mappings, got %d", len(contextData.hashMapping.engineToAibrix))
	}
	contextData.mappingMu.RUnlock()

	// Check prefix store
	contextData.prefixMu.RLock()
	if contextData.prefixStore.totalPrefixes != 2 {
		t.Errorf("expected 2 prefixes, got %d", contextData.prefixStore.totalPrefixes)
	}
	contextData.prefixMu.RUnlock()

	// Test idempotency - process same event again
	err = table.ProcessBlockStored(event)
	if err != nil {
		t.Fatalf("failed to process block stored again: %v", err)
	}

	// Should still have same counts
	contextData.mappingMu.RLock()
	if len(contextData.hashMapping.engineToAibrix) != 2 {
		t.Error("hash mapping count should remain 2")
	}
	contextData.mappingMu.RUnlock()
}

func TestProcessBlockRemoved(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(-1)
	sourcePod := testPod1Name

	// First, store some blocks
	storeEvent := BlockStored{
		BlockHashes: []int64{2001, 2002, 2003},
		Tokens:      [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10, 11, 12}},
		ModelName:   modelName,
		LoraID:      loraID,
		SourcePod:   sourcePod,
	}

	err := table.ProcessBlockStored(storeEvent)
	if err != nil {
		t.Fatalf("failed to store blocks: %v", err)
	}

	// Now remove some blocks
	removeEvent := BlockRemoved{
		BlockHashes: []int64{2001, 2003},
		ModelName:   modelName,
		LoraID:      loraID,
	}

	err = table.ProcessBlockRemoved(removeEvent)
	if err != nil {
		t.Fatalf("failed to remove blocks: %v", err)
	}

	// Verify blocks were removed
	ctx := ModelContext{ModelName: modelName, LoraID: -1}
	value, _ := table.contextMap.Load(ctx)
	contextData := value.(*ContextData)

	contextData.mappingMu.RLock()

	// Should have only 1 block left (2002)
	if len(contextData.hashMapping.engineToAibrix) != 1 {
		t.Errorf("expected 1 hash mapping, got %d", len(contextData.hashMapping.engineToAibrix))
	}

	_, exists := contextData.hashMapping.engineToAibrix[2002]
	if !exists {
		t.Error("block 2002 should still exist")
	}

	contextData.mappingMu.RUnlock()
}

func TestConcurrentOperations(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	numGoroutines := 10
	numOperations := 100
	var wg sync.WaitGroup

	// Concurrent writes to different contexts
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			modelName := fmt.Sprintf("model-%d", id)
			loraID := int64(id)
			podName := fmt.Sprintf("pod-%d", id)

			for j := 0; j < numOperations; j++ {
				tokens := []byte{byte(j), byte(j + 1), byte(j + 2), byte(j + 3)}
				hashes := table.GetPrefixHashes(tokens)

				err := table.AddPrefix(modelName, loraID, podName, hashes)
				if err != nil {
					t.Errorf("goroutine %d: failed to add prefix: %v", id, err)
				}
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			modelName := fmt.Sprintf("model-%d", id)
			loraID := int64(id)
			podName := fmt.Sprintf("pod-%d", id)
			readyPods := map[string]struct{}{podName: {}}

			for j := 0; j < numOperations; j++ {
				tokens := []byte{byte(j), byte(j + 1), byte(j + 2), byte(j + 3)}
				table.MatchPrefix(modelName, loraID, tokens, readyPods)
			}
		}(i)
	}

	wg.Wait()

	// Verify all contexts were created
	expectedContexts := int32(numGoroutines)
	if table.contextCount.Load() != expectedContexts {
		t.Errorf("expected %d contexts, got %d", expectedContexts, table.contextCount.Load())
	}
}

func TestEviction(t *testing.T) {
	// Create table with short eviction settings
	table := &SyncPrefixHashTable{
		seed:                  12345,
		maxContexts:           maxContexts,
		maxPrefixesPerContext: maxPrefixesPerContext,
		blockSize:             prefixCacheBlockSize,
		evictionInterval:      100 * time.Millisecond,
		evictionDuration:      200 * time.Millisecond,
		stopCh:                make(chan struct{}),
	}

	// Start eviction worker
	table.wg.Add(1)
	go table.evictionWorker()
	defer table.Close()

	// Add a context
	modelName := testModelName
	loraID := int64(-1)
	podName := testPod1Name
	tokens := []byte{1, 2, 3, 4}
	hashes := table.GetPrefixHashes(tokens)

	err := table.AddPrefix(modelName, loraID, podName, hashes)
	if err != nil {
		t.Fatalf("failed to add prefix: %v", err)
	}

	// Context should exist
	if table.contextCount.Load() != 1 {
		t.Error("context count should be 1")
	}

	// Manually set the last access time to be expired
	ctx := ModelContext{ModelName: modelName, LoraID: loraID}
	if value, exists := table.contextMap.Load(ctx); exists {
		contextData := value.(*ContextData)
		// Set last access to 5 minutes ago (well beyond eviction duration)
		contextData.prefixStore.lastAccess.Store(time.Now().Add(-5 * time.Minute).Unix())
	}

	// Force eviction
	table.performEviction()

	// Context should be evicted
	if table.contextCount.Load() != 0 {
		t.Errorf("context should have been evicted, but count is %d", table.contextCount.Load())

		// Debug: check if context still exists
		if _, exists := table.contextMap.Load(ctx); exists {
			t.Error("context still exists in map")
		}
	}
}

func TestMaxContextsLimit(t *testing.T) {
	// Create table with low limit
	table := NewSyncPrefixHashTable()
	table.maxContexts = 5
	defer table.Close()

	// Add more contexts than the limit
	for i := 0; i < 10; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		loraID := int64(i)
		podName := fmt.Sprintf("pod-%d", i)
		tokens := []byte{1, 2, 3, 4}
		hashes := table.GetPrefixHashes(tokens)

		err := table.AddPrefix(modelName, loraID, podName, hashes)
		if err != nil {
			t.Fatalf("failed to add prefix: %v", err)
		}
	}

	// Should have created all contexts (eviction is async)
	if table.contextCount.Load() != 10 {
		t.Errorf("expected 10 contexts, got %d", table.contextCount.Load())
	}
}

func TestRemovePrefix(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(-1)
	pod1 := "pod1"
	pod2 := "pod2"
	tokens := makeTokens(32) // 32 bytes = 2 blocks with block size 16
	hashes := table.GetPrefixHashes(tokens)

	// Add prefixes for two pods
	_ = table.AddPrefix(modelName, loraID, pod1, hashes)
	_ = table.AddPrefix(modelName, loraID, pod2, hashes)

	// Both pods should match
	readyPods := map[string]struct{}{pod1: {}, pod2: {}}
	matches, _ := table.MatchPrefix(modelName, loraID, tokens, readyPods)
	if len(matches) != 2 {
		t.Errorf("expected 2 matches, got %d", len(matches))
	}

	// Remove pod1
	err := table.RemovePrefix(modelName, loraID, pod1)
	if err != nil {
		t.Fatalf("failed to remove prefix: %v", err)
	}

	// Only pod2 should match now
	matches, _ = table.MatchPrefix(modelName, loraID, tokens, readyPods)
	if len(matches) != 1 {
		t.Errorf("expected 1 match, got %d", len(matches))
	}
	if _, exists := matches[pod2]; !exists {
		t.Error("pod2 should still match")
	}
}

func TestProcessAllBlocksCleared(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Test empty implementation
	event := AllBlocksCleared{}
	err := table.ProcessAllBlocksCleared(event)
	if err != nil {
		t.Errorf("ProcessAllBlocksCleared should not return error: %v", err)
	}
}

func TestReverseIndex(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Add blocks to multiple contexts
	sharedBlock := int64(5000)
	for i := 0; i < 3; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		event := BlockStored{
			BlockHashes: []int64{sharedBlock, int64(1000 + i)},
			Tokens:      [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}},
			ModelName:   modelName,
			LoraID:      -1,
			SourcePod:   "pod1",
		}
		err := table.ProcessBlockStored(event)
		if err != nil {
			t.Fatalf("failed to process block: %v", err)
		}
	}

	// Verify reverse index
	table.blockIndexMu.RLock()
	contexts := table.blockIndex[sharedBlock]
	if len(contexts) != 3 {
		t.Errorf("expected 3 contexts for shared block, got %d", len(contexts))
	}
	table.blockIndexMu.RUnlock()

	// Remove shared block from each context separately
	for i := 0; i < 3; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		removeEvent := BlockRemoved{
			BlockHashes: []int64{sharedBlock},
			ModelName:   modelName,
			LoraID:      -1,
		}
		err := table.ProcessBlockRemoved(removeEvent)
		if err != nil {
			t.Fatalf("failed to remove block: %v", err)
		}
	}

	// Verify block was removed from all contexts
	for i := 0; i < 3; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		ctx := ModelContext{ModelName: modelName, LoraID: -1}
		value, _ := table.contextMap.Load(ctx)
		contextData := value.(*ContextData)

		contextData.mappingMu.RLock()
		if _, exists := contextData.hashMapping.engineToAibrix[sharedBlock]; exists {
			t.Errorf("shared block should be removed from context %s", modelName)
		}
		contextData.mappingMu.RUnlock()
	}
}

func TestScheduleEviction(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Set eviction flag
	table.scheduleEviction()

	// Verify flag is set
	if !table.evictionNeeded.Load() {
		t.Error("eviction flag should be set")
	}

	// Wait for eviction to be triggered (1 second check interval + buffer)
	time.Sleep(1500 * time.Millisecond)

	// Flag should be cleared after eviction runs
	if table.evictionNeeded.Load() {
		t.Error("eviction flag should be cleared after eviction")
	}

	// Additionally verify eviction can run
	if table.evictionRunning.Load() {
		t.Error("eviction should not still be running")
	}
}

func TestGracefulShutdown(t *testing.T) {
	table := NewSyncPrefixHashTable()

	// Add some contexts
	for i := 0; i < 5; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		tokens := []byte{1, 2, 3, 4}
		hashes := table.GetPrefixHashes(tokens)
		_ = table.AddPrefix(modelName, int64(i), "pod1", hashes)
	}

	// Close should complete within timeout
	start := time.Now()
	table.Close()
	elapsed := time.Since(start)

	if elapsed > 6*time.Second {
		t.Errorf("shutdown took too long: %v", elapsed)
	}
}

// TestConcurrentEventProcessingFromMultiplePods tests processing events from multiple pods concurrently
func TestConcurrentEventProcessingFromMultiplePods(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(-1)
	numPods := 10
	numEventsPerPod := 100
	var wg sync.WaitGroup

	// Concurrent event processing from multiple pods
	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func(podID int) {
			defer wg.Done()

			podName := fmt.Sprintf("pod-%d", podID)

			for j := 0; j < numEventsPerPod; j++ {
				// Mix of store and remove events
				if j%3 == 0 {
					// Store event
					blockHash := int64(podID*1000 + j)
					event := BlockStored{
						BlockHashes: []int64{blockHash},
						Tokens:      [][]byte{{byte(j), byte(j), byte(j), byte(j)}},
						ModelName:   modelName,
						LoraID:      loraID,
						SourcePod:   podName,
					}

					err := table.ProcessBlockStored(event)
					if err != nil {
						t.Errorf("pod %d: failed to process block stored: %v", podID, err)
					}
				} else if j%3 == 1 && j > 0 {
					// Remove event for previously stored block
					blockHash := int64(podID*1000 + j - 1)
					event := BlockRemoved{
						BlockHashes: []int64{blockHash},
						ModelName:   modelName,
						LoraID:      loraID,
					}

					err := table.ProcessBlockRemoved(event)
					if err != nil {
						t.Errorf("pod %d: failed to process block removed: %v", podID, err)
					}
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify context exists
	ctx := ModelContext{ModelName: modelName, LoraID: loraID}
	value, exists := table.contextMap.Load(ctx)
	if !exists {
		t.Fatal("context should exist after concurrent operations")
	}

	contextData := value.(*ContextData)

	// Check that we have some blocks remaining
	contextData.mappingMu.RLock()
	blockCount := len(contextData.hashMapping.engineToAibrix)
	contextData.mappingMu.RUnlock()

	if blockCount == 0 {
		t.Error("should have some blocks after concurrent operations")
	}

	t.Logf("Final block count after concurrent operations: %d", blockCount)
}

// TestEventValidation tests validation of events
func TestEventValidation(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	testCases := []struct {
		name      string
		event     BlockStored
		wantError bool
	}{
		{
			name: "valid event",
			event: BlockStored{
				BlockHashes: []int64{1, 2, 3},
				Tokens:      [][]byte{{1}, {2}, {3}},
				ModelName:   "model",
				LoraID:      -1,
				SourcePod:   "pod1",
			},
			wantError: false,
		},
		{
			name: "empty block hashes",
			event: BlockStored{
				BlockHashes: []int64{},
				Tokens:      [][]byte{},
				ModelName:   "model",
				LoraID:      -1,
				SourcePod:   "pod1",
			},
			wantError: false, // Empty is allowed
		},
		{
			name: "mismatched lengths",
			event: BlockStored{
				BlockHashes: []int64{1, 2},
				Tokens:      [][]byte{{1}}, // Length mismatch
				ModelName:   "model",
				LoraID:      -1,
				SourcePod:   "pod1",
			},
			wantError: true,
		},
		{
			name: "nil tokens",
			event: BlockStored{
				BlockHashes: []int64{1, 2},
				Tokens:      nil,
				ModelName:   "model",
				LoraID:      -1,
				SourcePod:   "pod1",
			},
			wantError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := table.ProcessBlockStored(tc.event)
			if tc.wantError && err == nil {
				t.Error("expected error but got none")
			}
			if !tc.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestMemoryCleanup tests that memory is properly cleaned up
func TestMemoryCleanup(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	modelName := testModelName
	loraID := int64(-1)

	// Add and remove many blocks
	for i := 0; i < 1000; i++ {
		// Store block
		storeEvent := BlockStored{
			BlockHashes: []int64{int64(i)},
			Tokens:      [][]byte{{byte(i), byte(i), byte(i), byte(i)}},
			ModelName:   modelName,
			LoraID:      loraID,
			SourcePod:   "pod1",
		}

		err := table.ProcessBlockStored(storeEvent)
		if err != nil {
			t.Fatalf("failed to store block %d: %v", i, err)
		}

		// Immediately remove it
		removeEvent := BlockRemoved{
			BlockHashes: []int64{int64(i)},
			ModelName:   modelName,
			LoraID:      loraID,
		}

		err = table.ProcessBlockRemoved(removeEvent)
		if err != nil {
			t.Fatalf("failed to remove block %d: %v", i, err)
		}
	}

	// Verify context still exists but is empty
	ctx := ModelContext{ModelName: modelName, LoraID: loraID}
	value, exists := table.contextMap.Load(ctx)
	if !exists {
		t.Fatal("context should still exist")
	}

	contextData := value.(*ContextData)

	// Check that no blocks remain
	contextData.mappingMu.RLock()
	if len(contextData.hashMapping.engineToAibrix) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(contextData.hashMapping.engineToAibrix))
	}
	contextData.mappingMu.RUnlock()

	// Check reverse index is clean
	table.blockIndexMu.RLock()
	for blockHash, contexts := range table.blockIndex {
		if len(contexts) > 0 {
			t.Errorf("block %d still has %d contexts in reverse index", blockHash, len(contexts))
		}
	}
	table.blockIndexMu.RUnlock()
}
