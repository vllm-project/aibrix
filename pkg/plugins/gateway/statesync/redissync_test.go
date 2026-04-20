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

package statesync

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helpers

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// fakeSyncable is a simple in-memory Syncable for testing.
type fakeSyncable struct {
	mu        sync.Mutex
	namespace string
	state     map[string][]byte // id -> data
}

func newFakeSyncable(ns string) *fakeSyncable {
	return &fakeSyncable{namespace: ns, state: make(map[string][]byte)}
}

func (f *fakeSyncable) Namespace() string { return f.namespace }

func (f *fakeSyncable) GetSnapshot(_ context.Context) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.state))
	for k, v := range f.state {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSyncable) ApplyRemote(_ context.Context, id string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[id] = data
	return nil
}

func (f *fakeSyncable) get(id string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.state[id]
	return v, ok
}

func (f *fakeSyncable) set(id string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[id] = data
}

// fakeDeltaSyncable adds DeltaSyncable on top of fakeSyncable.
type fakeDeltaSyncable struct {
	*fakeSyncable
	mu      sync.Mutex
	updated map[string][]byte
	deleted []string
}

func newFakeDeltaSyncable(ns string) *fakeDeltaSyncable {
	return &fakeDeltaSyncable{
		fakeSyncable: newFakeSyncable(ns),
		updated:      make(map[string][]byte),
	}
}

func (d *fakeDeltaSyncable) GetDelta(_ context.Context) (map[string][]byte, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u := make(map[string][]byte, len(d.updated))
	for k, v := range d.updated {
		u[k] = v
	}
	del := make([]string, len(d.deleted))
	copy(del, d.deleted)
	return u, del, nil
}

func (d *fakeDeltaSyncable) ClearDirty(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updated = make(map[string][]byte)
	d.deleted = nil
	return nil
}

func (d *fakeDeltaSyncable) markUpdated(id string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updated[id] = data
	d.fakeSyncable.state[id] = data
}

func (d *fakeDeltaSyncable) markDeleted(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, id)
	delete(d.fakeSyncable.state, id)
}

// fakeTombstoneSyncable adds TombstoneSupport on top of fakeDeltaSyncable.
type fakeTombstoneSyncable struct {
	*fakeDeltaSyncable
	deletedLocal []string
}

func newFakeTombstoneSyncable(ns string) *fakeTombstoneSyncable {
	return &fakeTombstoneSyncable{fakeDeltaSyncable: newFakeDeltaSyncable(ns)}
}

const tombstoneMarker = "TOMBSTONE:"

func (ts *fakeTombstoneSyncable) MakeTombstone(_ context.Context, id string) []byte {
	return []byte(tombstoneMarker + id)
}

func (ts *fakeTombstoneSyncable) IsTombstone(_ context.Context, _ string, data []byte) bool {
	return len(data) > len(tombstoneMarker) && string(data[:len(tombstoneMarker)]) == tombstoneMarker
}

func (ts *fakeTombstoneSyncable) DeleteLocal(_ context.Context, id string) error {
	ts.deletedLocal = append(ts.deletedLocal, id)
	delete(ts.fakeSyncable.state, id)
	return nil
}

// fakeStaleSyncable adds StalePolicy.
type fakeStaleSyncable struct {
	*fakeSyncable
	staleIDs map[string]bool
}

func newFakeStaleSyncable(ns string) *fakeStaleSyncable {
	return &fakeStaleSyncable{fakeSyncable: newFakeSyncable(ns), staleIDs: make(map[string]bool)}
}

func (s *fakeStaleSyncable) IsStale(_ context.Context, id string, _ []byte) bool {
	return s.staleIDs[id]
}

// fakeHooksSyncable adds OptionalHooks.
type fakeHooksSyncable struct {
	*fakeSyncable
	startCalled int
	endCalled   int
}

func newFakeHooksSyncable(ns string) *fakeHooksSyncable {
	return &fakeHooksSyncable{fakeSyncable: newFakeSyncable(ns)}
}

func (h *fakeHooksSyncable) OnSyncStart(_ context.Context) error {
	h.startCalled++
	return nil
}

func (h *fakeHooksSyncable) OnSyncEnd(_ context.Context) error {
	h.endCalled++
	return nil
}

// --- Tests ---

func TestPut(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	err := r.Put(ctx, "ns1", "entity1", []byte("value1"))
	require.NoError(t, err)

	key := "aibrix:{ns1}:e:entity1"
	got, err := mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "value1", got)

	// TTL should be set
	ttl := mr.TTL(key)
	assert.Greater(t, ttl, time.Duration(0))
}

func TestPut_EmptyNamespace(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)
	err := r.Put(context.Background(), "", "id", []byte("v"))
	assert.Error(t, err)
}

func TestDelete(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	key := "aibrix:{ns1}:e:entity1"
	require.NoError(t, mr.Set(key, "value1"))

	err := r.Delete(ctx, "ns1", "entity1")
	require.NoError(t, err)
	assert.False(t, mr.Exists(key))
}

func TestDelete_EmptyNamespace(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)
	err := r.Delete(context.Background(), "", "id")
	assert.Error(t, err)
}

func TestPull_Basic(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	require.NoError(t, mr.Set("aibrix:{ns}:e:a", "val-a"))
	require.NoError(t, mr.Set("aibrix:{ns}:e:b", "val-b"))

	s := newFakeSyncable("ns")
	err := r.Pull(ctx, s)
	require.NoError(t, err)

	v, ok := s.get("a")
	require.True(t, ok)
	assert.Equal(t, "val-a", string(v))

	v, ok = s.get("b")
	require.True(t, ok)
	assert.Equal(t, "val-b", string(v))
}

func TestPull_Empty(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	s := newFakeSyncable("ns")
	err := r.Pull(context.Background(), s)
	require.NoError(t, err)
	assert.Empty(t, s.state)
}

func TestPull_BatchingAcrossMGetBatchSize(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client, WithMGetBatchSize(10))
	ctx := context.Background()

	total := 35 // more than one batch
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("aibrix:{ns}:e:entity%d", i)
		require.NoError(t, mr.Set(key, fmt.Sprintf("val%d", i)))
	}

	s := newFakeSyncable("ns")
	err := r.Pull(ctx, s)
	require.NoError(t, err)
	assert.Len(t, s.state, total)
}

func TestPull_IgnoresOtherNamespaceKeys(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	require.NoError(t, mr.Set("aibrix:{ns1}:e:a", "mine"))
	require.NoError(t, mr.Set("aibrix:{ns2}:e:b", "other"))

	s := newFakeSyncable("ns1")
	err := r.Pull(ctx, s)
	require.NoError(t, err)

	assert.Len(t, s.state, 1)
	v, ok := s.get("a")
	require.True(t, ok)
	assert.Equal(t, "mine", string(v))
}

func TestPull_TombstoneDeletesLocal(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	s := newFakeTombstoneSyncable("ns")
	tombstoneData := s.MakeTombstone(ctx, "entity1")
	require.NoError(t, mr.Set("aibrix:{ns}:e:entity1", string(tombstoneData)))
	require.NoError(t, mr.Set("aibrix:{ns}:e:entity2", "alive"))

	err := r.Pull(ctx, s)
	require.NoError(t, err)

	assert.Contains(t, s.deletedLocal, "entity1")
	_, ok := s.get("entity1")
	assert.False(t, ok)

	v, ok := s.get("entity2")
	require.True(t, ok)
	assert.Equal(t, "alive", string(v))
}

func TestPull_StalePolicyDeletesFromRedis(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	require.NoError(t, mr.Set("aibrix:{ns}:e:stale", "old"))
	require.NoError(t, mr.Set("aibrix:{ns}:e:fresh", "new"))

	s := newFakeStaleSyncable("ns")
	s.staleIDs["stale"] = true

	err := r.Pull(ctx, s)
	require.NoError(t, err)

	// stale key must be deleted from Redis and not applied locally
	assert.False(t, mr.Exists("aibrix:{ns}:e:stale"))
	_, ok := s.get("stale")
	assert.False(t, ok)

	v, ok := s.get("fresh")
	require.True(t, ok)
	assert.Equal(t, "new", string(v))
}

func TestPull_OptionalHooksCalled(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	s := newFakeHooksSyncable("ns")
	err := r.Pull(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, 1, s.startCalled)
	assert.Equal(t, 1, s.endCalled)
}

func TestPush_FullSnapshot(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	s := newFakeSyncable("ns")
	s.set("id1", []byte("v1"))
	s.set("id2", []byte("v2"))

	err := r.Push(ctx, s)
	require.NoError(t, err)

	got, err := mr.Get("aibrix:{ns}:e:id1")
	require.NoError(t, err)
	assert.Equal(t, "v1", got)

	got, err = mr.Get("aibrix:{ns}:e:id2")
	require.NoError(t, err)
	assert.Equal(t, "v2", got)
}

func TestPush_FullSnapshot_Empty(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	s := newFakeSyncable("ns")
	err := r.Push(context.Background(), s)
	require.NoError(t, err) // no-op, no error
}

func TestPush_DeltaUpdatedAndDeleted(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	// Pre-seed a key that will be "deleted"
	require.NoError(t, mr.Set("aibrix:{ns}:e:old", "stale"))

	s := newFakeDeltaSyncable("ns")
	s.markUpdated("new", []byte("fresh"))
	s.markDeleted("old")

	err := r.Push(ctx, s)
	require.NoError(t, err)

	got, err := mr.Get("aibrix:{ns}:e:new")
	require.NoError(t, err)
	assert.Equal(t, "fresh", got)

	assert.False(t, mr.Exists("aibrix:{ns}:e:old"))

	// dirty should be cleared after successful push
	updated, deleted, err := s.GetDelta(ctx)
	require.NoError(t, err)
	assert.Empty(t, updated)
	assert.Empty(t, deleted)
}

func TestPush_DeltaWithTombstone(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	s := newFakeTombstoneSyncable("ns")
	s.markUpdated("alive", []byte("data"))
	s.markDeleted("gone")

	err := r.Push(ctx, s)
	require.NoError(t, err)

	// "gone" should exist as a tombstone, not be deleted
	val, err := mr.Get("aibrix:{ns}:e:gone")
	require.NoError(t, err)
	assert.True(t, s.IsTombstone(ctx, "gone", []byte(val)))
}

func TestPush_DeltaEmpty_NoClearDirty(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	s := newFakeDeltaSyncable("ns")
	// no delta; ClearDirty should not be called (nothing to sync)
	err := r.Push(context.Background(), s)
	require.NoError(t, err)
}

func TestKeyFormat(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	assert.Equal(t, "aibrix:{myns}:e:myid", r.redisKeyForEntity("myns", "myid"))
	assert.Equal(t, "aibrix:{myns}:e:*", r.redisScanPattern("myns"))
}

func TestEntityIDFromKey(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	key := "aibrix:{ns}:e:entity123"
	id := r.entityIDFromKey("ns", key)
	assert.Equal(t, "entity123", id)

	// exact prefix with no entity ID returns empty (caller skips it)
	assert.Equal(t, "", r.entityIDFromKey("ns", "aibrix:{ns}:e:"))
}

func TestWithOptions(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client,
		WithKeyPrefix("custom"),
		WithSyncPeriod(5*time.Second),
		WithOpTimeout(15*time.Second),
		WithSetexChunkSize(50),
		WithMGetBatchSize(100),
		WithRecordTTL(7*time.Minute),
		WithStopWaitTimeout(45*time.Second),
	)

	assert.Equal(t, "custom", r.keyPrefix)
	assert.Equal(t, 5*time.Second, r.syncPeriod)
	assert.Equal(t, 15*time.Second, r.opTimeout)
	assert.Equal(t, 50, r.setexChunkSize)
	assert.Equal(t, 100, r.mgetBatchSize)
	assert.Equal(t, 7*time.Minute, r.recordTTL)
	assert.Equal(t, 45*time.Second, r.stopWaitTimeout)
}

func TestStop_Idempotent(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	r.Start()
	assert.NotPanics(t, func() {
		r.Stop()
		r.Stop() // second call must not panic
	})
}

func TestRegister_AfterStart_IsNoOp(t *testing.T) {
	_, client := newTestRedis(t)
	r := New(client)

	r.Start()
	defer r.Stop()

	s := newFakeSyncable("ns")
	r.Register(s) // should warn and not add
	assert.Empty(t, r.syncables)
}

func TestPull_TTLSetOnPush(t *testing.T) {
	mr, client := newTestRedis(t)
	r := New(client)
	ctx := context.Background()

	s := newFakeSyncable("ns")
	s.set("id1", []byte("v1"))

	err := r.Push(ctx, s)
	require.NoError(t, err)

	ttl := mr.TTL("aibrix:{ns}:e:id1")
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, defaultRecordTTL)
}
