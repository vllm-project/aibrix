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
	"k8s.io/klog/v2"
)

// DecodeEventBatch parses a raw msgpack payload of KV cache events.
// The subscriber must supply batch timestamp + model/pod name.
//
// vLLM's ZMQ publisher sends one msgspec-encoded EventBatch per message. The
// batch is an array, [ts, events] or [ts, events, data_parallel_rank]. Each
// event is a map with its type under "type" since vllm-project/vllm#42892
// (v0.24), and an array with the type first before that:
//
//	{"type": "BlockStored", "block_hashes": [...], ...}
//	["BlockStored", block_hashes, parent_block_hash, token_ids, block_size, ...]
//
// A single event outside a batch, in either encoding, is accepted too.
func DecodeEventBatch(
	data []byte,
	modelName string,
	podName string,
) (*EventBatch, error) {
	var raw interface{}
	if err := msgpack.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal event batch: %w", err)
	}

	switch v := raw.(type) {
	case map[string]interface{}:
		// A single map-encoded event
		evt, err := parseEventMap(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse map event: %w", err)
		}
		ts := time.Now().UTC()
		applyBatchMetadata(evt, ts, modelName, podName)
		return &EventBatch{
			Timestamp: ts,
			Events:    []KVEvent{evt},
		}, nil

	case []interface{}:
		return decodeEventArray(v, modelName, podName)

	default:
		return nil, fmt.Errorf("unexpected event payload type: %T", raw)
	}
}

// decodeEventArray dispatches an array payload to either a batch [ts, events]
// or a single array-encoded event (tag first).
func decodeEventArray(arr []interface{}, modelName, podName string) (*EventBatch, error) {
	if len(arr) == 0 {
		return nil, fmt.Errorf("empty event array payload")
	}

	switch arr[0].(type) {
	case string:
		// Single event: [tag, fields...]
		evt, err := parseEventArray(arr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse single event: %w", err)
		}
		ts := time.Now().UTC()
		applyBatchMetadata(evt, ts, modelName, podName)
		return &EventBatch{
			Timestamp: ts,
			Events:    []KVEvent{evt},
		}, nil

	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		// Batch: [ts, events] (+ optional data_parallel_rank)
		return decodeBatchArray(arr, modelName, podName)

	default:
		return nil, fmt.Errorf("unexpected first element type in event payload: %T", arr[0])
	}
}

// decodeBatchArray parses the [ts, events] batch wrapper.
func decodeBatchArray(arr []interface{}, modelName, podName string) (*EventBatch, error) {
	// if size of rawBatch is 3, the third element is the data parallel rank
	// data_parallel_rank is not used in aibrix now
	if len(arr) == 3 {
		if data_parallel_rank, err := parseInt(arr[2]); err != nil {
			return nil, fmt.Errorf("data_parallel_rank is not an int: %T", arr[2])
		} else {
			klog.V(4).Infof("event has data_parallel_rank: %d", data_parallel_rank)
		}
	} else if len(arr) != 2 {
		return nil, fmt.Errorf("expected 2 elements in batch (ts, events), got %d", len(arr))
	}

	// 0: batch timestamp
	tsFloat, ok := arr[0].(float64)
	if !ok {
		return nil, fmt.Errorf("invalid batch timestamp type: %T", arr[0])
	}
	batchTS := time.Unix(int64(tsFloat), int64((tsFloat-float64(int64(tsFloat)))*1e9)).UTC()

	// 1: events array
	eventsRaw, ok := arr[1].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected events array, got %T", arr[1])
	}

	batch := &EventBatch{
		Timestamp: batchTS,
		Events:    make([]KVEvent, 0, len(eventsRaw)),
	}

	for i, raw := range eventsRaw {
		var evt KVEvent
		var err error
		switch e := raw.(type) {
		case []interface{}:
			evt, err = parseEventArray(e)
		case map[string]interface{}:
			evt, err = parseEventMap(e)
		default:
			return nil, fmt.Errorf("event %d: expected msgpack array or map, got %T", i, raw)
		}
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
	case EventTypeBlockRemoved:
		if len(arr) < 2 {
			return nil, fmt.Errorf("BlockRemoved expects ≥2 fields, got %d", len(arr))
		}
	}

	return parseEvent(tag, arrayFields(arr))
}

// parseEventMap parses a single event encoded as a msgpack map (vLLM's
// post-#42892 encoding). The map is flat, with the event type under the
// "type" key and all fields under their Python attribute names.
func parseEventMap(m map[string]interface{}) (KVEvent, error) {
	rawTag, ok := m["type"]
	if !ok {
		return nil, fmt.Errorf("map event missing 'type' key")
	}
	tagStr, ok := rawTag.(string)
	if !ok {
		return nil, fmt.Errorf("event tag not string: %T", rawTag)
	}

	return parseEvent(EventType(tagStr), mapFields(m))
}

// eventFields returns an event field by its name in map-encoded events or by
// its position after the type tag in array-encoded ones, and whether the event
// has it. msgspec omit_defaults may drop trailing or default fields, so
// optional fields can be missing in both encodings.
type eventFields func(name string, pos int) (any, bool)

func arrayFields(arr []interface{}) eventFields {
	return func(_ string, pos int) (any, bool) {
		if pos < len(arr) {
			return arr[pos], true
		}
		return nil, false
	}
}

func mapFields(m map[string]interface{}) eventFields {
	return func(name string, _ int) (any, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// optionalField is a field newer vLLM builds add to an event; it is left nil
// when the event does not carry it.
type optionalField struct {
	name string
	pos  int
	set  func(v any) error
}

func setOptionalFields(fields eventFields, optional []optionalField) error {
	for _, f := range optional {
		v, ok := fields(f.name, f.pos)
		if !ok {
			continue
		}
		if err := f.set(v); err != nil {
			return fmt.Errorf("invalid %s: %w", f.name, err)
		}
	}
	return nil
}

func parseEvent(tag EventType, fields eventFields) (KVEvent, error) {
	switch tag {
	case EventTypeBlockStored:
		return parseBlockStored(fields)
	case EventTypeBlockRemoved:
		return parseBlockRemoved(fields)
	case EventTypeAllCleared:
		return &AllBlocksClearedEvent{
			Type: tag,
		}, nil
	default:
		return nil, fmt.Errorf("unknown event type: %s", tag)
	}
}

// parseBlockStored parses a BlockStored event:
// [tag, block_hashes, parent_block_hash, token_ids, block_size, lora_id,
// medium, lora_name, extra_keys, group_idx, kv_cache_spec_kind,
// kv_cache_spec_sliding_window, locality].
func parseBlockStored(fields eventFields) (KVEvent, error) {
	v, _ := fields("block_hashes", 1)
	blockHashes, err := toBlockHashSlice(v)
	if err != nil {
		return nil, fmt.Errorf("invalid block_hashes: %w", err)
	}

	v, _ = fields("parent_block_hash", 2)
	parentHash, err := toBlockHashPtr(v)
	if err != nil {
		return nil, fmt.Errorf("invalid parent_block_hash: %w", err)
	}

	v, _ = fields("token_ids", 3)
	rawTokenIDs, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid token_ids type: %T", v)
	}

	v, _ = fields("block_size", 4)
	blockSize, err := parseInt(v)
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

	ev := &BlockStoredEvent{
		Type:            EventTypeBlockStored,
		BlockHashes:     blockHashes,
		ParentBlockHash: parentHash,
		TokenIDs:        tokens,
	}

	err = setOptionalFields(fields, []optionalField{
		{"lora_id", 5, func(v any) (err error) { ev.LoraID, err = toInt64Ptr(v); return }},
		{"medium", 6, func(v any) (err error) { ev.Medium, err = toStringPtr(v); return }},
		{"lora_name", 7, func(v any) (err error) { ev.LoraName, err = toStringPtr(v); return }},
		{"extra_keys", 8, func(v any) (err error) { ev.ExtraKeys, err = toExtraKeys(v); return }},
		{"group_idx", 9, func(v any) (err error) { ev.GroupIdx, err = toInt64Ptr(v); return }},
		{"kv_cache_spec_kind", 10, func(v any) (err error) { ev.KVCacheSpecKind, err = toStringPtr(v); return }},
		{"kv_cache_spec_sliding_window", 11, func(v any) (err error) {
			ev.KVCacheSpecSlidingWindow, err = toInt64Ptr(v)
			return
		}},
		{"locality", 12, func(v any) (err error) { ev.Locality, err = toStringPtr(v); return }},
	})
	if err != nil {
		return nil, err
	}

	return ev, nil
}

// parseBlockRemoved parses a BlockRemoved event:
// [tag, block_hashes, medium, group_idx, locality].
func parseBlockRemoved(fields eventFields) (KVEvent, error) {
	v, _ := fields("block_hashes", 1)
	blockHashes, err := toBlockHashSlice(v)
	if err != nil {
		return nil, fmt.Errorf("invalid block_hashes: %w", err)
	}

	ev := &BlockRemovedEvent{
		Type:        EventTypeBlockRemoved,
		BlockHashes: blockHashes,
	}

	err = setOptionalFields(fields, []optionalField{
		{"medium", 2, func(v any) (err error) { ev.Medium, err = toStringPtr(v); return }},
		{"group_idx", 3, func(v any) (err error) { ev.GroupIdx, err = toInt64Ptr(v); return }},
		{"locality", 4, func(v any) (err error) { ev.Locality, err = toStringPtr(v); return }},
	})
	if err != nil {
		return nil, err
	}

	return ev, nil
}

// toExtraKeys converts the extra_keys field (one entry per block, each a list
// of hash-computation inputs or nil) into [][]interface{}. A nil field or a
// list of nils yields a nil slice so callers can treat it as "not provided".
func toExtraKeys(v any) ([][]interface{}, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{}, got %T", v)
	}
	out := make([][]interface{}, len(raw))
	for i, x := range raw {
		if x == nil {
			out[i] = nil
			continue
		}
		entry, ok := x.([]interface{})
		if !ok {
			return nil, fmt.Errorf("extra_keys[%d]: expected []interface{}, got %T", i, x)
		}
		out[i] = entry
	}
	return out, nil
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

// toBlockHashSlice converts block_hashes field to []int64.
// Supports both legacy int64 format and new bytes format from vLLM PR #23673.
// This function handles the conversion at decode time, keeping the rest of the codebase simple.
func toBlockHashSlice(v any) ([]int64, error) {
	raw, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{}, got %T", v)
	}

	out := make([]int64, len(raw))
	for i, x := range raw {
		hash, err := parseBlockHashToInt64(x)
		if err != nil {
			return nil, fmt.Errorf("block_hashes[%d]: %w", i, err)
		}
		out[i] = hash
	}
	return out, nil
}

// bytesToInt64 converts a byte array to int64 using big-endian encoding.
// If the byte array is shorter than 8 bytes, it pads with leading zeros.
func bytesToInt64(b []byte) int64 {
	if len(b) >= 8 {
		// Use first 8 bytes for both 8-byte and 32-byte formats
		return int64(binary.BigEndian.Uint64(b[:8]))
	}
	// Unexpected short byte array: pad with leading zeros for big-endian
	padded := make([]byte, 8)
	copy(padded[8-len(b):], b)
	return int64(binary.BigEndian.Uint64(padded))
}

// parseBlockHashToInt64 parses a single block hash and converts it to int64.
// Supports:
// 1. int64 types (legacy format from old vLLM) → used directly
// 2. []byte (new format from vLLM PR #23673):
//   - 8 bytes: big-endian int64
//   - 32 bytes: SHA-256, uses first 8 bytes
//
// 3. string (msgpack may decode bytes as string) → same as []byte
//
// Using the first 8 bytes of SHA-256 provides sufficient uniqueness:
// - Collision probability ≈ 1/2^64 ≈ 10^-19 (extremely low)
// - In typical scenarios (thousands to millions of blocks), collisions are virtually impossible
func parseBlockHashToInt64(v any) (int64, error) {
	switch x := v.(type) {
	case []byte:
		return bytesToInt64(x), nil

	case string:
		// msgpack may decode bytes as string
		return bytesToInt64([]byte(x)), nil

	// Legacy format: integer types → convert to int64
	case int64:
		return x, nil

	case uint64:
		return int64(x), nil

	case int:
		return int64(x), nil

	case uint:
		return int64(x), nil

	case int8:
		return int64(x), nil

	case int16:
		return int64(x), nil

	case int32:
		return int64(x), nil

	case uint8:
		return int64(x), nil

	case uint16:
		return int64(x), nil

	case uint32:
		return int64(x), nil

	// Floating-point types (for backward compatibility with msgpack decoding)
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
		return 0, fmt.Errorf("unsupported block hash type: %T", v)
	}
}

// toBlockHashPtr converts a single block hash (can be nil) to *int64
func toBlockHashPtr(v any) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	hash, err := parseBlockHashToInt64(v)
	if err != nil {
		return nil, err
	}
	return &hash, nil
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

// toStringPtr converts a nullable msgpack string field to *string.
// A nil input (Python None) yields a nil pointer. msgpack may decode a string
// as either string or []byte, so both are accepted.
func toStringPtr(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	switch s := v.(type) {
	case string:
		return &s, nil
	case []byte:
		str := string(s)
		return &str, nil
	default:
		return nil, fmt.Errorf("expected string, got %T", v)
	}
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
