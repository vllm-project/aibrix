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

package kvevent

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
)

// mockSyncIndexerWithErrors allows simulating errors
type mockSyncIndexerWithErrors struct {
	blockStoredErr  error
	blockRemovedErr error
	removePrefixErr error
	storedEvents    []BlockStoredEvent
	removedEvents   []BlockRemovedEvent
}

func (m *mockSyncIndexerWithErrors) ProcessBlockStored(ctx context.Context, event BlockStoredEvent) error {
	m.storedEvents = append(m.storedEvents, event)
	return m.blockStoredErr
}

func (m *mockSyncIndexerWithErrors) ProcessBlockRemoved(ctx context.Context, event BlockRemovedEvent) error {
	m.removedEvents = append(m.removedEvents, event)
	return m.blockRemovedErr
}

func (m *mockSyncIndexerWithErrors) RemovePrefix(ctx context.Context, modelName string, loraID int64, podKey string) error {
	return m.removePrefixErr
}

// mockSyncProvider for testing
type mockSyncProvider struct {
	indexer SyncIndexer
	err     error
}

func (m *mockSyncProvider) GetSyncIndexer(ctx context.Context) (SyncIndexer, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.indexer, nil
}

// Test tokenIDsToBytes conversion
func TestTokenIDsToBytes(t *testing.T) {
	tests := []struct {
		name     string
		tokenIDs []int32
		expected []byte
	}{
		{
			name:     "empty tokens",
			tokenIDs: []int32{},
			expected: []byte{},
		},
		{
			name:     "single token",
			tokenIDs: []int32{12345},
			expected: []byte{0, 0, 48, 57}, // 12345 in big-endian
		},
		{
			name:     "multiple tokens",
			tokenIDs: []int32{1, 256, 65535},
			expected: []byte{
				0, 0, 0, 1, // 1
				0, 0, 1, 0, // 256
				0, 0, 255, 255, // 65535
			},
		},
		{
			name:     "negative token",
			tokenIDs: []int32{-1},
			expected: []byte{255, 255, 255, 255}, // -1 in two's complement
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tokenIDsToBytes(tt.tokenIDs)
			if len(result) != len(tt.expected) {
				t.Fatalf("Expected length %d, got %d", len(tt.expected), len(result))
			}
			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("Byte %d: expected %d, got %d", i, tt.expected[i], result[i])
				}
			}
		})
	}
}

// Test convertTokenIDs
func TestConvertTokenIDs(t *testing.T) {
	input := [][]int32{
		{1, 2, 3},
		{},
		{12345},
	}

	result := convertTokenIDs(input)

	if len(result) != 3 {
		t.Fatalf("Expected 3 results, got %d", len(result))
	}

	// Check first array
	expected0 := []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3}
	if len(result[0]) != len(expected0) {
		t.Errorf("Result[0] length mismatch")
	}

	// Check second array (empty)
	if len(result[1]) != 0 {
		t.Errorf("Result[1] should be empty")
	}

	// Check third array
	expected2 := []byte{0, 0, 48, 57}
	if len(result[2]) != len(expected2) {
		t.Errorf("Result[2] length mismatch")
	}
}

// Test HandleEvent with BlockStoredEvent
func TestHandleBlockStoredEvent(t *testing.T) {
	syncIndexer := &mockSyncIndexerWithErrors{}
	syncProvider := &mockSyncProvider{
		indexer: syncIndexer,
	}

	// Create a real manager with mock providers
	manager := &Manager{
		syncProvider: syncProvider,
		ctx:          context.Background(),
	}

	handler := &eventHandler{
		manager:   manager,
		podKey:    "default/test-pod",
		modelName: "test-model",
		loraID:    123,
	}

	event := &kvcache.BlockStoredEvent{
		BlockHashes:     []int64{1001, 1002, 1003},
		ParentBlockHash: &[]int64{1000}[0],
		TokenIDs:        [][]int32{{1, 2, 3}, {4, 5, 6}},
	}

	err := handler.HandleEvent(event)
	if err != nil {
		t.Errorf("HandleEvent failed: %v", err)
	}

	// Verify the event was processed
	if len(syncIndexer.storedEvents) != 1 {
		t.Fatalf("Expected 1 stored event, got %d", len(syncIndexer.storedEvents))
	}

	storedEvent := syncIndexer.storedEvents[0]
	if len(storedEvent.BlockHashes) != 3 {
		t.Errorf("Expected 3 block hashes, got %d", len(storedEvent.BlockHashes))
	}
	if storedEvent.ModelName != "test-model" {
		t.Errorf("Expected model name 'test-model', got %s", storedEvent.ModelName)
	}
	if storedEvent.LoraID != 123 {
		t.Errorf("Expected LoraID 123, got %d", storedEvent.LoraID)
	}
	if storedEvent.SourcePod != "default/test-pod" {
		t.Errorf("Expected pod key 'default/test-pod', got %s", storedEvent.SourcePod)
	}
	if len(storedEvent.Tokens) != 2 {
		t.Errorf("Expected 2 token arrays, got %d", len(storedEvent.Tokens))
	}
}

// Test HandleEvent with BlockRemovedEvent
func TestHandleBlockRemovedEvent(t *testing.T) {
	syncIndexer := &mockSyncIndexerWithErrors{}
	syncProvider := &mockSyncProvider{
		indexer: syncIndexer,
	}

	manager := &Manager{
		syncProvider: syncProvider,
		ctx:          context.Background(),
	}

	handler := &eventHandler{
		manager:   manager,
		podKey:    "default/test-pod",
		modelName: "test-model",
		loraID:    456,
	}

	event := &kvcache.BlockRemovedEvent{
		BlockHashes: []int64{2001, 2002},
	}

	err := handler.HandleEvent(event)
	if err != nil {
		t.Errorf("HandleEvent failed: %v", err)
	}

	// Verify the event was processed
	if len(syncIndexer.removedEvents) != 1 {
		t.Fatalf("Expected 1 removed event, got %d", len(syncIndexer.removedEvents))
	}

	removedEvent := syncIndexer.removedEvents[0]
	if len(removedEvent.BlockHashes) != 2 {
		t.Errorf("Expected 2 block hashes, got %d", len(removedEvent.BlockHashes))
	}
	if removedEvent.ModelName != "test-model" {
		t.Errorf("Expected model name 'test-model', got %s", removedEvent.ModelName)
	}
}

// Test HandleEvent with AllBlocksClearedEvent
func TestHandleAllBlocksClearedEvent(t *testing.T) {
	manager := &Manager{
		ctx: context.Background(),
	}

	handler := &eventHandler{
		manager:   manager,
		podKey:    "default/test-pod",
		modelName: "test-model",
		loraID:    789,
	}

	event := &kvcache.AllBlocksClearedEvent{
		ModelName: "test-model",
	}

	// Should not return error (no-op implementation)
	err := handler.HandleEvent(event)
	if err != nil {
		t.Errorf("HandleEvent failed: %v", err)
	}
}

// Test HandleEvent with unknown event type
func TestHandleUnknownEvent(t *testing.T) {
	manager := &Manager{
		ctx: context.Background(),
	}

	handler := &eventHandler{
		manager: manager,
	}

	// Create a mock unknown event
	type unknownEvent struct {
		kvcache.KVEvent
	}

	err := handler.HandleEvent(&unknownEvent{})
	if err != nil {
		t.Errorf("HandleEvent should not error on unknown event type: %v", err)
	}
}

// Test HandleEvent when manager is stopped
func TestHandleEventManagerStopped(t *testing.T) {
	manager := &Manager{
		stopped: true,
		ctx:     context.Background(),
	}

	handler := &eventHandler{
		manager: manager,
	}

	event := &kvcache.BlockStoredEvent{}
	err := handler.HandleEvent(event)

	if !errors.Is(err, ErrManagerStopped) {
		t.Errorf("Expected ErrManagerStopped, got: %v", err)
	}
}

// Test HandleEvent with temporary error
func TestHandleEventTemporaryError(t *testing.T) {
	syncProvider := &mockSyncProvider{
		err: ErrIndexerNotInitialized,
	}

	manager := &Manager{
		syncProvider: syncProvider,
		ctx:          context.Background(),
	}

	handler := &eventHandler{
		manager:   manager,
		podKey:    "default/test-pod",
		modelName: "test-model",
		loraID:    123,
	}

	event := &kvcache.BlockStoredEvent{
		BlockHashes: []int64{1001},
	}

	// Should not return error for temporary errors
	err := handler.HandleEvent(event)
	if err != nil {
		t.Errorf("HandleEvent should not fail on temporary error: %v", err)
	}
}

// Test HandleEvent with processing error
func TestHandleEventProcessingError(t *testing.T) {
	expectedErr := errors.New("processing failed")
	syncIndexer := &mockSyncIndexerWithErrors{
		blockStoredErr: expectedErr,
	}
	syncProvider := &mockSyncProvider{
		indexer: syncIndexer,
	}

	manager := &Manager{
		syncProvider: syncProvider,
		ctx:          context.Background(),
	}

	handler := &eventHandler{
		manager:   manager,
		podKey:    "default/test-pod",
		modelName: "test-model",
		loraID:    123,
	}

	event := &kvcache.BlockStoredEvent{
		BlockHashes: []int64{1001},
	}

	err := handler.HandleEvent(event)
	if err == nil {
		t.Error("Expected error from processing")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("Expected %v, got %v", expectedErr, err)
	}
}

// Test binary encoding is correct
func TestBinaryEncoding(t *testing.T) {
	// Test that our encoding matches the expected format
	tokenID := int32(12345)
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(bytes, uint32(tokenID))

	expected := []byte{0, 0, 48, 57}
	for i := range bytes {
		if bytes[i] != expected[i] {
			t.Errorf("Byte %d: expected %d, got %d", i, expected[i], bytes[i])
		}
	}
}
