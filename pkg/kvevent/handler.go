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
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
)

// eventHandler implements kvcache.EventHandler interface
type eventHandler struct {
	manager   *Manager
	podKey    string
	modelName string
	loraID    int64
}

// HandleEvent processes KV cache events
func (h *eventHandler) HandleEvent(event kvcache.KVEvent) error {
	// Check if manager is stopped before using its context
	h.manager.mu.RLock()
	stopped := h.manager.stopped
	h.manager.mu.RUnlock()

	if stopped {
		return ErrManagerStopped
	}

	// Create context with timeout
	// Use 10s timeout for event processing as sync indexer operations
	// may involve Redis calls and network I/O under high load
	ctx, cancel := context.WithTimeout(h.manager.ctx, 10*time.Second)
	defer cancel()

	switch e := event.(type) {
	case *kvcache.BlockStoredEvent:
		return h.handleBlockStored(ctx, e)
	case *kvcache.BlockRemovedEvent:
		return h.handleBlockRemoved(ctx, e)
	case *kvcache.AllBlocksClearedEvent:
		return h.handleAllBlocksCleared(ctx, e)
	default:
		klog.Warningf("Unknown event type: %T", event)
		return nil
	}
}

func (h *eventHandler) handleBlockStored(ctx context.Context, event *kvcache.BlockStoredEvent) error {
	// Get sync indexer
	syncIndexer, err := h.manager.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		if IsTemporaryError(err) {
			klog.V(4).Infof("Temporary error getting sync indexer: %v", err)
			return nil // Don't fail on temporary errors
		}
		return fmt.Errorf("failed to get sync indexer: %w", err)
	}

	// Convert to sync event
	// Note: BlockHashes are already []int64 after msgpack decoding
	syncEvent := BlockStoredEvent{
		BlockHashes:     event.BlockHashes,
		ModelName:       h.modelName,
		LoraID:          h.loraID,
		SourcePod:       h.podKey,
		ParentBlockHash: event.ParentBlockHash,
		Tokens:          event.TokenIDs,
	}

	// Process event
	if err := syncIndexer.ProcessBlockStored(ctx, syncEvent); err != nil {
		klog.Errorf("Failed to process BlockStored event for pod %s: %v", h.podKey, err)
		return err
	}

	klog.V(4).Infof("Processed BlockStored event: %d blocks for pod %s",
		len(event.BlockHashes), h.podKey)

	return nil
}

func (h *eventHandler) handleBlockRemoved(ctx context.Context, event *kvcache.BlockRemovedEvent) error {
	// Get sync indexer
	syncIndexer, err := h.manager.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		if IsTemporaryError(err) {
			klog.V(4).Infof("Temporary error getting sync indexer: %v", err)
			return nil // Don't fail on temporary errors
		}
		return fmt.Errorf("failed to get sync indexer: %w", err)
	}

	// Convert to sync event
	// Note: BlockHashes are already []int64 after msgpack decoding
	syncEvent := BlockRemovedEvent{
		BlockHashes: event.BlockHashes,
		ModelName:   h.modelName,
		LoraID:      h.loraID,
		SourcePod:   h.podKey,
	}

	// Process event
	if err := syncIndexer.ProcessBlockRemoved(ctx, syncEvent); err != nil {
		klog.Errorf("Failed to process BlockRemoved event for pod %s: %v", h.podKey, err)
		return err
	}

	klog.V(4).Infof("Processed BlockRemoved event: %d blocks for pod %s",
		len(event.BlockHashes), h.podKey)

	return nil
}

func (h *eventHandler) handleAllBlocksCleared(ctx context.Context, event *kvcache.AllBlocksClearedEvent) error {
	// The engine wiped its entire prefix cache (/reset_prefix_cache, OOM-driven
	// reset, or /sleep level>=1). The pod stays Running and /health still returns
	// 200, so the pod-unsubscribe path that normally calls RemovePrefix never
	// fires. If we don't purge the pod's entries here, the prefix router keeps
	// preferring this pod for prefixes whose blocks are now gone, turning every
	// such match into a cold miss. See issue #2287.
	//
	// Note: events alone are not a complete fix (vLLM only flushes queued KV
	// events from inside a scheduler step, so an idle/sleeping engine may never
	// emit this, and ZMQ delivery is lossy). A metric-driven reconcile backstop
	// is tracked separately in #2287.
	syncIndexer, err := h.manager.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		if IsTemporaryError(err) {
			klog.V(4).Infof("Temporary error getting sync indexer: %v", err)
			return nil // Don't fail on temporary errors
		}
		return fmt.Errorf("failed to get sync indexer: %w", err)
	}

	if err := syncIndexer.RemovePrefix(ctx, h.modelName, h.loraID, h.podKey); err != nil {
		klog.Errorf("Failed to process AllBlocksCleared event for pod %s: %v", h.podKey, err)
		return err
	}

	klog.V(4).Infof("Processed AllBlocksCleared event: purged prefix entries for pod %s", h.podKey)

	return nil
}
