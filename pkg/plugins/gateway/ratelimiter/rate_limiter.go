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
)

// RateLimiter defines an interface for rate limiting operations.
// It allows querying, retrieving limits, and incrementing usage for a given key.
type RateLimiter interface {
	// Get retrieves the current rate limit usage for the given key.
	// Returns the current value of the rate limit counter and an error if retrieval fails.
	Get(ctx context.Context, key string) (int64, error)

	// GetLimit retrieves the configured maximum rate limit for the given key.
	// Returns the maximum allowed value for the rate limit and an error if retrieval fails.
	GetLimit(ctx context.Context, key string) (int64, error)

	// Incr increments the rate limit counter for the given key by the specified value.
	// Returns the updated rate limit counter after the increment and an error if the operation fails.
	Incr(ctx context.Context, key string, val int64) (int64, error)
}
