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

package modelclaim

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestRuntimeSnapshotCacheUsesFreshEntry(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	cache := newRuntimeSnapshotCache(5*time.Second, func() time.Time { return now })
	key := types.NamespacedName{Namespace: "default", Name: "warm-1"}
	calls := 0
	fetch := func() (*RuntimeSnapshot, error) {
		calls++
		return &RuntimeSnapshot{CachedArtifacts: []string{"hf://Org/M1"}}, nil
	}

	first, ok := cache.Get(key, types.UID("pod-uid"), fetch)
	require.True(t, ok)
	assert.Equal(t, []string{"hf://Org/M1"}, first.CachedArtifacts)
	second, ok := cache.Get(key, types.UID("pod-uid"), fetch)
	require.True(t, ok)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, calls)
}

func TestRuntimeSnapshotCacheDropsExpiredEntryAfterRefreshFailure(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	cache := newRuntimeSnapshotCache(5*time.Second, func() time.Time { return now })
	key := types.NamespacedName{Namespace: "default", Name: "warm-1"}

	_, ok := cache.Get(key, types.UID("pod-uid"), func() (*RuntimeSnapshot, error) {
		return &RuntimeSnapshot{}, nil
	})
	require.True(t, ok)
	now = now.Add(6 * time.Second)

	snapshot, ok := cache.Get(key, types.UID("pod-uid"), func() (*RuntimeSnapshot, error) {
		return nil, errors.New("runtime unavailable")
	})
	assert.False(t, ok)
	assert.Nil(t, snapshot)
}

func TestRuntimeSnapshotCacheRefreshesWhenPodIsRecreated(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	cache := newRuntimeSnapshotCache(time.Hour, func() time.Time { return now })
	key := types.NamespacedName{Namespace: "default", Name: "warm-1"}
	calls := 0
	fetch := func() (*RuntimeSnapshot, error) {
		calls++
		return &RuntimeSnapshot{}, nil
	}

	_, ok := cache.Get(key, types.UID("old-pod"), fetch)
	require.True(t, ok)
	_, ok = cache.Get(key, types.UID("new-pod"), fetch)
	require.True(t, ok)
	assert.Equal(t, 2, calls)
}

func TestPlacementStateFromSnapshot(t *testing.T) {
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 800},
			{ID: "GPU-1", HBMFreeBytes: 300},
		},
		Models: []RuntimeSnapshotModel{
			{ModelName: "m1", KVUsedBytes: 10},
			{ModelName: "m2", KVUsedBytes: 25},
		},
		CachedArtifacts: []string{"hf://Org/M1"},
	}

	singleGPUState := placementStateFromSnapshot(snapshot, "hf://Org/M1", 1)
	groupState := placementStateFromSnapshot(snapshot, "hf://Org/M1", 2)

	assert.True(t, singleGPUState.SnapshotKnown)
	assert.True(t, singleGPUState.ArtifactCached)
	assert.True(t, singleGPUState.MemoryKnown)
	assert.Equal(t, int64(800), singleGPUState.HBMFreeBytes)
	assert.Equal(t, int64(300), groupState.HBMFreeBytes)
	assert.Equal(t, int64(35), groupState.KVUsedBytes)
	assert.Equal(t, 2, groupState.ModelCount)
}
