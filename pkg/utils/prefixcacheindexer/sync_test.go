/*
Copyright 2026 The Aibrix Team.

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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helpers

func newTableWithBlocks(t *testing.T) (*PrefixHashTable, []uint64) {
	t.Helper()
	originalBlockSize := prefixCacheBlockSize
	t.Cleanup(func() { prefixCacheBlockSize = originalBlockSize })
	prefixCacheBlockSize = 4

	cache := NewPrefixHashTable()
	_, hashes := cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8}, "model1", getReadyPods())
	cache.AddPrefix(hashes, "model1", "pod1")
	return cache, hashes
}

// encodeBlock / decodeBlock

func TestEncodeDecodeBlock_RoundTrip(t *testing.T) {
	now := time.Unix(0, time.Now().UnixNano()) // truncate to nanos
	b := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": now, "pod2": now.Add(time.Second)},
			"model2": {"pod3": now.Add(2 * time.Second)},
		},
	}
	data, err := encodeBlock(b)
	require.NoError(t, err)

	got, err := decodeBlock(data)
	require.NoError(t, err)

	assert.Equal(t, len(b.modelToPods), len(got.modelToPods))
	for model, pods := range b.modelToPods {
		for pod, ts := range pods {
			assert.Equal(t, ts.UnixNano(), got.modelToPods[model][pod].UnixNano())
		}
	}
}

func TestEncodeDecodeBlock_Empty(t *testing.T) {
	b := Block{modelToPods: map[string]map[string]time.Time{}}
	data, err := encodeBlock(b)
	require.NoError(t, err)

	got, err := decodeBlock(data)
	require.NoError(t, err)
	assert.Empty(t, got.modelToPods)
}

func TestDecodeBlock_InvalidJSON(t *testing.T) {
	_, err := decodeBlock([]byte("not-json"))
	assert.Error(t, err)
}

// GetSnapshotForSync

func TestGetSnapshotForSync_ReturnsAllBlocks(t *testing.T) {
	cache, hashes := newTableWithBlocks(t)

	snap, err := cache.GetSnapshotForSync(context.Background())
	require.NoError(t, err)
	assert.Len(t, snap, len(hashes))

	for _, h := range hashes {
		key := strconv.FormatUint(h, 10)
		assert.Contains(t, snap, key)
	}
}

func TestGetSnapshotForSync_CancelledContext(t *testing.T) {
	cache, _ := newTableWithBlocks(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := cache.GetSnapshotForSync(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGetSnapshotForSync_EmptyTable(t *testing.T) {
	cache := NewPrefixHashTable()
	snap, err := cache.GetSnapshotForSync(context.Background())
	require.NoError(t, err)
	assert.Empty(t, snap)
}

// EncodeBlockForSync

func TestEncodeBlockForSync_Present(t *testing.T) {
	cache, hashes := newTableWithBlocks(t)

	data, ok := cache.EncodeBlockForSync(hashes[0])
	assert.True(t, ok)
	assert.NotEmpty(t, data)

	// must decode back cleanly
	block, err := decodeBlock(data)
	require.NoError(t, err)
	assert.NotEmpty(t, block.modelToPods)
}

func TestEncodeBlockForSync_Missing(t *testing.T) {
	cache := NewPrefixHashTable()
	data, ok := cache.EncodeBlockForSync(99999999)
	assert.False(t, ok)
	assert.Nil(t, data)
}

// GetDeltaForSync

func TestGetDeltaForSync_ReturnsOnlyDirty(t *testing.T) {
	cache, hashes := newTableWithBlocks(t)

	updated, deleted, err := cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.Nil(t, deleted)
	assert.Len(t, updated, len(hashes))

	for _, h := range hashes {
		key := strconv.FormatUint(h, 10)
		assert.Contains(t, updated, key)
	}
}

func TestGetDeltaForSync_EmptyWhenNoDirty(t *testing.T) {
	cache := NewPrefixHashTable()
	updated, deleted, err := cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.Nil(t, updated)
	assert.Nil(t, deleted)
}

func TestGetDeltaForSync_CancelledContext(t *testing.T) {
	cache, _ := newTableWithBlocks(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := cache.GetDeltaForSync(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// ClearDirtyForSync

func TestClearDirtyForSync_ClearsDirtySet(t *testing.T) {
	cache, _ := newTableWithBlocks(t)

	// dirty set should be non-empty
	updated, _, err := cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, updated)

	require.NoError(t, cache.ClearDirtyForSync(context.Background()))

	updated, deleted, err := cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.Nil(t, updated)
	assert.Nil(t, deleted)
}

// ApplyRemoteForSync

func TestApplyRemoteForSync_NewBlock(t *testing.T) {
	cache := NewPrefixHashTable()

	// Create a block and encode it
	ts := time.Unix(0, time.Now().UnixNano())
	b := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": ts},
		},
	}
	data, err := encodeBlock(b)
	require.NoError(t, err)

	id := strconv.FormatUint(42, 10)
	require.NoError(t, cache.ApplyRemoteForSync(context.Background(), id, data))

	// Block should now be present
	got, ok := cache.store.Get(42)
	assert.True(t, ok)
	assert.Equal(t, ts.UnixNano(), got.modelToPods["model1"]["pod1"].UnixNano())
}

func TestApplyRemoteForSync_MergesNewerTimestamps(t *testing.T) {
	cache := NewPrefixHashTable()

	older := time.Unix(1000, 0)
	newer := time.Unix(2000, 0)

	// Seed local block with older timestamp
	localBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": older},
		},
	}
	cache.store.Put(1, localBlock)

	// Remote block has newer timestamp for same pod
	remoteBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": newer},
		},
	}
	data, err := encodeBlock(remoteBlock)
	require.NoError(t, err)

	id := strconv.FormatUint(1, 10)
	require.NoError(t, cache.ApplyRemoteForSync(context.Background(), id, data))

	got, ok := cache.store.Get(1)
	assert.True(t, ok)
	assert.Equal(t, newer.UnixNano(), got.modelToPods["model1"]["pod1"].UnixNano())
}

func TestApplyRemoteForSync_KeepsLocalIfNewer(t *testing.T) {
	cache := NewPrefixHashTable()

	older := time.Unix(1000, 0)
	newer := time.Unix(2000, 0)

	// Seed local block with newer timestamp
	localBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": newer},
		},
	}
	cache.store.Put(1, localBlock)

	// Remote block has older timestamp
	remoteBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": older},
		},
	}
	data, err := encodeBlock(remoteBlock)
	require.NoError(t, err)

	id := strconv.FormatUint(1, 10)
	require.NoError(t, cache.ApplyRemoteForSync(context.Background(), id, data))

	got, ok := cache.store.Get(1)
	assert.True(t, ok)
	// local (newer) must be preserved
	assert.Equal(t, newer.UnixNano(), got.modelToPods["model1"]["pod1"].UnixNano())
}

func TestApplyRemoteForSync_MergesNewModel(t *testing.T) {
	cache := NewPrefixHashTable()

	ts := time.Unix(1000, 0)
	localBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model1": {"pod1": ts},
		},
	}
	cache.store.Put(1, localBlock)

	// Remote adds a new model entry
	remoteBlock := Block{
		modelToPods: map[string]map[string]time.Time{
			"model2": {"pod2": ts},
		},
	}
	data, err := encodeBlock(remoteBlock)
	require.NoError(t, err)

	require.NoError(t, cache.ApplyRemoteForSync(context.Background(), "1", data))

	got, ok := cache.store.Get(1)
	assert.True(t, ok)
	assert.Contains(t, got.modelToPods, "model1")
	assert.Contains(t, got.modelToPods, "model2")
}

func TestApplyRemoteForSync_InvalidID(t *testing.T) {
	cache := NewPrefixHashTable()
	err := cache.ApplyRemoteForSync(context.Background(), "not-a-number", []byte(`{}`))
	assert.Error(t, err)
}

func TestApplyRemoteForSync_InvalidData(t *testing.T) {
	cache := NewPrefixHashTable()
	err := cache.ApplyRemoteForSync(context.Background(), "1", []byte("bad-json"))
	assert.Error(t, err)
}

// PrefixHashTableSyncable

func TestPrefixHashTableSyncable_Namespace(t *testing.T) {
	cache := NewPrefixHashTable()
	s := NewPrefixHashTableSyncable(cache)
	assert.Equal(t, prefixCacheNamespace, s.Namespace())
}

func TestPrefixHashTableSyncable_GetSnapshot(t *testing.T) {
	cache, hashes := newTableWithBlocks(t)
	s := NewPrefixHashTableSyncable(cache)

	snap, err := s.GetSnapshot(context.Background())
	require.NoError(t, err)
	assert.Len(t, snap, len(hashes))
}

func TestPrefixHashTableSyncable_ApplyRemote(t *testing.T) {
	cache := NewPrefixHashTable()
	s := NewPrefixHashTableSyncable(cache)

	ts := time.Unix(1000, 0)
	b := Block{modelToPods: map[string]map[string]time.Time{"m": {"p": ts}}}
	data, err := encodeBlock(b)
	require.NoError(t, err)

	require.NoError(t, s.ApplyRemote(context.Background(), "7", data))

	got, ok := cache.store.Get(7)
	assert.True(t, ok)
	assert.Equal(t, ts.UnixNano(), got.modelToPods["m"]["p"].UnixNano())
}

func TestPrefixHashTableSyncable_GetDeltaAndClearDirty(t *testing.T) {
	cache, hashes := newTableWithBlocks(t)
	s := NewPrefixHashTableSyncable(cache)

	// Cast to DeltaSyncable
	ds, ok := s.(interface {
		GetDelta(ctx context.Context) (map[string][]byte, []string, error)
		ClearDirty(ctx context.Context) error
	})
	require.True(t, ok)

	updated, deleted, err := ds.GetDelta(context.Background())
	require.NoError(t, err)
	assert.Len(t, updated, len(hashes))
	assert.Nil(t, deleted)

	require.NoError(t, ds.ClearDirty(context.Background()))

	updated, _, err = ds.GetDelta(context.Background())
	require.NoError(t, err)
	assert.Nil(t, updated)
}

// Delta is empty after clear, repopulated after AddPrefix

func TestDeltaLifecycle(t *testing.T) {
	originalBlockSize := prefixCacheBlockSize
	t.Cleanup(func() { prefixCacheBlockSize = originalBlockSize })
	prefixCacheBlockSize = 4

	cache := NewPrefixHashTable()

	_, h1 := cache.MatchPrefix([]byte{1, 2, 3, 4}, "m", getReadyPods())
	cache.AddPrefix(h1, "m", "p1")

	updated, _, err := cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.Len(t, updated, 1)

	require.NoError(t, cache.ClearDirtyForSync(context.Background()))

	_, h2 := cache.MatchPrefix([]byte{5, 6, 7, 8}, "m", getReadyPods())
	cache.AddPrefix(h2, "m", "p2")

	updated, _, err = cache.GetDeltaForSync(context.Background())
	require.NoError(t, err)
	assert.Len(t, updated, 1)
	assert.Contains(t, updated, strconv.FormatUint(h2[0], 10))
}
