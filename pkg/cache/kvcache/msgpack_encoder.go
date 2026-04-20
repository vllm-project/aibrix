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
	"fmt"

	msgpack "github.com/vmihailenco/msgpack/v5"
)

// EncodeEventBatch encodes an EventBatch to vLLM msgpack format.
// Only the array-encoded event fields are included; subscriber metadata (Timestamp, ModelName, PodName) is NOT encoded.
func EncodeEventBatch(batch *EventBatch) ([]byte, error) {
	if batch == nil {
		return nil, fmt.Errorf("nil event batch")
	}

	rawEvents := make([]interface{}, 0, len(batch.Events))
	for _, event := range batch.Events {
		rawEvent, err := encodeEvent(event)
		if err != nil {
			return nil, fmt.Errorf("failed to encode event: %w", err)
		}
		rawEvents = append(rawEvents, rawEvent)
	}

	// vLLM expects the batch as [timestamp, [events...]]
	rawBatch := []interface{}{
		float64(batch.Timestamp.UnixNano()) / 1e9, // float timestamp
		rawEvents,
	}

	// Marshal to MessagePack
	data, err := msgpack.Marshal(rawBatch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal event batch: %w", err)
	}

	return data, nil
}

// encodeEvent converts a KVEvent into the array-like format vLLM expects.
func encodeEvent(event KVEvent) ([]interface{}, error) {
	switch e := event.(type) {
	case *BlockStoredEvent:
		// Flatten [][]byte back to []uint32
		tokenIDs := flattenTokens(e.TokenIDs)
		if tokenIDs == nil {
			tokenIDs = []uint32{}
		}

		// Determine block_size from first block
		blockSize := 0
		if len(e.TokenIDs) > 0 {
			n := len(e.TokenIDs[0])
			if n%4 != 0 {
				return nil, fmt.Errorf("invalid TokenIDs length %d, must be multiple of 4", n)
			}
			blockSize = n / 4
		}

		arr := []interface{}{
			string(e.Type),    // tag
			e.BlockHashes,     // block_hashes
			e.ParentBlockHash, // parent_block_hash (nullable *[]byte)
			tokenIDs,          // flat token IDs
			blockSize,         // block_size
		}
		return arr, nil

	case *BlockRemovedEvent:
		arr := []interface{}{
			string(e.Type),
			e.BlockHashes,
		}
		return arr, nil

	case *AllBlocksClearedEvent:
		return []interface{}{string(e.Type)}, nil

	default:
		return nil, fmt.Errorf("unknown event type: %T", event)
	}
}

// flattenTokens converts [][]byte (each block) back to []uint32 for encoding
func flattenTokens(tokens [][]byte) []uint32 {
	var result []uint32
	for _, block := range tokens {
		for i := 0; i < len(block); i += 4 {
			val := binary.BigEndian.Uint32(block[i : i+4])
			result = append(result, val)
		}
	}
	return result
}
