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

// Package redissync provides a generic Redis-backed state sync layer for use
// across multiple gateway (or other) replicas. State is stored as one Redis
// key per entity (aibrix:{namespace}:e:{entityId}) with per-record TTL so each
// record expires independently. Uses SET/SETEX, GET, DEL, and SCAN (or ISCAN
// on ByteCloud). No PUB/SUB.
//
// Usage:
//
//  1. Create a Manager and register Syncables (e.g. prefixcacheindexer.NewPrefixHashTableSyncable(table)).
//  2. Call Start() to run periodic pull from Redis.
//  3. For write-through: after local state changes, call Sync().Put(ctx, namespace, id, data)
//     (e.g. after AddPrefix, encode each updated block and Put to "prefixcache").
//  4. On shutdown, call Stop().
//
// Example: prefixcacheindexer.NewPrefixHashTableSyncable(table) implements
// syncable.Syncable; use EncodeBlockForSync on the table for write-through.
package redissync

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/utils/syncable"
	"k8s.io/klog/v2"
)

const (
	defaultKeyPrefix         = "aibrix"
	defaultSyncPeriod        = 10 * time.Second
	recordTTL                = 5 * time.Minute // TTL per record (each entity key); record expires if not written within this window
	setexChunkSize           = 25             // batch size for pipeline SETEX (ByteCloud suggests ~20-30)
	mgetBatchSize            = 25             // batch size for MGET when pulling
	scanCount                = 100            // SCAN COUNT hint per iteration
	opTimeout                = 30 * time.Second
	maxBackoff               = 2 * time.Minute
	snapshotWarnFields       = 10000          // warn when full snapshot exceeds this many fields
	snapshotWarnBytesPerField = 512 * 1024    // warn when total snapshot size/fields exceeds this (512KB per field avg)
)

// RedisSync stores one Redis key per entity with per-record TTL. Key format
// aibrix:{namespace}:e:{entityId} so {namespace} is the hash tag for ByteCloud.
type RedisSync struct {
	client        *redis.Client
	keyPrefix     string
	syncPeriod    time.Duration
	opTimeout     time.Duration
	syncables     []syncable.Syncable
	started       bool
	startedMu     sync.Mutex
	setexChunkSize int
	mgetBatchSize  int
	stopCh        chan struct{}
	stopOnce      sync.Once
}

// Option configures RedisSync.
type Option func(*RedisSync)

// WithKeyPrefix sets the Redis key prefix (default "aibrix").
func WithKeyPrefix(prefix string) Option {
	return func(r *RedisSync) {
		r.keyPrefix = prefix
	}
}

// WithSyncPeriod sets the interval for periodic sync (default 10s).
func WithSyncPeriod(d time.Duration) Option {
	return func(r *RedisSync) {
		r.syncPeriod = d
	}
}

// WithOpTimeout sets the per-operation context timeout for Pull/Push (default 30s).
func WithOpTimeout(d time.Duration) Option {
	return func(r *RedisSync) {
		r.opTimeout = d
	}
}

// WithSetexChunkSize sets the pipeline chunk size for SETEX writes; if <=0, default is used.
func WithSetexChunkSize(n int) Option {
	return func(r *RedisSync) { r.setexChunkSize = n }
}

// WithMGetBatchSize sets the batch size for MGET during Pull; if <=0, default is used.
func WithMGetBatchSize(n int) Option {
	return func(r *RedisSync) { r.mgetBatchSize = n }
}

// New creates a RedisSync that uses only Hash commands on one key per namespace.
func New(client *redis.Client, opts ...Option) *RedisSync {
	r := &RedisSync{
		client:     client,
		keyPrefix:  defaultKeyPrefix,
		syncPeriod:  defaultSyncPeriod,
		opTimeout:  opTimeout,
		syncables:  nil,
		stopCh:     make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	// Seed RNG for jitter/backoff randomness
	rand.Seed(time.Now().UnixNano())
	return r
}

// Register adds a Syncable for periodic sync. Must be called before Start;
// Register after Start is a no-op and is not applied.
func (r *RedisSync) Register(s syncable.Syncable) {
	r.startedMu.Lock()
	defer r.startedMu.Unlock()
	if r.started {
		klog.Warning("redissync: Register called after Start; ignoring")
		return
	}
	if s.Namespace() == "" {
		klog.Warning("redissync: Syncable with empty namespace ignored")
		return
	}
	r.syncables = append(r.syncables, s)
}

// redisKeyForEntity returns the Redis key for one entity (hash tag = namespace for ByteCloud).
func (r *RedisSync) redisKeyForEntity(namespace, entityID string) string {
	if namespace == "" {
		klog.Warning("redissync: empty namespace when building entity key")
	}
	return fmt.Sprintf("%s:{%s}:e:%s", r.keyPrefix, namespace, entityID)
}

// redisScanPattern returns the SCAN match pattern for all entity keys in a namespace.
func (r *RedisSync) redisScanPattern(namespace string) string {
	return fmt.Sprintf("%s:{%s}:e:*", r.keyPrefix, namespace)
}

// entityIDFromKey extracts entity ID from key "aibrix:{ns}:e:{id}" -> id.
func (r *RedisSync) entityIDFromKey(namespace, key string) string {
	prefix := fmt.Sprintf("%s:{%s}:e:", r.keyPrefix, namespace)
	if len(key) > len(prefix) {
		return key[len(prefix):]
	}
	return ""
}

// Put writes one entity to Redis (write-through). Uses SETEX so the record has per-entity TTL (recordTTL).
func (r *RedisSync) Put(ctx context.Context, namespace, id string, data []byte) error {
	if namespace == "" {
		return fmt.Errorf("redissync put: namespace must be non-empty")
	}
	key := r.redisKeyForEntity(namespace, id)
	if err := r.client.SetEx(ctx, key, data, recordTTL).Err(); err != nil {
		return fmt.Errorf("redissync put %s/%s: %w", namespace, id, err)
	}
	return nil
}

// Delete removes one entity from Redis. Uses DEL.
func (r *RedisSync) Delete(ctx context.Context, namespace, id string) error {
	if namespace == "" {
		return fmt.Errorf("redissync delete: namespace must be non-empty")
	}
	key := r.redisKeyForEntity(namespace, id)
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("redissync delete %s/%s: %w", namespace, id, err)
	}
	return nil
}

// Pull loads all entity keys for the namespace via SCAN (ByteCloud requires ISCAN—replace
// client.Scan with client.Do(ctx, "ISCAN", ...) if needed), then MGET in batches and
// ApplyRemote for each. Each record has its own TTL; expired keys are not returned by Redis.
func (r *RedisSync) Pull(ctx context.Context, s syncable.Syncable) error {
	if o, ok := s.(syncable.OptionalHooks); ok {
		if err := o.OnSyncStart(ctx); err != nil {
			klog.V(4).InfoS("redissync OnSyncStart error", "namespace", s.Namespace(), "err", err)
		}
		defer func() {
			if err := o.OnSyncEnd(ctx); err != nil {
				klog.V(4).InfoS("redissync OnSyncEnd error", "namespace", s.Namespace(), "err", err)
			}
		}()
	}

	namespace := s.Namespace()
	if namespace == "" {
		return fmt.Errorf("redissync pull: empty namespace")
	}
	pattern := r.redisScanPattern(namespace)
	var keys []string
	iter := r.client.Scan(ctx, 0, pattern, scanCount).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("redissync pull Scan %s: %w", namespace, err)
	}

	batchSize := r.mgetBatchSize
	if batchSize <= 0 {
		batchSize = mgetBatchSize
	}
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		vals, err := r.client.MGet(ctx, batch...).Result()
		if err != nil {
			return fmt.Errorf("redissync pull MGet %s: %w", namespace, err)
		}
		for j, key := range batch {
			if j >= len(vals) {
				break
			}
			v := vals[j]
			if v == nil {
				continue
			}
			sv, ok := v.(string)
			if !ok {
				continue
			}
			entityID := r.entityIDFromKey(namespace, key)
			if entityID == "" {
				continue
			}
			if ts, ok := s.(syncable.TombstoneSupport); ok {
				if ts.IsTombstone(ctx, entityID, []byte(sv)) {
					_ = ts.DeleteLocal(ctx, entityID)
					continue
				}
			}
			if sp, ok := s.(syncable.StalePolicy); ok {
				if sp.IsStale(ctx, entityID, []byte(sv)) {
					_ = r.client.Del(ctx, key).Err()
					continue
				}
			}
			if err := s.ApplyRemote(ctx, entityID, []byte(sv)); err != nil {
				klog.V(4).InfoS("redissync ApplyRemote error", "namespace", namespace, "id", entityID, "err", err)
			}
		}
	}
	return nil
}

// Push writes Syncable state to Redis. If s implements syncable.DeltaSyncable, only
// changed entities (delta) are written; otherwise the full snapshot. Each record is
// written with SETEX so it has its own TTL (recordTTL).
func (r *RedisSync) Push(ctx context.Context, s syncable.Syncable) error {
	namespace := s.Namespace()
	if delta, ok := s.(syncable.DeltaSyncable); ok {
		return r.pushDelta(ctx, namespace, delta)
	}
	return r.pushFull(ctx, namespace, s)
}

// setexChunked writes each entity as a separate key with SETEX (per-record TTL), in pipelined chunks.
func (r *RedisSync) setexChunked(ctx context.Context, namespace string, m map[string][]byte) error {
	if len(m) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	n := 0
	chunkSize := r.setexChunkSize
	if chunkSize <= 0 {
		chunkSize = setexChunkSize
	}
	for id, data := range m {
		key := r.redisKeyForEntity(namespace, id)
		pipe.SetEx(ctx, key, data, recordTTL)
		n++
		if n >= chunkSize {
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
			pipe = r.client.Pipeline()
			n = 0
		}
	}
	if n > 0 {
		_, err := pipe.Exec(ctx)
		return err
	}
	return nil
}

// pushDelta writes only updated and deleted entities; each record gets its own TTL via SETEX/DEL.
func (r *RedisSync) pushDelta(ctx context.Context, namespace string, s syncable.DeltaSyncable) error {
	updated, deleted, err := s.GetDelta(ctx)
	if err != nil {
		return fmt.Errorf("redissync push GetDelta %s: %w", namespace, err)
	}
	nUpdated, nDeleted := len(updated), len(deleted)
	if nUpdated > 0 {
		if err := r.setexChunked(ctx, namespace, updated); err != nil {
			return fmt.Errorf("redissync push SETEX delta %s: %w", namespace, err)
		}
	}
	if nDeleted > 0 {
		for _, id := range deleted {
			key := r.redisKeyForEntity(namespace, id)
			if ts, ok := s.(syncable.TombstoneSupport); ok {
				payload := ts.MakeTombstone(ctx, id)
				if err := r.client.SetEx(ctx, key, payload, recordTTL).Err(); err != nil {
					klog.V(4).InfoS("redissync SetEx tombstone error", "key", key, "err", err)
				}
				continue
			}
			if err := r.client.Del(ctx, key).Err(); err != nil {
				klog.V(4).InfoS("redissync Del error", "key", key, "err", err)
			}
		}
	}
	if nUpdated > 0 || nDeleted > 0 {
		klog.InfoS("redissync pushed delta only", "namespace", namespace, "updated", nUpdated, "deleted", nDeleted)
	}
	if err := s.ClearDirty(ctx); err != nil {
		klog.V(4).InfoS("redissync ClearDirty error", "namespace", namespace, "err", err)
	}
	return nil
}

// pushFull writes the full snapshot as one key per entity with SETEX (per-record TTL).
func (r *RedisSync) pushFull(ctx context.Context, namespace string, s syncable.Syncable) error {
	snap, err := s.GetSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("redissync push GetSnapshot %s: %w", namespace, err)
	}
	if len(snap) == 0 {
		return nil
	}
	n := len(snap)
	var totalBytes int
	for _, data := range snap {
		totalBytes += len(data)
	}
	if n > snapshotWarnFields {
		klog.Warningf("redissync snapshot field count exceeds soft limit: namespace=%s fields=%d limit=%d", namespace, n, snapshotWarnFields)
	}
	if n > 0 && totalBytes/n > snapshotWarnBytesPerField {
		klog.Warningf("redissync snapshot avg size per field high: namespace=%s avgBytes=%d limit=%d", namespace, totalBytes/n, snapshotWarnBytesPerField)
	}
	if err := r.setexChunked(ctx, namespace, snap); err != nil {
		return fmt.Errorf("redissync push SETEX %s: %w", namespace, err)
	}
	klog.V(4).InfoS("redissync pushed full snapshot", "namespace", namespace, "entries", n)
	return nil
}

// Start starts the background goroutine: optional initial sync (with small random
// delay), then periodic Pull-first then optional Push, with jitter and backoff on errors.
// Register must be called before Start.
func (r *RedisSync) Start() {
	r.startedMu.Lock()
	if r.started {
		r.startedMu.Unlock()
		return
	}
	r.started = true
	r.startedMu.Unlock()
	go r.syncLoop()
}

// StartWithContext starts the sync loop and binds it to the provided context.
// When ctx is canceled, Stop() is invoked.
func (r *RedisSync) StartWithContext(ctx context.Context) {
	go func() {
		<-ctx.Done()
		r.Stop()
	}()
	r.Start()
}

// syncLoop runs: initial random delay, one immediate Pull (and optional Push),
// then periodic Pull-first and optional Push with jitter and backoff on errors.
func (r *RedisSync) syncLoop() {
	// Immediate initial sync: Pull-first then optional Push.
	r.runOneSyncCycle()
	backoff := r.syncPeriod
	for {
		// Next run after period + jitter (±20%), or backoff on error.
		interval := backoff
		jitter := time.Duration(rand.Int63n(int64(r.syncPeriod)/5)) - r.syncPeriod/10
		interval += jitter
		if interval < r.syncPeriod/2 {
			interval = r.syncPeriod / 2
		}
		select {
		case <-r.stopCh:
			return
		case <-time.After(interval):
		}
		if err := r.runOneSyncCycle(); err != nil {
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		} else {
			backoff = r.syncPeriod
		}
	}
}

// runOneSyncCycle runs Pull first for all Syncables, then optional Push. Pull-before-Push
// reduces overwrites. Uses opTimeout for the cycle.
func (r *RedisSync) runOneSyncCycle() error {
	ctx, cancel := context.WithTimeout(context.Background(), r.opTimeout)
	defer cancel()
	var lastErr error
	for _, s := range r.syncables {
		if err := r.Pull(ctx, s); err != nil {
			klog.ErrorS(err, "redissync periodic pull failed", "namespace", s.Namespace())
			lastErr = err
		}
	}
	for _, s := range r.syncables {
		if err := r.Push(ctx, s); err != nil {
			klog.ErrorS(err, "redissync periodic push failed", "namespace", s.Namespace())
			lastErr = err
		}
	}
	return lastErr
}

// Stop stops the background sync loop. Safe to call more than once.
func (r *RedisSync) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}
