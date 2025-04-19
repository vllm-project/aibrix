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

package vtc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSlidingWindowTokenTracker_GetTokenCount(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	// Test initial count for a new user
	tokens, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count should be 0")

	// Test count after update
	err = tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(40), tokens, "Token count after first update")

	// Test initial count for another new user
	tokens, err = tracker.GetTokenCount(ctx, "user2")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count for user2 should be 0")

	// Test count for empty user
	tokens, err = tracker.GetTokenCount(ctx, "")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Token count for empty user should be 0")

	// Test non-existent user
	tokens, _ = tracker.GetTokenCount(ctx, "nonexistent")
	assert.Equal(t, float64(0), tokens, "Token count for non-existent user should be 0")
}

func TestSlidingWindowTokenTracker_WindowBehavior(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	implTracker := tracker.(*InMemorySlidingWindowTokenTracker) // Type assertion
	ctx := context.Background()

	// Initial count
	tokens, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count should be 0")

	// Add tokens in current millisecond
	err = tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(40), tokens, "Token count after first update")

	// Add tokens in next bucket (simulate time advance)
	nextBucket := time.Now().Add(10 * time.Millisecond)
	implTracker.mu.Lock()
	windowStart := nextBucket.Truncate(time.Millisecond).UnixNano() / int64(time.Millisecond)
	if _, ok := implTracker.userBuckets["user1"]; !ok {
		implTracker.userBuckets["user1"] = make(map[int64]float64)
	}
	implTracker.userBuckets["user1"][windowStart] += 20
	implTracker.mu.Unlock()

	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(60), tokens, "Sum over two buckets")

	// Simulate all tokens outside window
	implTracker.mu.Lock()
	for ts := range implTracker.userBuckets["user1"] {
		delete(implTracker.userBuckets["user1"], ts)
		// Set tokens 200ms ago (outside 100ms window)
		implTracker.userBuckets["user1"][ts-200] = 100
	}
	implTracker.mu.Unlock()
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Tokens outside window should not be counted")
}

func TestSlidingWindowTokenTracker_UpdateTokenCount_WithWeights(t *testing.T) {
	config := DefaultVTCConfig()
	config.InputTokenWeight = 2.0
	config.OutputTokenWeight = 3.0
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	err := tracker.UpdateTokenCount(ctx, "user2", 2, 4) // 2*2 + 4*3 = 16
	assert.NoError(t, err)
	tokens, err := tracker.GetTokenCount(ctx, "user2")
	assert.NoError(t, err)
	assert.Equal(t, float64(16), tokens, "Weighted token count")
}

func TestSlidingWindowTokenTracker_UpdateTokenCount(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	// Test initial update
	err := tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, _ := tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(40), tokens, "First update")

	// Test second update for the same user
	err = tracker.UpdateTokenCount(ctx, "user1", 5, 10) // 40 + (5*1.0 + 10*2.0) = 40 + 25 = 65
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(65), tokens, "Second update")

	// Test update for a different user
	err = tracker.UpdateTokenCount(ctx, "user2", 100, 50) // 100*1.0 + 50*2.0 = 200
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user2")
	assert.Equal(t, float64(200), tokens, "Update for user2")

	// Test update for empty user (should do nothing)
	err = tracker.UpdateTokenCount(ctx, "", 5, 5)
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "")
	assert.Equal(t, float64(0), tokens, "Update for empty user should have no effect")
}

func TestSlidingWindowTokenTracker_UpdateTokenCount_WithCustomWeights(t *testing.T) {
	config := VTCConfig{
		InputTokenWeight:  2.0,
		OutputTokenWeight: 0.5,
	}
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	// Test update with custom weights
	err := tracker.UpdateTokenCount(ctx, "user1", 10, 20)
	assert.NoError(t, err)
	tokens, _ := tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(30), tokens, "Update with custom weights")

	// Test second update with custom weights
	err = tracker.UpdateTokenCount(ctx, "user1", 5, 10)
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(45), tokens, "Second update with custom weights")
}

func TestTokenTrackerInterface(t *testing.T) {
	config := DefaultVTCConfig()
	
	var tracker TokenTracker = NewInMemorySlidingWindowTokenTracker(&config)
	
	ctx := context.Background()
	_, err := tracker.GetTokenCount(ctx, "user")
	assert.NoError(t, err)
	
	err = tracker.UpdateTokenCount(ctx, "user", 10, 20)
	assert.NoError(t, err)
}
