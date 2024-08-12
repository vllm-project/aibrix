package ratelimiter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 用来限制 rate limiter 使用的 key 的数量，需要大于 2
const binSize = 64

type redisAccountRateLimiter struct {
	client     *redis.Client
	name       string
	windowSize time.Duration
}

// simple fixed window rate limiter
func NewRedisAccountRateLimiter(name string, client *redis.Client, windowSize time.Duration) AccountRateLimiter {
	if windowSize < time.Second {
		windowSize = time.Second
	}

	return &redisAccountRateLimiter{
		name:       name,
		client:     client,
		windowSize: windowSize,
	}
}

func (rrl redisAccountRateLimiter) Get(ctx context.Context, accountId string) (int64, error) {
	return rrl.get(ctx, rrl.genKey(accountId))
}

func (rrl redisAccountRateLimiter) GetLimit(ctx context.Context, accountId string) (int64, error) {
	return rrl.get(ctx, fmt.Sprintf("%s:%s", rrl.name, accountId))
}

func (rrl redisAccountRateLimiter) get(ctx context.Context, key string) (int64, error) {
	val, err := rrl.client.Get(ctx, key).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	return val, err
}

func (rrl redisAccountRateLimiter) Incr(ctx context.Context, accountId string, val int64) (int64, error) {
	return rrl.incrAndExpire(ctx, rrl.genKey(accountId), val)
}

func (rrl redisAccountRateLimiter) genKey(accountId string) string {
	return fmt.Sprintf("%s:%s:%d", rrl.name, accountId, time.Now().Unix()/int64(rrl.windowSize.Seconds())%binSize)
}

func (rrl redisAccountRateLimiter) incrAndExpire(ctx context.Context, key string, val int64) (int64, error) {
	pipe := rrl.client.Pipeline()

	incr := pipe.IncrBy(ctx, key, val)
	pipe.Expire(ctx, key, rrl.windowSize)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}

	return incr.Val(), nil
}
