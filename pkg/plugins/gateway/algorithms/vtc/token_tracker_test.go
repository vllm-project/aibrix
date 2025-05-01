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
	"fmt"
	"sync"
	"sync/atomic"
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

func TestTokenTrackerThreadSafety(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	// Number of concurrent goroutines
	const numGoroutines = 10
	// Number of operations per goroutine
	const opsPerGoroutine = 100

	// Use a WaitGroup to coordinate goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start multiple goroutines to update and read token counts concurrently
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Each goroutine uses its own user ID
			userID := fmt.Sprintf("user-%d", id)

			for j := 0; j < opsPerGoroutine; j++ {
				// Alternate between read and write operations
				if j%2 == 0 {
					// Update token count
					err := tracker.UpdateTokenCount(ctx, userID, float64(j), float64(j))
					assert.NoError(t, err)
				} else {
					// Read token count
					_, err := tracker.GetTokenCount(ctx, userID)
					assert.NoError(t, err)
				}
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify that all users have the expected token counts
	for i := 0; i < numGoroutines; i++ {
		userID := fmt.Sprintf("user-%d", i)
		tokens, err := tracker.GetTokenCount(ctx, userID)
		assert.NoError(t, err)

		// Calculate expected tokens: sum of all even j values from 0 to opsPerGoroutine-1
		// Each update adds j input tokens and j output tokens with weights from config
		expectedTokens := 0.0
		for j := 0; j < opsPerGoroutine; j += 2 {
			expectedTokens += float64(j)*config.InputTokenWeight + float64(j)*config.OutputTokenWeight
		}

		assert.Equal(t, expectedTokens, tokens, "Token count for %s should match expected value", userID)
	}

	// Also test min/max functions
	min, err := tracker.GetMinTokenCount(ctx)
	assert.NoError(t, err)
	max, err := tracker.GetMaxTokenCount(ctx)
	assert.NoError(t, err)
	assert.True(t, min <= max, "Min token count should be less than or equal to max token count")
}

func TestTokenTrackerThreadSafety_SharedUser(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	// Number of concurrent goroutines all updating the same user
	const numGoroutines = 20
	// Number of operations per goroutine
	const opsPerGoroutine = 50
	// All goroutines update the same user
	const sharedUserID = "shared-user"

	// Use atomic counter to track the expected total
	var expectedTotal int64 = 0

	// Use a WaitGroup to coordinate goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start multiple goroutines to update the same user's tokens concurrently
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < opsPerGoroutine; j++ {
				// Each goroutine adds a fixed amount of tokens
				inputTokens := float64(id + 1)
				outputTokens := float64(id + 1)

				err := tracker.UpdateTokenCount(ctx, sharedUserID, inputTokens, outputTokens)
				assert.NoError(t, err)

				// Track expected total with atomic operations
				atomic.AddInt64(&expectedTotal, int64(inputTokens*config.InputTokenWeight+outputTokens*config.OutputTokenWeight))
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify the final token count
	tokens, err := tracker.GetTokenCount(ctx, sharedUserID)
	assert.NoError(t, err)
	assert.Equal(t, float64(expectedTotal), tokens, "Token count for shared user should match expected value")
}

func TestTokenTrackerThreadSafety_Expiration(t *testing.T) {
	config := DefaultVTCConfig()
	// Use a very short window to test expiration
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(20), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	// Number of concurrent goroutines
	const numGoroutines = 10
	// Number of operations per goroutine
	const opsPerGoroutine = 20

	// Use a WaitGroup to coordinate goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start multiple goroutines to update and read token counts with expiration
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			userID := fmt.Sprintf("exp-user-%d", id)

			for j := 0; j < opsPerGoroutine; j++ {
				// Add tokens
				err := tracker.UpdateTokenCount(ctx, userID, 1.0, 1.0)
				assert.NoError(t, err)

				// Sleep to allow some tokens to expire (stagger the sleeps)
				if j%5 == 0 {
					time.Sleep(time.Duration(5+id) * time.Millisecond)
				}

				// Read token count
				_, err = tracker.GetTokenCount(ctx, userID)
				assert.NoError(t, err)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Wait for all tokens to expire
	time.Sleep(30 * time.Millisecond)

	// Verify all tokens expired
	for i := 0; i < numGoroutines; i++ {
		userID := fmt.Sprintf("exp-user-%d", i)
		tokens, err := tracker.GetTokenCount(ctx, userID)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, tokens, "All tokens should have expired")
	}
}

func TestTokenTrackerThreadSafety_MinMaxRecalculation(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
	ctx := context.Background()

	// Number of concurrent goroutines
	const numGoroutines = 10

	// Use a WaitGroup to coordinate goroutines
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start multiple goroutines to update token counts with values that will trigger min/max recalculations
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Each goroutine uses a different user
			userID := fmt.Sprintf("minmax-user-%d", id)

			// Add a specific token count based on the goroutine ID
			tokenValue := float64(100 * (id + 1))
			err := tracker.UpdateTokenCount(ctx, userID, tokenValue, 0)
			assert.NoError(t, err)

			// Get min/max to trigger potential race conditions
			_, err = tracker.GetMinTokenCount(ctx)
			assert.NoError(t, err)
			_, err = tracker.GetMaxTokenCount(ctx)
			assert.NoError(t, err)

			// Sleep a bit to stagger operations
			time.Sleep(time.Duration(id) * time.Millisecond)

			// Remove the tokens to trigger min/max recalculation
			time.Sleep(110 * time.Millisecond) // Wait for tokens to expire

			// Add a different token count
			newTokenValue := float64(50 * (id + 1))
			err = tracker.UpdateTokenCount(ctx, userID, newTokenValue, 0)
			assert.NoError(t, err)
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify min and max are consistent
	min, err := tracker.GetMinTokenCount(ctx)
	assert.NoError(t, err)
	max, err := tracker.GetMaxTokenCount(ctx)
	assert.NoError(t, err)
	assert.True(t, min <= max, "Min token count should be less than or equal to max token count")

	// The expected min should be 50 (from user-0)
	expectedMin := 50.0
	// The expected max should be 500 (from user-9)
	expectedMax := 50.0 * float64(numGoroutines)

	assert.Equal(t, expectedMin, min, "Min token count should match expected value")
	assert.Equal(t, expectedMax, max, "Max token count should match expected value")
}

func TestTokenExpirationScenarios(t *testing.T) {
	tests := []struct {
		name         string
		setupFunc    func(TokenTracker, context.Context) error
		verifyFunc   func(TokenTracker, context.Context, *testing.T)
		expiryWaitMs int
	}{
		{
			name: "MultipleUsersExpiration",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				// Add tokens for multiple users
				err := tracker.UpdateTokenCount(ctx, "user1", 100, 0)
				if err != nil {
					return err
				}
				return tracker.UpdateTokenCount(ctx, "user2", 200, 0)
			},
			verifyFunc: func(tracker TokenTracker, ctx context.Context, t *testing.T) {
				// Verify min and max are set correctly before expiration
				min, err := tracker.GetMinTokenCount(ctx)
				assert.NoError(t, err)
				assert.Equal(t, float64(100), min, "min should be 100")
				max, err := tracker.GetMaxTokenCount(ctx)
				assert.NoError(t, err)
				assert.Equal(t, float64(200), max, "max should be 200")

				// After all tokens expire, GetTokenCount should return 0 for both users
				tokensUser1, err := tracker.GetTokenCount(ctx, "user1")
				assert.NoError(t, err)
				assert.Equal(t, float64(0), tokensUser1, "user1 tokens should be 0 after expiration")
				tokensUser2, err := tracker.GetTokenCount(ctx, "user2")
				assert.NoError(t, err)
				assert.Equal(t, float64(0), tokensUser2, "user2 tokens should be 0 after expiration")
			},
			expiryWaitMs: 110,
		},
		{
			name: "SingleUserExpiration",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				// Add tokens for a single user
				return tracker.UpdateTokenCount(ctx, "user1", 100, 0)
			},
			verifyFunc: func(tracker TokenTracker, ctx context.Context, t *testing.T) {
				// Verify min and max are set correctly before expiration
				min, err := tracker.GetMinTokenCount(ctx)
				assert.NoError(t, err)
				assert.Equal(t, float64(100), min, "min should be 100")
				max, err := tracker.GetMaxTokenCount(ctx)
				assert.NoError(t, err)
				assert.Equal(t, float64(100), max, "max should be 100")

				// After tokens expire, token count should be 0
				tokens, err := tracker.GetTokenCount(ctx, "user1")
				assert.NoError(t, err)
				assert.Equal(t, float64(0), tokens, "tokens should be 0 after expiration")
			},
			expiryWaitMs: 110,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := DefaultVTCConfig()
			tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
			ctx := context.Background()

			// Run setup
			err := tc.setupFunc(tracker, ctx)
			assert.NoError(t, err)

			// Wait for tokens to expire
			time.Sleep(time.Duration(tc.expiryWaitMs) * time.Millisecond)

			// Verify the result
			tc.verifyFunc(tracker, ctx, t)
		})
	}
}

func TestTokenTrackerEdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		setupFunc      func(TokenTracker, context.Context) error
		updateFunc     func(TokenTracker, context.Context) error
		expectedTokens float64
		message        string
	}{
		{
			name: "ZeroTokenUpdateIsNoOp",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", 50, 25) // 50 + 25*2 = 100
			},
			updateFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", 0, 0)
			},
			expectedTokens: 100,
			message:        "token count should be unchanged after zero-token update",
		},
		{
			name: "SameBucketAccumulation",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", 5, 0)
			},
			updateFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", 7, 0)
			},
			expectedTokens: 12,
			message:        "tokens should accumulate in the same time bucket",
		},
		{
			name: "NegativeTokensClampToZero",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				// No setup needed
				return nil
			},
			updateFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", -5, 0)
			},
			expectedTokens: 0,
			message:        "negative tokens should be clamped to zero",
		},
		{
			name: "NegativeTokenUpdateIsNoOp",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				return tracker.UpdateTokenCount(ctx, "user1", 50, 25) // 50 + 25*2 = 100
			},
			updateFunc: func(tracker TokenTracker, ctx context.Context) error {
				// Update with negative tokens should be a no-op
				err := tracker.UpdateTokenCount(ctx, "user1", -10, -5)
				if err != nil {
					return err
				}
				// Multiple negative updates also shouldn't change anything
				return tracker.UpdateTokenCount(ctx, "user1", -20, -15)
			},
			expectedTokens: 100,
			message:        "token count should be unchanged after negative token updates",
		},
		{
			name: "PositiveAfterNegativeTokens",
			setupFunc: func(tracker TokenTracker, ctx context.Context) error {
				// First add negative tokens (should be clamped to 0)
				err := tracker.UpdateTokenCount(ctx, "user1", -10, -5)
				if err != nil {
					return err
				}
				return nil
			},
			updateFunc: func(tracker TokenTracker, ctx context.Context) error {
				// Then add positive tokens
				return tracker.UpdateTokenCount(ctx, "user1", 30, 10) // 30 + 10*2 = 50
			},
			expectedTokens: 50,
			message:        "positive tokens should be added correctly after negative tokens",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := DefaultVTCConfig()
			tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(100), WithTimeUnit(Milliseconds))
			ctx := context.Background()

			// Run setup
			err := tc.setupFunc(tracker, ctx)
			assert.NoError(t, err)

			// Run the update function
			err = tc.updateFunc(tracker, ctx)
			assert.NoError(t, err)

			// Verify the result
			tokens, err := tracker.GetTokenCount(ctx, "user1")
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedTokens, tokens, tc.message)
		})
	}
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
