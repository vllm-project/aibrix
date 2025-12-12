// Copyright 2025 The AIBrix Authors
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

package kvcache

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockStoredEventEncodeDecode(t *testing.T) {
	ts := time.Now().UTC()
	originalEvent := &BlockStoredEvent{
		Type:            EventTypeBlockStored,
		BlockHashes:     []int64{101, 102, 103},
		ParentBlockHash: func() *int64 { v := int64(100); return &v }(),
		TokenIDs: [][]byte{
			tokenIDsToBytes([]uint32{1, 2, 3, 4}),
			tokenIDsToBytes([]uint32{5, 6, 7, 8}),
		},
		ModelName: "gpt-test",
		PodName:   "pod-a",
	}

	original := &EventBatch{
		Timestamp: ts,
		Events:    []KVEvent{originalEvent},
	}

	// Encode
	data, err := EncodeEventBatch(original)
	require.NoError(t, err)

	// Decode
	decoded, err := DecodeEventBatch(data, "gpt-test", "pod-a")
	require.NoError(t, err)
	require.Len(t, decoded.Events, 1)

	// Type assert
	stored, ok := decoded.Events[0].(*BlockStoredEvent)
	require.True(t, ok, "decoded event is not BlockStoredEvent")

	// Compare BlockHashes
	assert.Equal(t, originalEvent.BlockHashes, stored.BlockHashes)

	// Compare ParentBlockHash
	assert.NotNil(t, stored.ParentBlockHash)
	assert.Equal(t, int64(100), *stored.ParentBlockHash)

	// Compare TokenIDs
	expectedTokens := [][]uint32{
		{1, 2, 3, 4},
		{5, 6, 7, 8},
	}

	for i, block := range stored.TokenIDs {
		for j := 0; j < len(block); j += 4 {
			val := binary.BigEndian.Uint32(block[j : j+4])
			assert.Equal(t, expectedTokens[i][j/4], val,
				"token mismatch at block %d index %d", i, j/4)
		}
	}
}

func TestBlockStoredEventNilParent(t *testing.T) {
	batch := &EventBatch{
		Timestamp: time.Now().UTC(),
		Events: []KVEvent{
			&BlockStoredEvent{
				Type:            EventTypeBlockStored,
				BlockHashes:     []int64{42},
				ParentBlockHash: nil,
				TokenIDs: [][]byte{
					tokenIDsToBytes([]uint32{9, 8}),
				},
			},
		},
	}

	data, err := EncodeEventBatch(batch)
	require.NoError(t, err)

	decoded, err := DecodeEventBatch(data, "modelX", "podX")
	require.NoError(t, err)

	stored := decoded.Events[0].(*BlockStoredEvent)
	assert.Nil(t, stored.ParentBlockHash)
}

func TestBlockStoredEventZeroLengthTokens(t *testing.T) {
	batch := &EventBatch{
		Timestamp: time.Now().UTC(),
		Events: []KVEvent{
			&BlockStoredEvent{
				Type:        EventTypeBlockStored,
				BlockHashes: []int64{1, 2},
				TokenIDs:    [][]byte{},
			},
		},
	}

	data, err := EncodeEventBatch(batch)
	require.NoError(t, err)

	decoded, err := DecodeEventBatch(data, "modelZ", "podZ")
	require.NoError(t, err)

	stored := decoded.Events[0].(*BlockStoredEvent)
	assert.Len(t, stored.TokenIDs, 0)
}

func TestEmptyEventBatch(t *testing.T) {
	batch := &EventBatch{
		Timestamp: time.Now().UTC(),
		Events:    []KVEvent{},
	}

	data, err := EncodeEventBatch(batch)
	require.NoError(t, err)

	decoded, err := DecodeEventBatch(data, "modelEmpty", "podEmpty")
	require.NoError(t, err)

	assert.Len(t, decoded.Events, 0)
}

func TestMultipleEventsInBatch_MixedEvents(t *testing.T) {
	batch := &EventBatch{
		Timestamp: time.Now().UTC(),
		Events: []KVEvent{
			// Multiple BlockStoredEvents
			&BlockStoredEvent{
				Type:        EventTypeBlockStored,
				BlockHashes: []int64{1, 2},
				TokenIDs: [][]byte{
					tokenIDsToBytes([]uint32{10, 20}),
				},
			},
			&BlockStoredEvent{
				Type:        EventTypeBlockStored,
				BlockHashes: []int64{3},
				TokenIDs: [][]byte{
					tokenIDsToBytes([]uint32{30, 40}),
				},
			},
			// Multiple BlockRemovedEvents
			&BlockRemovedEvent{
				Type:        EventTypeBlockRemoved,
				BlockHashes: []int64{99},
			},
			&BlockRemovedEvent{
				Type:        EventTypeBlockRemoved,
				BlockHashes: []int64{100, 101},
			},
			// Multiple AllBlocksClearedEvents
			&AllBlocksClearedEvent{Type: EventTypeAllCleared},
			&AllBlocksClearedEvent{Type: EventTypeAllCleared},
		},
	}

	data, err := EncodeEventBatch(batch)
	require.NoError(t, err)

	decoded, err := DecodeEventBatch(data, "multiModel", "multiPod")
	require.NoError(t, err)

	require.Len(t, decoded.Events, 6)

	// Validate BlockStoredEvents
	stored1, ok := decoded.Events[0].(*BlockStoredEvent)
	require.True(t, ok)
	assert.Equal(t, []int64{1, 2}, stored1.BlockHashes)

	stored2, ok := decoded.Events[1].(*BlockStoredEvent)
	require.True(t, ok)
	assert.Equal(t, []int64{3}, stored2.BlockHashes)

	// Validate BlockRemovedEvents
	removed1, ok := decoded.Events[2].(*BlockRemovedEvent)
	require.True(t, ok)
	assert.Equal(t, []int64{99}, removed1.BlockHashes)

	removed2, ok := decoded.Events[3].(*BlockRemovedEvent)
	require.True(t, ok)
	assert.Equal(t, []int64{100, 101}, removed2.BlockHashes)

	// Validate AllBlocksClearedEvents
	cleared1, ok := decoded.Events[4].(*AllBlocksClearedEvent)
	require.True(t, ok)
	assert.Equal(t, EventTypeAllCleared, cleared1.Type)

	cleared2, ok := decoded.Events[5].(*AllBlocksClearedEvent)
	require.True(t, ok)
	assert.Equal(t, EventTypeAllCleared, cleared2.Type)

	// Check that all events have correct metadata where applicable
	for _, e := range decoded.Events {
		switch ev := e.(type) {
		case *BlockStoredEvent:
			assert.Equal(t, "multiModel", ev.ModelName)
			assert.Equal(t, "multiPod", ev.PodName)
		case *BlockRemovedEvent:
			assert.Equal(t, "multiModel", ev.ModelName)
			assert.Equal(t, "multiPod", ev.PodName)
		case *AllBlocksClearedEvent:
			assert.Equal(t, "multiModel", ev.ModelName)
			assert.Equal(t, "multiPod", ev.PodName)
		default:
			t.Fatalf("unexpected event type: %T", e)
		}
	}
}
