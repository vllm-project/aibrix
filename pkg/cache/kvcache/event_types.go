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

import "time"

// EventType represents the type of KV cache event
type EventType string

// Note on Token Representation:
// - vLLM sends token IDs as []int32 arrays
// - Gateway expects tokens as []byte for hashing
// - Conversion: Each int32 is encoded as 4 bytes in big-endian format
// - Example: []int32{1, 2} becomes []byte{0, 0, 0, 1, 0, 0, 0, 2}

const (
	// EventTypeBlockStored indicates that blocks have been stored in the KV cache
	EventTypeBlockStored EventType = "BLOCK_STORED"

	// EventTypeBlockRemoved indicates that blocks have been removed from the KV cache
	EventTypeBlockRemoved EventType = "BLOCK_REMOVED"

	// EventTypeAllCleared indicates that all blocks have been cleared from the cache
	EventTypeAllCleared EventType = "ALL_BLOCKS_CLEARED"
)

// KVEvent is the base interface for all KV cache events
type KVEvent interface {
	GetType() EventType
	GetTimestamp() time.Time
}

// BlockStoredEvent represents blocks being stored in KV cache
type BlockStoredEvent struct {
	Type            EventType `msgpack:"type"`
	Timestamp       time.Time `msgpack:"timestamp"`
	BlockHashes     []int64   `msgpack:"block_hashes"`
	TokenIDs        [][]int32 `msgpack:"token_ids"`                   // One array per block
	ParentBlockHash *int64    `msgpack:"parent_block_hash,omitempty"` // Parent hash for chaining
	ModelName       string    `msgpack:"model_name"`
	PodName         string    `msgpack:"-"` // Set by subscriber
}

// GetType returns the event type
func (e *BlockStoredEvent) GetType() EventType {
	return e.Type
}

// GetTimestamp returns the event timestamp
func (e *BlockStoredEvent) GetTimestamp() time.Time {
	return e.Timestamp
}

// BlockRemovedEvent represents blocks being removed from KV cache
type BlockRemovedEvent struct {
	Type        EventType `msgpack:"type"`
	Timestamp   time.Time `msgpack:"timestamp"`
	BlockHashes []int64   `msgpack:"block_hashes"`
	ModelName   string    `msgpack:"model_name"`
	PodName     string    `msgpack:"-"` // Set by subscriber
}

// GetType returns the event type
func (e *BlockRemovedEvent) GetType() EventType {
	return e.Type
}

// GetTimestamp returns the event timestamp
func (e *BlockRemovedEvent) GetTimestamp() time.Time {
	return e.Timestamp
}

// AllBlocksClearedEvent represents all blocks being cleared
type AllBlocksClearedEvent struct {
	Type      EventType `msgpack:"type"`
	Timestamp time.Time `msgpack:"timestamp"`
	ModelName string    `msgpack:"model_name"`
	PodName   string    `msgpack:"-"` // Set by subscriber
}

// GetType returns the event type
func (e *AllBlocksClearedEvent) GetType() EventType {
	return e.Type
}

// GetTimestamp returns the event timestamp
func (e *AllBlocksClearedEvent) GetTimestamp() time.Time {
	return e.Timestamp
}

// EventBatch represents a batch of events from vLLM
type EventBatch struct {
	Events []KVEvent `msgpack:"events"`
}
