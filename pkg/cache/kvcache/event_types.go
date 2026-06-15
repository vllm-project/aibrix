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
	"time"
)

// vLLM encodes KV cache events as lists of values in msgpack.
// In each list, the first entry is the event type as string.
// The remaining entries depend on the event type.
//
// [
//   "BlockStored",      # <- tag (string)
//   [123, 567, 999],    # block_hashes
//   None,               # parent_block_hash
//   [55, 11, 22],       # token_ids
//   32,                 # block_size
//   None,               # lora_id
//   "cuda:0"            # medium
// ]
//
// [
//   "BlockRemoved",
//   [555, 666],
//   "cpu"
// ]
//
// [
//   "AllBlocksCleared"
// ]

// EventType represents the type of KV cache event
type EventType string

// Note on Token Representation:
// - vLLM sends token IDs as []int32 arrays
// - Gateway expects tokens as []byte for hashing
// - Conversion: Each int32 is encoded as 4 bytes in big-endian format
// - Example: []int32{1, 2} becomes []byte{0, 0, 0, 1, 0, 0, 0, 2}

const (
	// EventTypeBlockStored indicates that blocks have been stored in the KV cache
	EventTypeBlockStored EventType = "BlockStored"
	// EventTypeBlockRemoved indicates that blocks have been removed from the KV cache
	EventTypeBlockRemoved EventType = "BlockRemoved"
	// EventTypeAllCleared indicates that all blocks have been cleared from the cache
	EventTypeAllCleared EventType = "AllBlocksCleared"
)

// KVEvent is the base interface for all KV cache events
type KVEvent interface {
	GetType() EventType
	// Timestamp of a KV event is shared among all events in the same EventBatch
	GetTimestamp() time.Time
	setTimestamp(time.Time)
	GetModelName() string
	setModelName(string)
	GetPodName() string
	setPodName(string)
}

// BlockStoredEvent represents blocks being stored in KV cache
// ------------------------------------------------------------
// BlockStored (msgspec encoding order)
// Python fields:
//
//	[tag, block_hashes, parent_block_hash, token_ids, block_size, lora_id, medium]
//
// ------------------------------------------------------------
//
// lora_id and medium are unused for now.
//
// Note: BlockHashes are converted at decode time:
// - vLLM legacy format (int64) → stored as-is
// - vLLM new format (32-byte SHA-256 from PR #23673) → first 8 bytes converted to int64
// This ensures internal consistency and compatibility with existing code.
type BlockStoredEvent struct {
	_               struct{}  `msgpack:",array"` // msgspec array encoding
	Type            EventType `msgpack:"-"`
	BlockHashes     []int64   // Decoded from vLLM, supports both old and new formats
	ParentBlockHash *int64    // Decoded from vLLM, supports both old and new formats
	TokenIDs        [][]byte

	// NOTE: These are NOT part of msgpack
	Timestamp time.Time `msgpack:"-"`
	ModelName string    `msgpack:"-"`
	PodName   string    `msgpack:"-"`
}

func (e *BlockStoredEvent) GetType() EventType        { return e.Type }
func (e *BlockStoredEvent) GetTimestamp() time.Time   { return e.Timestamp }
func (e *BlockStoredEvent) setTimestamp(ts time.Time) { e.Timestamp = ts }
func (e *BlockStoredEvent) GetModelName() string      { return e.ModelName }
func (e *BlockStoredEvent) setModelName(name string)  { e.ModelName = name }
func (e *BlockStoredEvent) GetPodName() string        { return e.PodName }
func (e *BlockStoredEvent) setPodName(name string)    { e.PodName = name }

// BlockRemovedEvent represents blocks being removed from KV cache
// ------------------------------------------------------------
// BlockRemoved (msgspec order)
// Python: [tag, block_hashes, medium]
// ------------------------------------------------------------
//
// lora_id is unused for now.
//
// Note: BlockHashes are converted at decode time:
// - vLLM legacy format (int64) → stored as-is
// - vLLM new format (32-byte SHA-256 from PR #23673) → first 8 bytes converted to int64
type BlockRemovedEvent struct {
	_           struct{}  `msgpack:",array"`
	Type        EventType `msgpack:"-"`
	BlockHashes []int64   // Decoded from vLLM, supports both old and new formats

	// NOTE: These are NOT part of msgpack
	Timestamp time.Time `msgpack:"-"`
	ModelName string    `msgpack:"-"`
	PodName   string    `msgpack:"-"`
}

func (e *BlockRemovedEvent) GetType() EventType        { return e.Type }
func (e *BlockRemovedEvent) GetTimestamp() time.Time   { return e.Timestamp }
func (e *BlockRemovedEvent) setTimestamp(ts time.Time) { e.Timestamp = ts }
func (e *BlockRemovedEvent) GetModelName() string      { return e.ModelName }
func (e *BlockRemovedEvent) setModelName(name string)  { e.ModelName = name }
func (e *BlockRemovedEvent) GetPodName() string        { return e.PodName }
func (e *BlockRemovedEvent) setPodName(name string)    { e.PodName = name }

// AllBlocksClearedEvent represents all blocks being cleared
// ------------------------------------------------------------
// AllBlocksCleared (msgspec order)
// Python: [tag]
// ------------------------------------------------------------
type AllBlocksClearedEvent struct {
	_    struct{}  `msgpack:",array"`
	Type EventType `msgpack:"-"`

	// NOTE: These are NOT part of msgpack
	Timestamp time.Time `msgpack:"-"`
	ModelName string    `msgpack:"-"`
	PodName   string    `msgpack:"-"`
}

func (e *AllBlocksClearedEvent) GetType() EventType        { return e.Type }
func (e *AllBlocksClearedEvent) GetTimestamp() time.Time   { return e.Timestamp }
func (e *AllBlocksClearedEvent) setTimestamp(ts time.Time) { e.Timestamp = ts }
func (e *AllBlocksClearedEvent) GetModelName() string      { return e.ModelName }
func (e *AllBlocksClearedEvent) setModelName(name string)  { e.ModelName = name }
func (e *AllBlocksClearedEvent) GetPodName() string        { return e.PodName }
func (e *AllBlocksClearedEvent) setPodName(name string)    { e.PodName = name }

// EventBatch represents a batch of events from vLLM
// ------------------------------------------------------------
// Batch object
// Python msgspec.Struct(array_like=True):
//
// EventBatch encoded as:
//
//	[ ts, [<event1>, <event2>, ...] ]
//
// ------------------------------------------------------------
type EventBatch struct {
	Timestamp time.Time
	Events    []KVEvent
}
