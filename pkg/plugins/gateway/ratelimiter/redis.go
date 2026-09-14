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
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const binSize = 64

type redisRateLimiter struct {
	client     *redis.Client
	name       string
	windowSize time.Duration
}

// NewRedisAccountRateLimiter is a simple fixed window rate limiter
func NewRedisAccountRateLimiter(name string, client *redis.Client, windowSize time.Duration) RateLimiter {
	if windowSize < time.Second {
		windowSize = time.Second
	}

	return &redisRateLimiter{
		name:       name,
		client:     client,
		windowSize: windowSize,
	}
}

func (rrl redisRateLimiter) Get(ctx context.Context, key string) (int64, error) {
	return rrl.get(ctx, rrl.genKey(key, rrl.windowSize))
}

func (rrl redisRateLimiter) GetLimit(ctx context.Context, key string) (int64, error) {
	return rrl.get(ctx, fmt.Sprintf("%s:%s", rrl.name, key))
}

func (rrl redisRateLimiter) get(ctx context.Context, key string) (int64, error) {
	val, err := rrl.client.Get(ctx, key).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	return val, err
}

func (rrl redisRateLimiter) Incr(ctx context.Context, key string, val int64, window ...time.Duration) (int64, error) {
	w := rrl.windowSize
	if len(window) > 0 && window[0] > 0 {
		w = window[0]
	}
	return rrl.incrAndExpire(ctx, rrl.genKey(key, w), val, w)
}

func (rrl redisRateLimiter) genKey(key string, window time.Duration) string {
	return fmt.Sprintf("%s:%s:%d", rrl.name, key, time.Now().Unix()/int64(window.Seconds())%binSize)
}

func (rrl redisRateLimiter) incrAndExpire(ctx context.Context, key string, val int64, window time.Duration) (int64, error) {
	pipe := rrl.client.Pipeline()

	incr := pipe.IncrBy(ctx, key, val)
	// ExpireNX (only set if the key has no TTL yet) anchors the window's expiry to the first
	// write that created it. Using a plain Expire here would push the deadline out on every
	// subsequent call -- including decrements that refund a rejected increment -- so a client
	// that keeps retrying after a 429 would keep resetting the window and could hold it open
	// far longer than intended (see enforceModelRPS, whose window can be configured up to an
	// hour for sub-1 rps limits).
	pipe.ExpireNX(ctx, key, window)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}

	return incr.Val(), nil
}
