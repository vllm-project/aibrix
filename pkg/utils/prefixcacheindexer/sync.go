/*
Copyright 2024 The Aibrix Team.

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

package prefixcacheindexer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils/syncable"
)

// blockWire is the serialized form of Block for Redis (time as Unix nano).
type blockWire struct {
	ModelToPods map[string]map[string]int64 `json:"model_to_pods"`
}

func encodeBlock(b Block) ([]byte, error) {
	w := blockWire{ModelToPods: make(map[string]map[string]int64)}
	for model, pods := range b.modelToPods {
		w.ModelToPods[model] = make(map[string]int64)
		for pod, t := range pods {
			w.ModelToPods[model][pod] = t.UnixNano()
		}
	}
	return json.Marshal(w)
}

func decodeBlock(data []byte) (Block, error) {
	var w blockWire
	if err := json.Unmarshal(data, &w); err != nil {
		return Block{}, err
	}
	b := Block{modelToPods: make(map[string]map[string]time.Time)}
	for model, pods := range w.ModelToPods {
		b.modelToPods[model] = make(map[string]time.Time)
		for pod, nano := range pods {
			b.modelToPods[model][pod] = time.Unix(0, nano)
		}
	}
	return b, nil
}

// GetSnapshotForSync returns the current prefix cache state as id -> serialized Block
// for use by a Redis sync layer. Caller must not modify the returned map.
func (c *PrefixHashTable) GetSnapshotForSync(ctx context.Context) (map[string][]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string][]byte)
	c.store.Range(func(blockHash uint64, block Block) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		data, err := encodeBlock(block)
		if err != nil {
			return false
		}
		out[strconv.FormatUint(blockHash, 10)] = data
		return true
	})
	return out, ctx.Err()
}

// EncodeBlockForSync returns the serialized form of the block for the given
// block hash, or nil if not present. Use for write-through after AddPrefix.
func (c *PrefixHashTable) EncodeBlockForSync(blockHash uint64) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	block, ok := c.store.Get(blockHash)
	if !ok {
		return nil, false
	}
	data, err := encodeBlock(block)
	if err != nil {
		return nil, false
	}
	return data, true
}

// GetDeltaForSync returns only entities that changed since last ClearDirtyForSync
// (updated id->bytes; deleted is nil — evictions are not tracked).
func (c *PrefixHashTable) GetDeltaForSync(ctx context.Context) (updated map[string][]byte, deleted []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirtyIds) == 0 {
		return nil, nil, nil
	}
	updated = make(map[string][]byte, len(c.dirtyIds))
	for id := range c.dirtyIds {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		blockHash, e := strconv.ParseUint(id, 10, 64)
		if e != nil {
			continue
		}
		block, ok := c.store.Get(blockHash)
		if !ok {
			continue // evicted since marked dirty; skip or could add to deleted
		}
		data, e := encodeBlock(block)
		if e != nil {
			continue
		}
		updated[id] = data
	}
	return updated, nil, nil
}

// ClearDirtyForSync clears the dirty set after a successful delta push.
func (c *PrefixHashTable) ClearDirtyForSync(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirtyIds = make(map[string]struct{})
	return nil
}

// ApplyRemoteForSync applies one entity from remote (Redis) into the local store.
// id is the block hash as string; data is the serialized Block.
func (c *PrefixHashTable) ApplyRemoteForSync(ctx context.Context, id string, data []byte) error {
	blockHash, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid block id %q: %w", id, err)
	}
	block, err := decodeBlock(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.store.Get(blockHash)
	if ok {
		// Merge: keep newer of each pod's last access time per model
		for model, pods := range block.modelToPods {
			if existing.modelToPods[model] == nil {
				existing.modelToPods[model] = make(map[string]time.Time)
			}
			for pod, t := range pods {
				if cur, has := existing.modelToPods[model][pod]; !has || t.After(cur) {
					existing.modelToPods[model][pod] = t
				}
			}
		}
		c.store.Put(blockHash, existing)
	} else {
		c.store.Put(blockHash, block)
	}
	return nil
}

const prefixCacheNamespace = "prefixcache"

// PrefixHashTableSyncable adapts PrefixHashTable to syncable.Syncable so it can
// be registered with redissync.Manager for cross-replica sync.
type PrefixHashTableSyncable struct {
	Table *PrefixHashTable
}

// NewPrefixHashTableSyncable returns a Syncable for the given table. Register
// it with redissync.Manager; use EncodeBlockForSync for write-through after AddPrefix.
func NewPrefixHashTableSyncable(table *PrefixHashTable) syncable.Syncable {
	return &PrefixHashTableSyncable{Table: table}
}

// Namespace implements syncable.Syncable.
func (p *PrefixHashTableSyncable) Namespace() string {
	return prefixCacheNamespace
}

// GetSnapshot implements syncable.Syncable.
func (p *PrefixHashTableSyncable) GetSnapshot(ctx context.Context) (map[string][]byte, error) {
	return p.Table.GetSnapshotForSync(ctx)
}

// ApplyRemote implements syncable.Syncable.
func (p *PrefixHashTableSyncable) ApplyRemote(ctx context.Context, id string, data []byte) error {
	return p.Table.ApplyRemoteForSync(ctx, id, data)
}

// GetDelta implements syncable.DeltaSyncable (delta push instead of full snapshot).
func (p *PrefixHashTableSyncable) GetDelta(ctx context.Context) (map[string][]byte, []string, error) {
	return p.Table.GetDeltaForSync(ctx)
}

// ClearDirty implements syncable.DeltaSyncable.
func (p *PrefixHashTableSyncable) ClearDirty(ctx context.Context) error {
	return p.Table.ClearDirtyForSync(ctx)
}

// Ensure PrefixHashTableSyncable implements DeltaSyncable (optional interface).
var _ syncable.DeltaSyncable = (*PrefixHashTableSyncable)(nil)
