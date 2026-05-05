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

import "context"

type noopRateLimiter struct{}

func NewNoopRateLimiter() RateLimiter {
	return &noopRateLimiter{}
}

func (n *noopRateLimiter) Get(ctx context.Context, key string) (int64, error) {
	return 0, nil
}

func (n *noopRateLimiter) GetLimit(ctx context.Context, key string) (int64, error) {
	return 9223372036854775807, nil
}

func (n *noopRateLimiter) Incr(ctx context.Context, key string, val int64) (int64, error) {
	return 0, nil
}
