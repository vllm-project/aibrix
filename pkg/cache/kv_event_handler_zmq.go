//go:build zmq

/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"k8s.io/klog/v2"
)

// kvEventHandler implements the EventHandler interface
type kvEventHandler struct {
	manager   *KVEventManager
	podKey    string
	modelName string
}

// HandleEvent processes a KV cache event
func (h *kvEventHandler) HandleEvent(event kvcache.KVEvent) error {
	switch e := event.(type) {
	case *kvcache.BlockStoredEvent:
		return h.handleBlockStored(e)
	case *kvcache.BlockRemovedEvent:
		return h.handleBlockRemoved(e)
	case *kvcache.AllBlocksClearedEvent:
		return h.handleAllBlocksCleared(e)
	default:
		klog.Warningf("Unknown event type: %T", event)
		return nil
	}
}

// handleBlockStored processes BlockStored events
func (h *kvEventHandler) handleBlockStored(event *kvcache.BlockStoredEvent) error {
	// Get sync indexer from store
	syncIndexer := h.manager.store.GetSyncPrefixIndexer()
	if syncIndexer == nil {
		return fmt.Errorf("sync indexer not available")
	}

	// Convert to sync indexer event format
	syncEvent := syncindexer.BlockStored{
		BlockHashes:     event.BlockHashes,
		ModelName:       h.modelName,
		LoraID:          h.getLoraID(),
		SourcePod:       h.podKey,
		ParentBlockHash: event.ParentBlockHash, // Use the parent hash from event
	}

	// Convert token IDs to byte arrays
	syncEvent.Tokens = make([][]byte, len(event.TokenIDs))
	for i, tokenIDs := range event.TokenIDs {
		syncEvent.Tokens[i] = tokenIDsToBytes(tokenIDs)
	}

	// Process event
	if err := syncIndexer.ProcessBlockStored(syncEvent); err != nil {
		klog.Errorf("Failed to process BlockStored event: %v", err)
		return err
	}

	klog.V(4).Infof("Processed BlockStored event: %d blocks for pod %s",
		len(event.BlockHashes), h.podKey)

	return nil
}

// handleBlockRemoved processes BlockRemoved events
func (h *kvEventHandler) handleBlockRemoved(event *kvcache.BlockRemovedEvent) error {
	// Get sync indexer from store
	syncIndexer := h.manager.store.GetSyncPrefixIndexer()
	if syncIndexer == nil {
		return fmt.Errorf("sync indexer not available")
	}

	syncEvent := syncindexer.BlockRemoved{
		BlockHashes: event.BlockHashes,
		ModelName:   h.modelName,
		LoraID:      h.getLoraID(),
		SourcePod:   h.podKey,
	}

	if err := syncIndexer.ProcessBlockRemoved(syncEvent); err != nil {
		klog.Errorf("Failed to process BlockRemoved event: %v", err)
		return err
	}

	klog.V(4).Infof("Processed BlockRemoved event: %d blocks for pod %s",
		len(event.BlockHashes), h.podKey)

	return nil
}

// handleAllBlocksCleared processes AllBlocksCleared events
func (h *kvEventHandler) handleAllBlocksCleared(event *kvcache.AllBlocksClearedEvent) error {
	// Current implementation is a no-op as per requirements
	klog.V(4).Infof("Received AllBlocksCleared event for pod %s (not implemented)", h.podKey)
	return nil
}

// getLoraID extracts LoRA ID from pod metadata in a thread-safe manner
func (h *kvEventHandler) getLoraID() int64 {
	// Thread-safe access to pod metadata
	h.manager.mu.RLock()
	defer h.manager.mu.RUnlock()

	// Look up pod from cache
	if metaPod, exists := h.manager.store.metaPods.Load(h.podKey); exists {
		// Extract LoRA ID from labels
		if loraStr, exists := metaPod.Pod.Labels["model.aibrix.ai/lora-id"]; exists {
			if loraID, err := strconv.ParseInt(loraStr, 10, 64); err == nil {
				return loraID
			}
		}
	}

	return -1 // Default: no LoRA adapter
}

// tokenIDsToBytes converts int32 token IDs to byte array
func tokenIDsToBytes(tokenIDs []int32) []byte {
	bytes := make([]byte, len(tokenIDs)*4)
	for i, id := range tokenIDs {
		binary.BigEndian.PutUint32(bytes[i*4:], uint32(id))
	}
	return bytes
}
