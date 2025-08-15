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

// Package cache adapter implementations for kvevent interfaces.
//
// IMPORTANT: The adapter pattern used here is intentional and necessary.
// DO NOT attempt to "simplify" by removing these adapters.
//
// Why these adapters exist:
//
// 1. Anti-Corruption Layer: These adapters prevent the kvevent package
//    from leaking into other packages and vice versa.
//
// 2. Type Conversion: Go doesn't allow direct conversion between identical
//    structs from different packages. The syncIndexerAdapter handles the
//    conversion between kvevent.BlockStoredEvent and syncindexer.BlockStored.
//
// 3. Avoiding Circular Dependencies: Any attempt to make syncprefixcacheindexer
//    directly implement kvevent interfaces would create a circular dependency.
//
// Architecture:
//
//   pkg/cache (this package) - Higher-level orchestrator
//        |
//        +-- Uses kvevent.Manager
//        |
//        +-- Implements kvevent interfaces via adapters:
//            - storeProviderAdapter (for PodProvider, SyncIndexProvider)
//            - syncIndexerAdapter (for SyncIndexer)
//
// The adapters allow pkg/kvevent and pkg/utils/syncprefixcacheindexer to
// remain completely independent of each other.

package cache

import (
	"context"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

// storeProviderAdapter bridges the Store implementation with the kvevent interfaces.
// This adapter pattern allows the kvevent package to remain decoupled from the
// specific implementation details of the cache Store.
//
// The adapter provides:
//   - PodProvider: Access to pod information via GetPod and RangePods
//   - SyncIndexProvider: Access to the sync indexer for cache synchronization
//
// This design breaks the circular dependency that existed when KVEventManager
// was directly part of the cache package.
type storeProviderAdapter struct {
	store *Store
}

// Compile-time verification
var _ kvevent.PodProvider = (*storeProviderAdapter)(nil)
var _ kvevent.SyncIndexProvider = (*storeProviderAdapter)(nil)

// GetPod implements kvevent.PodProvider interface
func (a *storeProviderAdapter) GetPod(ctx context.Context, podKey string) (*kvevent.PodInfo, bool) {
	// Check context
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}

	// Load from SyncMap
	metaPod, exists := a.store.metaPods.Load(podKey)
	if !exists {
		return nil, false
	}

	// Create lightweight copy
	return &kvevent.PodInfo{
		Name:      metaPod.Pod.Name,
		Namespace: metaPod.Pod.Namespace,
		PodIP:     metaPod.Pod.Status.PodIP,
		ModelName: metaPod.Pod.Labels[constants.ModelLabelName],
		Labels:    metaPod.Pod.Labels, // Shallow copy - labels are read-only
		Models:    metaPod.Models.Array(),
	}, true
}

// RangePods implements kvevent.PodProvider interface
func (a *storeProviderAdapter) RangePods(ctx context.Context, f func(key string, pod *kvevent.PodInfo) bool) error {
	var err error

	a.store.metaPods.Range(func(key string, metaPod *Pod) bool {
		// Check context
		select {
		case <-ctx.Done():
			err = ctx.Err()
			return false
		default:
		}

		// Create lightweight copy
		podInfo := &kvevent.PodInfo{
			Name:      metaPod.Pod.Name,
			Namespace: metaPod.Pod.Namespace,
			PodIP:     metaPod.Pod.Status.PodIP,
			ModelName: metaPod.Pod.Labels[constants.ModelLabelName],
			Labels:    metaPod.Pod.Labels,
			Models:    metaPod.Models.Array(),
		}

		return f(key, podInfo)
	})

	return err
}

// GetSyncIndexer implements kvevent.SyncIndexProvider interface
func (a *storeProviderAdapter) GetSyncIndexer(ctx context.Context) (kvevent.SyncIndexer, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if a.store.syncPrefixIndexer == nil {
		return nil, kvevent.ErrIndexerNotInitialized
	}

	// Wrap the existing syncPrefixIndexer with an adapter
	// to convert between kvevent and syncindexer event types
	return &syncIndexerAdapter{indexer: a.store.syncPrefixIndexer}, nil
}

// NewStoreProviderAdapter creates a new adapter for Store
func NewStoreProviderAdapter(store *Store) (kvevent.PodProvider, kvevent.SyncIndexProvider) {
	adapter := &storeProviderAdapter{store: store}
	return adapter, adapter
}

// syncIndexerAdapter adapts the existing SyncPrefixHashTable to implement kvevent.SyncIndexer.
//
// This adapter is NECESSARY and should NOT be removed. It serves as an
// Anti-Corruption Layer that:
//  1. Converts between kvevent types and syncindexer types
//  2. Prevents circular dependencies between packages
//  3. Maintains clean architectural boundaries
//
// Without this adapter, we would need to either:
//   - Make syncprefixcacheindexer depend on kvevent (circular dependency)
//   - Merge all packages together (violates separation of concerns)
type syncIndexerAdapter struct {
	indexer *syncindexer.SyncPrefixHashTable
}

// ProcessBlockStored implements kvevent.SyncIndexer interface
func (a *syncIndexerAdapter) ProcessBlockStored(ctx context.Context, event kvevent.BlockStoredEvent) error {
	// Convert to syncindexer event - types now match directly
	syncEvent := syncindexer.BlockStored{
		BlockHashes:     event.BlockHashes,
		ModelName:       event.ModelName,
		LoraID:          event.LoraID,
		SourcePod:       event.SourcePod,
		ParentBlockHash: event.ParentBlockHash,
		Tokens:          event.Tokens,
	}

	return a.indexer.ProcessBlockStored(syncEvent)
}

// ProcessBlockRemoved implements kvevent.SyncIndexer interface
func (a *syncIndexerAdapter) ProcessBlockRemoved(ctx context.Context, event kvevent.BlockRemovedEvent) error {
	// Convert to syncindexer event - types now match directly
	syncEvent := syncindexer.BlockRemoved{
		BlockHashes: event.BlockHashes,
		ModelName:   event.ModelName,
		LoraID:      event.LoraID,
		SourcePod:   event.SourcePod,
	}

	return a.indexer.ProcessBlockRemoved(syncEvent)
}

// RemovePrefix implements kvevent.SyncIndexer interface
func (a *syncIndexerAdapter) RemovePrefix(ctx context.Context, modelName string, loraID int64, podKey string) error {
	return a.indexer.RemovePrefix(modelName, loraID, podKey)
}
