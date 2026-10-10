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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const defaultRuntimeSnapshotTTL = 5 * time.Second

type cachedRuntimeSnapshot struct {
	podUID    types.UID
	fetchedAt time.Time
	snapshot  *RuntimeSnapshot
}

// runtimeSnapshotCache bounds controller-to-runtime observation traffic. It is
// deliberately process-local: a restart simply repopulates it from sidecars.
type runtimeSnapshotCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[types.NamespacedName]cachedRuntimeSnapshot
}

func newRuntimeSnapshotCache(ttl time.Duration, now func() time.Time) *runtimeSnapshotCache {
	if ttl <= 0 {
		ttl = defaultRuntimeSnapshotTTL
	}
	if now == nil {
		now = time.Now
	}
	return &runtimeSnapshotCache{
		ttl:     ttl,
		now:     now,
		entries: make(map[types.NamespacedName]cachedRuntimeSnapshot),
	}
}

// Get returns a fresh snapshot for a pod. An expired snapshot is never used if
// refresh fails, preventing the scheduler from making a decision on stale HBM.
func (c *runtimeSnapshotCache) Get(
	key types.NamespacedName,
	podUID types.UID,
	refresh func() (*RuntimeSnapshot, error),
) (*RuntimeSnapshot, bool) {
	now := c.now()
	c.mu.Lock()
	entry, found := c.entries[key]
	if found && entry.podUID == podUID && now.Sub(entry.fetchedAt) < c.ttl {
		c.mu.Unlock()
		return entry.snapshot, true
	}
	c.mu.Unlock()

	snapshot, err := refresh()
	if err != nil || snapshot == nil {
		c.mu.Lock()
		entry, found = c.entries[key]
		if found && entry.podUID == podUID && now.Sub(entry.fetchedAt) >= c.ttl {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return nil, false
	}

	c.mu.Lock()
	c.entries[key] = cachedRuntimeSnapshot{
		podUID:    podUID,
		fetchedAt: now,
		snapshot:  snapshot,
	}
	c.mu.Unlock()
	return snapshot, true
}

// PodPlacementState is the scheduling-relevant summary of one warm pod. It is
// not persisted in a CRD because runtime sidecars are the authoritative source.
type PodPlacementState struct {
	SnapshotKnown  bool
	ArtifactCached bool
	MemoryKnown    bool
	HBMFreeBytes   int64
	KVUsedBytes    int64
	ModelCount     int
}

func placementStateFromSnapshot(snapshot *RuntimeSnapshot, artifactURL string, parallelism int64) PodPlacementState {
	state := PodPlacementState{SnapshotKnown: true, ModelCount: len(snapshot.Models)}
	for _, cached := range snapshot.CachedArtifacts {
		if cached == artifactURL {
			state.ArtifactCached = true
			break
		}
	}
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > 1 && int64(len(snapshot.Accelerators)) != parallelism {
		return state
	}
	for _, accelerator := range snapshot.Accelerators {
		// A single-GPU engine needs the largest available single-device slot. A
		// fixed TP/PP group uses every visible GPU, so its safe headroom is the
		// least-free rank rather than a misleading aggregate or maximum.
		if !state.MemoryKnown || (parallelism == 1 && accelerator.HBMFreeBytes > state.HBMFreeBytes) ||
			(parallelism > 1 && accelerator.HBMFreeBytes < state.HBMFreeBytes) {
			state.HBMFreeBytes = accelerator.HBMFreeBytes
			state.MemoryKnown = true
		}
	}
	for _, model := range snapshot.Models {
		state.KVUsedBytes += model.KVUsedBytes
	}
	return state
}
