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
)

// PodProvider provides access to pod information.
// Implementations must be thread-safe.
type PodProvider interface {
	// GetPod retrieves pod information by key.
	// Returns false if pod not found.
	GetPod(ctx context.Context, podKey string) (*PodInfo, bool)

	// RangePods iterates over all pods.
	// The callback function should return false to stop iteration.
	// Returns error if context is cancelled or other errors occur.
	RangePods(ctx context.Context, f func(key string, pod *PodInfo) bool) error
}

// PodInfo contains essential pod information.
// This is a lightweight copy to avoid passing full Pod objects.
// Consumers of this struct MUST treat the Labels map as read-only
// to prevent data races, as it is a shallow copy.
type PodInfo struct {
	// Basic pod information
	Name      string
	Namespace string
	PodIP     string
	ModelName string

	// Labels are shared (not deep copied) as they are read-only
	// DO NOT modify this map - it's a shallow copy from the original Pod
	Labels map[string]string

	// Models list from the internal registry
	Models []string
}

// SyncIndexProvider provides access to sync indexer functionality.
// Implementations must be thread-safe.
type SyncIndexProvider interface {
	// GetSyncIndexer returns the sync indexer.
	// Returns ErrIndexerNotInitialized if indexer is not ready.
	GetSyncIndexer(ctx context.Context) (SyncIndexer, error)
}

// SyncIndexer defines sync indexing operations.
// Implementations must be thread-safe.
type SyncIndexer interface {
	ProcessBlockStored(ctx context.Context, event BlockStoredEvent) error
	ProcessBlockRemoved(ctx context.Context, event BlockRemovedEvent) error
	RemovePrefix(ctx context.Context, modelName string, loraID int64, podKey string) error
}

// Event types for sync indexer
// These types mirror the kvcache event types but with necessary conversions:
// - TokenIDs ([][]int32) are converted to Tokens ([][]byte) for storage
type BlockStoredEvent struct {
	BlockHashes     []int64
	ModelName       string
	LoraID          int64
	SourcePod       string
	ParentBlockHash *int64
	Tokens          [][]byte // Converted from [][]int32 TokenIDs
}

type BlockRemovedEvent struct {
	BlockHashes []int64
	ModelName   string
	LoraID      int64
	SourcePod   string
}