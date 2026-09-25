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

	"github.com/alicebob/miniredis/v2"
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

func TestRedisRateLimiter_WindowStartsWithFirstWrite(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		key        string
		counterKey string
		windowSize time.Duration
		override   []time.Duration
		window     time.Duration
	}{
		{
			name:       "model RPS override",
			prefix:     "aibrix_model_test",
			key:        "model_MODEL_RPS_CURRENT",
			counterKey: "aibrix_model_test:model_MODEL_RPS_CURRENT:2000:counter",
			windowSize: time.Second,
			override:   []time.Duration{2 * time.Second},
			window:     2 * time.Second,
		},
		{
			name:       "user RPM default",
			prefix:     "aibrix_test",
			key:        "user_RPM_CURRENT",
			counterKey: "aibrix_test:user_RPM_CURRENT:60000:counter",
			windowSize: time.Minute,
			window:     time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			ctx := context.Background()
			rl := NewRedisAccountRateLimiter(tt.prefix, client, tt.windowSize)
			limitKey := tt.prefix + ":" + tt.key
			counterKey := tt.counterKey

			require.NoError(t, client.Set(ctx, limitKey, 7, 0).Err())
			first, err := rl.Incr(ctx, tt.key, 1, tt.override...)
			require.NoError(t, err)
			require.Equal(t, int64(1), first)
			stored, err := client.Get(ctx, counterKey).Int64()
			require.NoError(t, err)
			assert.Equal(t, int64(1), stored)
			limit, err := rl.GetLimit(ctx, tt.key)
			require.NoError(t, err)
			assert.Equal(t, int64(7), limit)

			mr.FastForward(tt.window / 2)
			second, err := rl.Incr(ctx, tt.key, 1, tt.override...)
			require.NoError(t, err)
			assert.Equal(t, int64(2), second, "requests within the first-write window share a counter")
			assert.Equal(t, tt.window/2, mr.TTL(counterKey), "the second write must not extend the window")

			mr.FastForward(tt.window/2 + time.Millisecond)
			exists, err := client.Exists(ctx, counterKey).Result()
			require.NoError(t, err)
			assert.Zero(t, exists, "the counter expires after the first write's window")
			if len(tt.override) == 0 {
				current, err := rl.Get(ctx, tt.key)
				require.NoError(t, err)
				assert.Equal(t, int64(0), current)
			}
			third, err := rl.Incr(ctx, tt.key, 1, tt.override...)
			require.NoError(t, err)
			assert.Equal(t, int64(1), third, "the next request starts a new window")
			limit, err = rl.GetLimit(ctx, tt.key)
			require.NoError(t, err)
			assert.Equal(t, int64(7), limit)
		})
	}
}

func TestRedisRateLimiter_DifferentWindowSizesUseSeparateCounters(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	rl := NewRedisAccountRateLimiter("aibrix_model_test", client, time.Second)
	ctx := context.Background()
	key := "model_MODEL_RPS_CURRENT"

	first, err := rl.Incr(ctx, key, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), first)
	second, err := rl.Incr(ctx, key, 1, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), second, "a new configured duration starts a separate counter")

	current, err := rl.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), current, "the default-window counter remains available")
}

func TestRedisRateLimiter_IncrAndGetCurrentWindow(t *testing.T) {
	client := setupRedisForTest(t)
	rl := NewRedisAccountRateLimiter("ratelimiter_test", client, time.Second)
	typed := rl.(*redisRateLimiter)

	ctx := context.Background()
	key := "userA_RPM_CURRENT"
	redisKey := typed.genKey(key, typed.windowSize)
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
	redisKey := typed.genKey(key, typed.windowSize)
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

// TestRedisRateLimiter_RejectedIncrementDoesNotExtendTTL guards against a regression where
// a rejected request (which enforceModelRPS refunds with a matching -1 Incr) or a client
// retrying after a 429 could keep resetting the window's expiry. incrAndExpire now uses
// ExpireNX, so only the increment that first creates the key in a window may set its TTL --
// every later call in the same window, whether it admits, rejects, or refunds, must leave
// the original deadline untouched. Without that, a long window (enforceModelRPS supports up
// to an hour for sub-1 rps limits) could be held open indefinitely by retries.
func TestRedisRateLimiter_RejectedIncrementDoesNotExtendTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	window := 2 * time.Second
	rl := NewRedisAccountRateLimiter("aibrix_model_test", client, window)
	typed := rl.(*redisRateLimiter)
	ctx := context.Background()
	key := "model_x_MODEL_RPS_CURRENT"
	redisKey := typed.genKey(key, typed.windowSize)

	// First increment (the admitted request) creates the key and sets its TTL.
	val, err := rl.Incr(ctx, key, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), val)
	initialTTL := mr.TTL(redisKey)
	require.Positive(t, initialTTL)

	mr.FastForward(500 * time.Millisecond)

	// A second increment (a rejected request) followed by its refund (-1, as
	// enforceModelRPS does on overflow) must not push the expiry back out.
	val, err = rl.Incr(ctx, key, 1)
	require.NoError(t, err)
	require.Equal(t, int64(2), val)
	val, err = rl.Incr(ctx, key, -1)
	require.NoError(t, err)
	require.Equal(t, int64(1), val, "the rejected increment must be fully refunded")

	ttlAfter := mr.TTL(redisKey)
	assert.Positive(t, ttlAfter, "key must still carry its original TTL, not have lost it")
	assert.LessOrEqual(t, ttlAfter, initialTTL, "TTL must not be extended by a rejected/refunded increment")

	// The window still expires on its original schedule: once it elapses, a fresh request
	// starts a brand new window rather than the retries having pinned the old one open.
	mr.FastForward(window)
	current, err := rl.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(0), current, "window must expire on schedule despite the earlier reject/refund")
}
