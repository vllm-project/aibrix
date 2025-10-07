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
	"fmt"

	msgpack "github.com/shamaton/msgpack/v2"
)

// EncodeEventBatch encodes an event batch to MessagePack format
func EncodeEventBatch(batch *EventBatch) ([]byte, error) {
	if batch == nil {
		return nil, fmt.Errorf("nil event batch")
	}

	// Convert events to encodable format
	rawBatch := map[string]interface{}{
		"events": make([]interface{}, 0, len(batch.Events)),
	}

	for _, event := range batch.Events {
		rawEvent, err := encodeEvent(event)
		if err != nil {
			return nil, fmt.Errorf("failed to encode event: %w", err)
		}
		rawBatch["events"] = append(rawBatch["events"].([]interface{}), rawEvent)
	}

	// Marshal to MessagePack
	data, err := msgpack.Marshal(rawBatch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal event batch: %w", err)
	}

	return data, nil
}

// encodeEvent converts a KVEvent to a map for encoding
func encodeEvent(event KVEvent) (map[string]interface{}, error) {
	switch e := event.(type) {
	case *BlockStoredEvent:
		return map[string]interface{}{
			"type":              string(e.Type),
			"timestamp":         e.Timestamp.Unix(),
			"block_hashes":      e.BlockHashes,
			"token_ids":         e.TokenIDs,
			"parent_block_hash": e.ParentBlockHash,
			"model_name":        e.ModelName,
		}, nil

	case *BlockRemovedEvent:
		return map[string]interface{}{
			"type":         string(e.Type),
			"timestamp":    e.Timestamp.Unix(),
			"block_hashes": e.BlockHashes,
			"model_name":   e.ModelName,
		}, nil

	case *AllBlocksClearedEvent:
		return map[string]interface{}{
			"type":       string(e.Type),
			"timestamp":  e.Timestamp.Unix(),
			"model_name": e.ModelName,
		}, nil

	default:
		return nil, fmt.Errorf("unknown event type: %T", event)
	}
}
