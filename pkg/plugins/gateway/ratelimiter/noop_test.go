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
	"testing"
)

func TestNoOpRateLimiter_Get(t *testing.T) {
	rl := NewNoOpRateLimiter()
	val, err := rl.Get(context.Background(), "test_key")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != 0 {
		t.Fatalf("expected 0, got %d", val)
	}
}

func TestNoOpRateLimiter_GetLimit(t *testing.T) {
	rl := NewNoOpRateLimiter()
	val, err := rl.GetLimit(context.Background(), "test_key")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != 0 {
		t.Fatalf("expected 0, got %d", val)
	}
}

func TestNoOpRateLimiter_Incr(t *testing.T) {
	rl := NewNoOpRateLimiter()
	val, err := rl.Incr(context.Background(), "test_key", 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != 0 {
		t.Fatalf("expected 0, got %d", val)
	}
}

func TestNoOpRateLimiter_ImplementsInterface(t *testing.T) {
	var _ RateLimiter = &noOpRateLimiter{}
	var _ RateLimiter = NewNoOpRateLimiter()
}
