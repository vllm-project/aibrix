// Copyright 2025 The AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	 http://www.apache.org/licenses/LICENSE-2.0
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
	"math"
	"time"

	msgpack "github.com/vmihailenco/msgpack/v5"
)

// DecodeEventBatch parses a raw msgpack batch of events.
// The subscriber must supply batch timestamp + model/pod name.
func DecodeEventBatch(
	data []byte,
	modelName string,
	podName string,
) (*EventBatch, error) {
	// The batch contains [ts, events]
	var rawBatch []interface{}
	if err := msgpack.Unmarshal(data, &rawBatch); err != nil {
		return nil, fmt.Errorf("failed to unmarshal event batch: %w", err)
	}
	if len(rawBatch) != 2 {
		return nil, fmt.Errorf("expected 2 elements in batch (ts, events), got %d", len(rawBatch))
	}

	// 0: batch timestamp
	tsFloat, ok := rawBatch[0].(float64)
	if !ok {
		return nil, fmt.Errorf("invalid batch timestamp type: %T", rawBatch[0])
	}
	batchTS := time.Unix(int64(tsFloat), int64((tsFloat-float64(int64(tsFloat)))*1e9)).UTC()

	// 1: events array
	eventsRaw, ok := rawBatch[1].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected events array, got %T", rawBatch[1])
	}

	batch := &EventBatch{
		Timestamp: batchTS,
		Events:    make([]KVEvent, 0, len(eventsRaw)),
	}

	for i, raw := range eventsRaw {
		arr, ok := raw.([]interface{})
		if !ok {
			return nil, fmt.Errorf("event %d: expected msgpack array, got %T", i, raw)
		}

		evt, err := parseEventArray(arr)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}

		// Apply batch metadata
		applyBatchMetadata(evt, batchTS, modelName, podName)
		batch.Events = append(batch.Events, evt)
	}

	return batch, nil
}

func parseEventArray(arr []interface{}) (KVEvent, error) {
	if len(arr) == 0 {
		return nil, fmt.Errorf("empty event array")
	}

	// First element is event type tag
	rawTag, ok := arr[0].(string)
	if !ok {
		return nil, fmt.Errorf("event tag not string: %T", arr[0])
	}
	tag := EventType(rawTag)

	switch tag {

	case EventTypeBlockStored:
		// Minimum = 5 fields
		if len(arr) < 5 {
			return nil, fmt.Errorf("BlockStored requires at least 5 fields, got %d", len(arr))
		}

		// 1: block_hashes
		blockHashes, err := toInt64Slice(arr[1])
		if err != nil {
			return nil, fmt.Errorf("invalid block_hashes: %w", err)
		}

		// 2: parent_block_hash
		parentHash, err := toInt64Ptr(arr[2])
		if err != nil {
			return nil, fmt.Errorf("invalid parent_block_hash: %w", err)
		}

		// 3: token_ids
		rawTokenIDs, ok := arr[3].([]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid token_ids type: %T", arr[3])
		}

		// 4: block_size (required)
		blockSize, err := parseInt(arr[4])
		if err != nil {
			return nil, fmt.Errorf("invalid block_size: %w", err)
		}

		// Flatten tokenIDs into []uint32
		tokenIDs := make([]uint32, len(rawTokenIDs))
		for i, v := range rawTokenIDs {
			n, err := parseUint32(v)
			if err != nil {
				return nil, fmt.Errorf("token_ids[%d]: %w", i, err)
			}
			tokenIDs[i] = n
		}

		// Convert directly to [][]byte grouped by blockSize
		tokens, err := convertTokenIDs(tokenIDs, blockSize)
		if err != nil {
			return nil, err
		}

		return &BlockStoredEvent{
			Type:            EventTypeBlockStored,
			BlockHashes:     blockHashes,
			ParentBlockHash: parentHash,
			TokenIDs:        tokens,
		}, nil

	case EventTypeBlockRemoved:
		if len(arr) < 2 {
			return nil, fmt.Errorf("BlockRemoved expects ≥2 fields, got %d", len(arr))
		}

		blockHashes, err := toInt64Slice(arr[1])
		if err != nil {
			return nil, fmt.Errorf("invalid block_hashes: %w", err)
		}

		ev := &BlockRemovedEvent{
			Type:        tag,
			BlockHashes: blockHashes,
		}

		return ev, nil

	case EventTypeAllCleared:
		return &AllBlocksClearedEvent{
			Type: tag,
		}, nil

	default:
		return nil, fmt.Errorf("unknown event type: %s", tag)
	}
}

func applyBatchMetadata(evt KVEvent, ts time.Time, model, pod string) {
	switch e := evt.(type) {

	case *BlockStoredEvent:
		e.Timestamp = ts
		e.ModelName = model
		e.PodName = pod

	case *BlockRemovedEvent:
		e.Timestamp = ts
		e.ModelName = model
		e.PodName = pod

	case *AllBlocksClearedEvent:
		e.Timestamp = ts
		e.ModelName = model
		e.PodName = pod
	}
}

func toInt64Slice(v any) ([]int64, error) {
	raw, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{}, got %T", v)
	}
	out := make([]int64, len(raw))
	for i, x := range raw {
		val, err := parseInt64(x)
		if err != nil {
			return nil, fmt.Errorf("block_hashes[%d]: %w", i, err)
		}
		out[i] = val
	}
	return out, nil
}

func toInt64Ptr(v any) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	val, err := parseInt64(v)
	if err != nil {
		return nil, err
	}
	return &val, nil
}

func parseUint32(v any) (uint32, error) {
	switch x := v.(type) {

	// ---- Unsigned integer types ----
	case uint:
		if x > math.MaxUint32 {
			return 0, fmt.Errorf("uint out of uint32 range: %d", x)
		}
		return uint32(x), nil

	case uint8:
		return uint32(x), nil

	case uint16:
		return uint32(x), nil

	case uint32:
		return x, nil

	case uint64:
		if x > math.MaxUint32 {
			return 0, fmt.Errorf("uint64 out of uint32 range: %d", x)
		}
		return uint32(x), nil

	// ---- Signed integer types ----
	case int:
		if x < 0 || x > math.MaxUint32 {
			return 0, fmt.Errorf("int out of uint32 range: %d", x)
		}
		return uint32(x), nil

	case int8:
		if x < 0 {
			return 0, fmt.Errorf("int8 negative: %d", x)
		}
		return uint32(x), nil

	case int16:
		if x < 0 {
			return 0, fmt.Errorf("int16 negative: %d", x)
		}
		return uint32(x), nil

	case int32:
		if x < 0 {
			return 0, fmt.Errorf("int32 negative: %d", x)
		}
		return uint32(x), nil

	case int64:
		if x < 0 || x > math.MaxUint32 {
			return 0, fmt.Errorf("int64 out of uint32 range: %d", x)
		}
		return uint32(x), nil

	// ---- Floating-point types ----
	case float32:
		f := float64(x)
		if f < 0 || f > math.MaxUint32 {
			return 0, fmt.Errorf("float32 out of uint32 range: %f", f)
		}
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("float32 has fractional part: %f", f)
		}
		return uint32(f), nil

	case float64:
		if x < 0 || x > math.MaxUint32 {
			return 0, fmt.Errorf("float64 out of uint32 range: %f", x)
		}
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("float64 has fractional part: %f", x)
		}
		return uint32(x), nil

	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}

func parseInt(v any) (int, error) {
	switch x := v.(type) {
	case int, int8, int16, int32, int64:
		return int(toInt64(x)), nil
	case uint, uint8, uint16, uint32, uint64:
		if toUint64(x) > math.MaxInt {
			return 0, fmt.Errorf("int overflow: %d", x)
		}
		return int(toUint64(x)), nil
	case float64:
		return int(x), nil
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	}
	panic("unreachable")
}

func toUint64(v any) uint64 {
	switch x := v.(type) {
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint64:
		return x
	}
	panic("unreachable")
}

func parseInt64(v any) (int64, error) {
	switch x := v.(type) {

	// ---- Signed integers ----
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil

	// ---- Unsigned integers ----
	case uint:
		if x > math.MaxInt64 {
			return 0, fmt.Errorf("uint out of int64 range: %d", x)
		}
		return int64(x), nil

	case uint8:
		return int64(x), nil

	case uint16:
		return int64(x), nil

	case uint32:
		return int64(x), nil

	case uint64:
		if x > uint64(math.MaxInt64) {
			return 0, fmt.Errorf("uint64 out of int64 range: %d", x)
		}
		return int64(x), nil

	// ---- Floating-point ----
	case float32:
		f := float64(x)
		if f < math.MinInt64 || f > math.MaxInt64 {
			return 0, fmt.Errorf("float32 out of int64 range: %f", f)
		}
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("float32 has fractional part: %f", f)
		}
		return int64(f), nil

	case float64:
		if x < math.MinInt64 || x > math.MaxInt64 {
			return 0, fmt.Errorf("float64 out of int64 range: %f", x)
		}
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("float64 has fractional part: %f", x)
		}
		return int64(x), nil

	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}

// convertTokenIDs groups tokenIDs into blocks of size blockSize and converts each block to []byte.
// Each uint32 value is encoded as 4 bytes in big-endian format.
func convertTokenIDs(tokenIDs []uint32, blockSize int) ([][]byte, error) {
	if len(tokenIDs) == 0 {
		return [][]byte{}, nil
	}

	if blockSize <= 0 {
		return nil, fmt.Errorf("blockSize must be > 0, got %d", blockSize)
	}
	if len(tokenIDs)%blockSize != 0 {
		return nil, fmt.Errorf(
			"tokenIDs len=%d not divisible by blockSize=%d",
			len(tokenIDs), blockSize,
		)
	}

	numBlocks := len(tokenIDs) / blockSize
	result := make([][]byte, numBlocks)

	for i := 0; i < numBlocks; i++ {
		start := i * blockSize
		end := start + blockSize
		result[i] = tokenIDsToBytes(tokenIDs[start:end])
	}
	return result, nil
}

// tokenIDsToBytes converts slice of uint32 to big-endian []byte.
func tokenIDsToBytes(ids []uint32) []byte {
	out := make([]byte, len(ids)*4)
	for i, v := range ids {
		binary.BigEndian.PutUint32(out[i*4:], v)
	}
	return out
}
