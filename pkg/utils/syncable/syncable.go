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

// Package syncable defines the interface for state that can be synced across
// replicas (e.g. via Redis). Implement this in the package that owns the state;
// the sync engine (e.g. redissync) depends on this package only.
package syncable

import "context"

// Syncable is the interface that any state struct must implement to participate
// in cross-replica sync. State is typically stored as a key-value map
// (namespace -> entity id -> serialized value). Implementations live in the
// package that owns the state (e.g. prefixcacheindexer), not in the sync engine.
type Syncable interface {
	// Namespace returns a stable name for this state (e.g. "prefixcache").
	Namespace() string

	// GetSnapshot returns the current local state as id -> serialized value.
	// Caller may modify the map; implementation should not retain references.
	GetSnapshot(ctx context.Context) (map[string][]byte, error)

	// ApplyRemote applies one entity from remote into local state.
	// id is the entity ID; data is the serialized value.
	ApplyRemote(ctx context.Context, id string, data []byte) error
}

// OptionalHooks is implemented by Syncables that want before/after full-sync callbacks.
type OptionalHooks interface {
	OnSyncStart(ctx context.Context) error
	OnSyncEnd(ctx context.Context) error
}

// DeltaSyncable is optional. When implemented, Push will send only changed entities
// (GetDelta) instead of the full snapshot (GetSnapshot). Reduces write load to Redis.
// GetDelta returns updated id->bytes and deleted ids; do not clear dirty inside GetDelta.
// ClearDirty is called by the sync layer after a successful push.
type DeltaSyncable interface {
	Syncable
	// GetDelta returns entities that changed since last ClearDirty: updated id->serialized value,
	// and deleted entity ids. Caller must not modify the map. Do not clear dirty in GetDelta.
	GetDelta(ctx context.Context) (updated map[string][]byte, deleted []string, err error)
	// ClearDirty marks all current dirty state as synced; call after successfully pushing delta.
	ClearDirty(ctx context.Context) error
}

// StalePolicy is optional. When implemented, Pull will consult IsStale before applying
// a remote entity; if true, the sync layer will delete the remote key and skip applying.
// Use this to enforce application-level staleness beyond Redis TTL based on lastUpdated metadata.
type StalePolicy interface {
	Syncable
	IsStale(ctx context.Context, id string, data []byte) bool
}

// TombstoneSupport is optional. When implemented, Push writes tombstone payloads for deletions,
// and Pull detects tombstones to delete local state.
type TombstoneSupport interface {
	Syncable
	MakeTombstone(ctx context.Context, id string) []byte
	IsTombstone(ctx context.Context, id string, data []byte) bool
	DeleteLocal(ctx context.Context, id string) error
}
