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

	tokens, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count should be 0")

	err = tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(40), tokens, "Token count after first update")

	tokens, err = tracker.GetTokenCount(ctx, "user2")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count for user2 should be 0")

	tokens, err = tracker.GetTokenCount(ctx, "")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Token count for empty user should be 0")

	tokens, _ = tracker.GetTokenCount(ctx, "nonexistent")
	assert.Equal(t, float64(0), tokens, "Token count for non-existent user should be 0")
}

func TestSlidingWindowTokenTracker_WindowBehavior(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	tokens, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count should be 0")

	err = tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(40), tokens, "Token count after first update")

	// Wait to move to next time bucket
	time.Sleep(10 * time.Millisecond)
	// Add tokens in next bucket
	err = tracker.UpdateTokenCount(ctx, "user1", 5, 5) // 5*1.0 + 5*2.0 = 15
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(55), tokens, "Sum over two buckets")

	// Wait for tokens to expire (beyond 100ms window)
	time.Sleep(110 * time.Millisecond)
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

	err := tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*2.0 = 40
	assert.NoError(t, err)
	tokens, _ := tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(40), tokens, "First update")

	err = tracker.UpdateTokenCount(ctx, "user1", 5, 10) // 40 + (5*1.0 + 10*2.0) = 40 + 25 = 65
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(65), tokens, "Second update")

	err = tracker.UpdateTokenCount(ctx, "user2", 100, 50) // 100*1.0 + 50*2.0 = 200
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user2")
	assert.Equal(t, float64(200), tokens, "Update for user2")

	err = tracker.UpdateTokenCount(ctx, "", 5, 5)
	assert.Error(t, err, "Update with empty user should error")
}

func TestSlidingWindowTokenTracker_UpdateTokenCount_WithCustomWeights(t *testing.T) {
	config := VTCConfig{
		InputTokenWeight:  2.0,
		OutputTokenWeight: 0.5,
	}
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds)) // 100ms window
	ctx := context.Background()

	err := tracker.UpdateTokenCount(ctx, "user1", 10, 20)
	assert.NoError(t, err)
	tokens, _ := tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(30), tokens, "Update with custom weights")

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

func TestTotalTokenCalculationDuringPruning(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	err := tracker.UpdateTokenCount(ctx, "user1", 10, 0)
	assert.NoError(t, err)
	err = tracker.UpdateTokenCount(ctx, "user2", 20, 0)
	assert.NoError(t, err)

	t1, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(10), t1, "user1 token count")
	t2, err := tracker.GetTokenCount(ctx, "user2")
	assert.NoError(t, err)
	assert.Equal(t, float64(20), t2, "user2 token count")

	total := t1 + t2
	assert.Equal(t, float64(30), total, "combined token count")

	time.Sleep(110 * time.Millisecond)

	t1, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), t1, "user1 tokens expired")
	t2, err = tracker.GetTokenCount(ctx, "user2")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), t2, "user2 tokens expired")
}

func TestGetMinMaxTokenCount(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	minVal, err := tracker.GetMinTokenCount(ctx)
	assert.NoError(t, err)
	assert.Equal(t, defaultTokenTrackerMinTokens, minVal, "default min tokens")
	maxVal, err := tracker.GetMaxTokenCount(ctx)
	assert.NoError(t, err)
	assert.Equal(t, defaultTokenTrackerMaxTokens, maxVal, "default max tokens")

	err = tracker.UpdateTokenCount(ctx, "user1", 500, 0)
	assert.NoError(t, err)
	minVal, _ = tracker.GetMinTokenCount(ctx)
	maxVal, _ = tracker.GetMaxTokenCount(ctx)
	assert.Equal(t, float64(500), minVal, "min after user1 update")
	assert.Equal(t, float64(500), maxVal, "max after user1 update")

	err = tracker.UpdateTokenCount(ctx, "user2", 1000, 0)
	assert.NoError(t, err)
	minVal, _ = tracker.GetMinTokenCount(ctx)
	maxVal, _ = tracker.GetMaxTokenCount(ctx)
	assert.Equal(t, float64(500), minVal, "min after user2 update")
	assert.Equal(t, float64(1000), maxVal, "max after user2 update")
}

func TestSlidingWindowTokenTracker_SecondsUnitWindow(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(1), WithTimeUnit(Seconds)) // 1s window
	ctx := context.Background()

	err := tracker.UpdateTokenCount(ctx, "user", 1, 0)
	assert.NoError(t, err)
	toks, err := tracker.GetTokenCount(ctx, "user")
	assert.NoError(t, err)
	assert.Equal(t, float64(1), toks, "initial token count in seconds window")

	// wait beyond 1 second (account for second-level granularity)
	time.Sleep(2100 * time.Millisecond)
	toks, err = tracker.GetTokenCount(ctx, "user")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), toks, "token expired after seconds window")
}
