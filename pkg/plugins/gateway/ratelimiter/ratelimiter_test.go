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

package ratelimiter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRedisForTest(t *testing.T) *redis.Client {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Skip("Redis is not available at localhost:6379")
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

func TestNoopRateLimiter(t *testing.T) {
	rl := NewNoopRateLimiter()

	got, err := rl.Get(context.Background(), "any")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)

	limit, err := rl.GetLimit(context.Background(), "any")
	require.NoError(t, err)
	assert.Equal(t, int64(9223372036854775807), limit)

	incr, err := rl.Incr(context.Background(), "any", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(0), incr)
}

func TestNewRedisAccountRateLimiter_ClampsWindowToOneSecond(t *testing.T) {
	rl := NewRedisAccountRateLimiter("test", nil, 100*time.Millisecond)
	typed, ok := rl.(*redisRateLimiter)
	require.True(t, ok)
	assert.Equal(t, time.Second, typed.windowSize)
}

func TestRedisRateLimiter_IncrAndGetCurrentWindow(t *testing.T) {
	client := setupRedisForTest(t)
	rl := NewRedisAccountRateLimiter("ratelimiter_test", client, time.Second)
	typed := rl.(*redisRateLimiter)

	ctx := context.Background()
	key := "userA_RPM_CURRENT"
	redisKey := typed.genKey(key)
	t.Cleanup(func() {
		_ = client.Del(ctx, redisKey).Err()
	})

	// Missing key should read as 0.
	initial, err := rl.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(0), initial)

	afterIncr, err := rl.Incr(ctx, key, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), afterIncr)

	afterSecondIncr, err := rl.Incr(ctx, key, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), afterSecondIncr)

	current, err := rl.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(3), current)
}

func TestRedisRateLimiter_GetLimitUsesStaticKey(t *testing.T) {
	client := setupRedisForTest(t)
	rl := NewRedisAccountRateLimiter("ratelimiter_test", client, time.Second)

	ctx := context.Background()
	limitKey := "alice_limit"
	fullKey := "ratelimiter_test:" + limitKey

	require.NoError(t, client.Set(ctx, fullKey, 42, time.Minute).Err())
	t.Cleanup(func() {
		_ = client.Del(ctx, fullKey).Err()
	})

	got, err := rl.GetLimit(ctx, limitKey)
	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

func TestRedisRateLimiter_ConcurrentIncrements(t *testing.T) {
	client := setupRedisForTest(t)
	rl := NewRedisAccountRateLimiter("ratelimiter_test", client, 5*time.Second)
	typed := rl.(*redisRateLimiter)

	ctx := context.Background()
	key := "burst_MODEL_RPS_CURRENT"
	redisKey := typed.genKey(key)
	t.Cleanup(func() {
		_ = client.Del(ctx, redisKey).Err()
	})

	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, err := rl.Incr(ctx, key, 1)
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	got, err := rl.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(workers), got, "all concurrent increments should be reflected")
}

