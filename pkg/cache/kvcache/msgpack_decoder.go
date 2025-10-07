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
	"time"

	msgpack "github.com/shamaton/msgpack/v2"
)

// DecodeEventBatch decodes a MessagePack encoded event batch
func DecodeEventBatch(data []byte) (*EventBatch, error) {
	var raw map[string]interface{}
	if err := msgpack.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal event batch: %w", err)
	}

	// Parse events array
	eventsRaw, ok := raw["events"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("missing or invalid events field")
	}

	batch := &EventBatch{
		Events: make([]KVEvent, 0, len(eventsRaw)),
	}

	for i, eventRaw := range eventsRaw {
		event, err := parseEvent(eventRaw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse event at index %d: %w", i, err)
		}
		batch.Events = append(batch.Events, event)
	}

	return batch, nil
}

// parseEvent parses a single event from raw data
func parseEvent(raw interface{}) (KVEvent, error) {
	// Handle both map[string]interface{} and map[interface{}]interface{}
	var eventMap map[string]interface{}

	switch m := raw.(type) {
	case map[string]interface{}:
		eventMap = m
	case map[interface{}]interface{}:
		// Convert map[interface{}]interface{} to map[string]interface{}
		eventMap = make(map[string]interface{})
		for k, v := range m {
			if ks, ok := k.(string); ok {
				eventMap[ks] = v
			} else {
				return nil, fmt.Errorf("non-string key in event map: %v", k)
			}
		}
	default:
		return nil, fmt.Errorf("invalid event format: expected map, got %T", raw)
	}

	eventType, ok := eventMap["type"].(string)
	if !ok {
		return nil, fmt.Errorf("missing event type")
	}

	switch EventType(eventType) {
	case EventTypeBlockStored:
		return parseBlockStoredEvent(eventMap)
	case EventTypeBlockRemoved:
		return parseBlockRemovedEvent(eventMap)
	case EventTypeAllCleared:
		return parseAllBlocksClearedEvent(eventMap)
	default:
		return nil, fmt.Errorf("unknown event type: %s", eventType)
	}
}

// parseBlockStoredEvent parses a BlockStoredEvent from raw data
func parseBlockStoredEvent(data map[string]interface{}) (*BlockStoredEvent, error) {
	event := &BlockStoredEvent{
		Type: EventTypeBlockStored,
	}

	// Parse timestamp
	if ts, err := parseTimestamp(data["timestamp"]); err == nil {
		event.Timestamp = ts
	} else {
		return nil, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	// Parse model name
	if modelName, ok := data["model_name"].(string); ok {
		event.ModelName = modelName
	} else {
		return nil, fmt.Errorf("missing or invalid model_name")
	}

	// Parse block hashes
	if hashes, err := parseInt64Array(data["block_hashes"]); err == nil {
		event.BlockHashes = hashes
	} else {
		return nil, fmt.Errorf("failed to parse block_hashes: %w", err)
	}

	// Parse token IDs (array of arrays)
	if tokenIDsRaw, ok := data["token_ids"].([]interface{}); ok {
		event.TokenIDs = make([][]int32, 0, len(tokenIDsRaw))
		for i, tokensRaw := range tokenIDsRaw {
			tokens, err := parseInt32Array(tokensRaw)
			if err != nil {
				return nil, fmt.Errorf("failed to parse token_ids at index %d: %w", i, err)
			}
			event.TokenIDs = append(event.TokenIDs, tokens)
		}
	} else {
		return nil, fmt.Errorf("missing or invalid token_ids")
	}

	// Parse optional parent block hash
	if parentHash, ok := data["parent_block_hash"]; ok && parentHash != nil {
		hash, err := parseInt64(parentHash)
		if err != nil {
			return nil, fmt.Errorf("failed to parse parent_block_hash: %w", err)
		}
		event.ParentBlockHash = &hash
	}

	return event, nil
}

// parseBlockRemovedEvent parses a BlockRemovedEvent from raw data
func parseBlockRemovedEvent(data map[string]interface{}) (*BlockRemovedEvent, error) {
	event := &BlockRemovedEvent{
		Type: EventTypeBlockRemoved,
	}

	// Parse timestamp
	if ts, err := parseTimestamp(data["timestamp"]); err == nil {
		event.Timestamp = ts
	} else {
		return nil, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	// Parse model name
	if modelName, ok := data["model_name"].(string); ok {
		event.ModelName = modelName
	} else {
		return nil, fmt.Errorf("missing or invalid model_name")
	}

	// Parse block hashes
	if hashes, err := parseInt64Array(data["block_hashes"]); err == nil {
		event.BlockHashes = hashes
	} else {
		return nil, fmt.Errorf("failed to parse block_hashes: %w", err)
	}

	return event, nil
}

// parseAllBlocksClearedEvent parses an AllBlocksClearedEvent from raw data
func parseAllBlocksClearedEvent(data map[string]interface{}) (*AllBlocksClearedEvent, error) {
	event := &AllBlocksClearedEvent{
		Type: EventTypeAllCleared,
	}

	// Parse timestamp
	if ts, err := parseTimestamp(data["timestamp"]); err == nil {
		event.Timestamp = ts
	} else {
		return nil, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	// Parse model name
	if modelName, ok := data["model_name"].(string); ok {
		event.ModelName = modelName
	} else {
		return nil, fmt.Errorf("missing or invalid model_name")
	}

	return event, nil
}

// Helper functions for parsing common types

func parseTimestamp(v interface{}) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case int64:
		// Unix timestamp in seconds
		return time.Unix(t, 0).UTC(), nil
	case int:
		// Unix timestamp in seconds
		return time.Unix(int64(t), 0).UTC(), nil
	case int32:
		// Unix timestamp in seconds
		return time.Unix(int64(t), 0).UTC(), nil
	case uint32:
		// Unix timestamp in seconds
		return time.Unix(int64(t), 0).UTC(), nil
	case uint64:
		// Unix timestamp in seconds
		return time.Unix(int64(t), 0).UTC(), nil
	case float64:
		// Unix timestamp with fractional seconds
		sec := int64(t)
		nsec := int64((t - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC().Truncate(time.Microsecond), nil
	case float32:
		// Unix timestamp with fractional seconds
		f64 := float64(t)
		sec := int64(f64)
		nsec := int64((f64 - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC().Truncate(time.Microsecond), nil
	case string:
		// Try to parse RFC3339 format
		return time.Parse(time.RFC3339, t)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp type: %T", v)
	}
}

func parseInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case uint:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("unsupported int64 type: %T", v)
	}
}

func parseInt64Array(v interface{}) ([]int64, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected array, got %T", v)
	}

	result := make([]int64, 0, len(arr))
	for i, item := range arr {
		val, err := parseInt64(item)
		if err != nil {
			return nil, fmt.Errorf("failed to parse element at index %d: %w", i, err)
		}
		result = append(result, val)
	}
	return result, nil
}

func parseInt32Array(v interface{}) ([]int32, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected array, got %T", v)
	}

	result := make([]int32, 0, len(arr))
	for i, item := range arr {
		switch n := item.(type) {
		case int32:
			result = append(result, n)
		case int:
			result = append(result, int32(n))
		case int64:
			result = append(result, int32(n))
		case int16:
			result = append(result, int32(n))
		case int8:
			result = append(result, int32(n))
		case uint:
			result = append(result, int32(n))
		case uint64:
			result = append(result, int32(n))
		case uint32:
			result = append(result, int32(n))
		case uint16:
			result = append(result, int32(n))
		case uint8:
			result = append(result, int32(n))
		case float64:
			result = append(result, int32(n))
		case float32:
			result = append(result, int32(n))
		default:
			return nil, fmt.Errorf("unsupported int32 type at index %d: %T", i, item)
		}
	}
	return result, nil
}
