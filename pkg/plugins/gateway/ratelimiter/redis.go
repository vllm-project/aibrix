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

type redisRateLimiter struct {
	client     *redis.Client
	name       string
	windowSize time.Duration
}

// NewRedisAccountRateLimiter is a fixed-window limiter whose window starts with
// the first write to each counter key.
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
	// Keep counters separate from the static limit keys read by GetLimit.
	return fmt.Sprintf("%s:%s:%d:counter", rrl.name, key, window.Milliseconds())
}

// incrAndExpireScript applies val to key and sets its TTL only if it doesn't have one yet
// (PTTL == -1), anchoring the window's expiry to the first write that created it. Using a
// plain PEXPIRE here would push the deadline out on every subsequent call -- including
// decrements that refund a rejected increment -- so a client that keeps retrying after a
// 429 would keep resetting the window and could hold it open far longer than intended (see
// enforceModelRPS, whose window can be configured up to an hour for sub-1 rps limits).
//
// A Lua script (rather than IncrBy+ExpireNX pipelined) is used because EXPIRE's NX flag
// requires Redis >= 7.0; EVAL works on any Redis with scripting (2.6+), matching the pattern
// already used for other atomic check-and-set ops (see runningRequestsIncrDecrScript).
//
// KEYS[1] = the counter key
// ARGV[1] = increment value
// ARGV[2] = window TTL in milliseconds
// Returns the counter's new value.
const incrAndExpireScript = `
local newVal = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('PTTL', KEYS[1]) == -1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return newVal
`

func (rrl redisRateLimiter) incrAndExpire(ctx context.Context, key string, val int64, window time.Duration) (int64, error) {
	res, err := rrl.client.Eval(ctx, incrAndExpireScript, []string{key}, val, window.Milliseconds()).Result()
	if err != nil {
		return 0, err
	}
	newVal, ok := res.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected incrAndExpire result type: %T", res)
	}
	return newVal, nil
}
