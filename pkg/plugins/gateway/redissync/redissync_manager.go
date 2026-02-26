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

package redissync

import (
	"context"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/utils/syncable"
	"k8s.io/klog/v2"
)

// Manager coordinates Redis-backed sync for multiple Syncables: registration,
// periodic pull, and write-through (Put/Delete). Use this when running
// multiple gateway replicas that share state via Redis (ByteCloud-compatible).
type Manager struct {
	sync *RedisSync
}

// NewManager creates a Manager that uses per-entity keys with SETEX/MGET and SCAN/ISCAN
// (ByteCloud-safe single-key operations; namespace tag ensures same-shard routing).
func NewManager(client *redis.Client, opts ...Option) *Manager {
	return &Manager{sync: New(client, opts...)}
}

// Register adds a Syncable for periodic pull from Redis. Call before Start.
func (m *Manager) Register(s syncable.Syncable) {
	m.sync.Register(s)
}

// Sync returns the underlying RedisSync for write-through: call Put/Delete
// after local state changes so other replicas see updates on next pull.
func (m *Manager) Sync() *RedisSync {
	return m.sync
}

// Start begins the background periodic pull loop. Safe to call once.
func (m *Manager) Start() {
	m.sync.Start()
	klog.InfoS("redissync manager started", "syncPeriod", m.sync.syncPeriod, "syncables", len(m.sync.syncables))
}

// Stop stops the background sync loop. Idempotent after first call.
func (m *Manager) Stop() {
	m.sync.Stop()
}

// PullFromRedis pulls the latest state from Redis into the given Syncable.
// Useful for one-off sync (e.g. on startup) or from tests.
func (m *Manager) PullFromRedis(ctx context.Context, s syncable.Syncable) error {
	return m.sync.Pull(ctx, s)
}

// PushToRedis writes the Syncable state to Redis (full or delta) using pipelined SETEX per-entity.
func (m *Manager) PushToRedis(ctx context.Context, s syncable.Syncable) error {
	return m.sync.Push(ctx, s)
}
