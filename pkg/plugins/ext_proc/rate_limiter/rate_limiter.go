package ratelimiter

import (
	"context"
)

type AccountRateLimiter interface {
	// Get the rate limit for the given key and return the current value.
	Get(ctx context.Context, accountId string) (int64, error)

	GetLimit(ctx context.Context, accountId string) (int64, error)

	// Incr increments the rate limit for the given key and return the increased value.
	Incr(ctx context.Context, accountId string, val int64) (int64, error)
}
