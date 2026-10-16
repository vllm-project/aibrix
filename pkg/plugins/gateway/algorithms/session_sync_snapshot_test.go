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

package routingalgorithms

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	corev1 "k8s.io/api/core/v1"
)

// Apply a real route update after Redis has produced an MGET reply but before
// the sync loop can use it. No sleeps are needed to force the ordering.
type sessionSyncReadHook struct {
	once  sync.Once
	after func()
}

func (h *sessionSyncReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *sessionSyncReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *sessionSyncReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && cmd.Name() == "mget" {
			h.once.Do(h.after)
		}
		return err
	}
}

func TestSessionSyncPreservesConcurrentFinalTarget(t *testing.T) {
	for _, redisMiss := range []bool{false, true} {
		name := "stale-hit"
		if redisMiss {
			name = "stale-miss"
		}
		t.Run(name, func(t *testing.T) {
			router, mr := newTestSessionAffinityRedis(t)
			const key = "long-agent-session"
			const addrA, addrB = "10.0.0.1:8000", "10.0.0.2:8000"
			cacheKey := sessionCacheKey("model1", key)
			router.storeSessionKeyLocal(cacheKey, addrA, true)
			if !redisMiss {
				require.NoError(t, mr.Set(sessionAffinityRedisKey(cacheKey), addrA))
			}
			pods := newMockPodList([]*corev1.Pod{
				newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			}, nil)
			ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "sync-repin", "")
			t.Cleanup(ctx.Delete)
			ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: key}
			hook := &sessionSyncReadHook{after: func() {
				// The blended/fast-path router commits B while sync still holds A
				// (or a miss). Wait for its real background Redis write to finish.
				require.NoError(t, router.PostRouteUpdate(ctx, pods, pods.All()[1]))
				waitSessionKeyInRedis(t, mr, cacheKey, addrB)
				require.Eventually(t, func() bool {
					addr, confirmed, found := router.loadCachedAddr(cacheKey)
					return found && confirmed && addr == addrB
				}, time.Second, time.Millisecond)
			}}
			router.redisClient.AddHook(hook)
			router.syncSessionKeyPodsFromRedis()

			addr, confirmed, found := router.loadCachedAddr(cacheKey)
			assert.True(t, found, "an old Redis miss must not evict the newly committed pin")
			assert.True(t, confirmed)
			assert.Equal(t, addrB, addr, "an old Redis hit must not replace the newly committed pin")
			assert.Equal(t, int64(1), atomic.LoadInt64(&router.sessionKeyPodsSize))
			remote, err := mr.Get(sessionAffinityRedisKey(cacheKey))
			require.NoError(t, err)
			assert.Equal(t, addrB, remote, "Redis still has the new target")
			scores, scored, err := router.ScoreAll(ctx, pods)
			require.NoError(t, err)
			assert.Equal(t, []bool{true, true}, scored)
			assert.Equal(t, []float64{0, 1}, scores, "subsequent routing must keep the final target")
		})
	}
}

func TestSessionSyncDoesNotRestoreRemovedEntry(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const key, addr = "removed", "10.0.0.1:8000"
	router.storeSessionKeyLocal(key, addr, true)
	require.NoError(t, mr.Set(sessionAffinityRedisKey(key), addr))
	router.redisClient.AddHook(&sessionSyncReadHook{after: func() { router.forgetSessionKey(key) }})
	router.syncSessionKeyPodsFromRedis()
	_, _, found := router.loadCachedAddr(key)
	assert.False(t, found, "a delayed read must not restore an entry removed after the snapshot")
	assert.Zero(t, atomic.LoadInt64(&router.sessionKeyPodsSize))
}

func TestSessionSyncPreservesSameAddressUpdate(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const key, addr = "refreshed-session", "10.0.0.1:8000"
	cacheKey := sessionCacheKey("model1", key)
	router.storeSessionKeyLocal(cacheKey, addr, false)
	// Another replica had changed Redis. The local request repins to the same
	// local address while sync holds that other replica's older value.
	require.NoError(t, mr.Set(sessionAffinityRedisKey(cacheKey), "10.0.0.2:8000"))
	pod := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	pods := newMockPodList([]*corev1.Pod{pod}, nil)
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "sync-refresh", "")
	t.Cleanup(ctx.Delete)
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: key}
	var refreshed any
	router.redisClient.AddHook(&sessionSyncReadHook{after: func() {
		require.NoError(t, router.PostRouteUpdate(ctx, pods, pod))
		waitSessionKeyInRedis(t, mr, cacheKey, addr)
		require.Eventually(t, func() bool {
			current, confirmed, found := router.loadCachedAddr(cacheKey)
			return found && confirmed && current == addr
		}, time.Second, time.Millisecond)
		refreshed, _ = router.sessionKeyPods.Load(cacheKey)
	}})
	router.syncSessionKeyPodsFromRedis()
	current, _ := router.sessionKeyPods.Load(cacheKey)
	assert.Equal(t, refreshed, current, "address equality alone cannot detect a concurrent refresh")
	scores, _, err := router.ScoreAll(ctx, pods)
	require.NoError(t, err)
	assert.Equal(t, []float64{1}, scores)
}

func TestSessionSyncConditionalEvictionPreservesRefresh(t *testing.T) {
	router, _ := newTestSessionAffinityRedis(t)
	const key, addr = "idle-then-active", "10.0.0.1:8000"
	router.storeSessionKeyLocal(key, addr, true)
	stale := sessionKeyCacheItem{addr: addr, confirmed: true, storedAt: time.Now().Add(-2 * sessionAffinityTTL)}
	router.sessionKeyPods.Store(key, stale)
	// This is the entry a Range callback saw before the request refreshed it.
	router.storeSessionKeyLocal(key, addr, true)
	router.forgetSessionKeyIfUnchanged(key, stale)
	current, found := router.sessionKeyPods.Load(key)
	require.True(t, found)
	assert.Equal(t, int64(1), atomic.LoadInt64(&router.sessionKeyPodsSize))
	router.forgetSessionKeyIfUnchanged(key, current.(sessionKeyCacheItem))
	router.forgetSessionKeyIfUnchanged(key, current.(sessionKeyCacheItem))
	assert.Zero(t, atomic.LoadInt64(&router.sessionKeyPodsSize), "only one deletion may decrement the count")
}

func TestSessionSyncRefreshesEveryBatch(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const oldAddr, newAddr = "10.0.0.1:8000", "10.0.0.2:8000"
	for i := 0; i < sessionKeyCacheSyncBatchSize+1; i++ {
		key := fmt.Sprint(i)
		router.storeSessionKeyLocal(key, oldAddr, true)
		require.NoError(t, mr.Set(sessionAffinityRedisKey(key), newAddr))
	}
	router.syncSessionKeyPodsFromRedis()
	for i := 0; i < sessionKeyCacheSyncBatchSize+1; i++ {
		addr, confirmed, found := router.loadCachedAddr(fmt.Sprint(i))
		assert.True(t, found)
		assert.True(t, confirmed)
		assert.Equal(t, newAddr, addr)
	}
	assert.Equal(t, int64(sessionKeyCacheSyncBatchSize+1), atomic.LoadInt64(&router.sessionKeyPodsSize))
}
