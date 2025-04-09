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

	"github.com/stretchr/testify/assert"
)

func TestInMemoryTokenTracker_GetTokenCount(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemoryTokenTracker(&config)
	ctx := context.Background()

	// Test initial count for a new user
	tokens, err := tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(0), tokens, "Initial token count should be 0")

	// Test count after update
	err = tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*1.5 = 32.5
	assert.NoError(t, err)
	tokens, err = tracker.GetTokenCount(ctx, "user1")
	assert.NoError(t, err)
	assert.Equal(t, float64(32.5), tokens, "Token count after first update")

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

func TestInMemoryTokenTracker_UpdateTokenCount(t *testing.T) {
	config := DefaultVTCConfig()
	tracker := NewInMemoryTokenTracker(&config)
	ctx := context.Background()

	// Test initial update
	err := tracker.UpdateTokenCount(ctx, "user1", 10, 15) // 10*1.0 + 15*1.5 = 32.5
	assert.NoError(t, err)
	tokens, _ := tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(32.5), tokens, "First update")

	// Test second update for the same user
	err = tracker.UpdateTokenCount(ctx, "user1", 5, 10) // 32.5 + (5*1.0 + 10*1.5) = 32.5 + 20 = 52.5
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user1")
	assert.Equal(t, float64(52.5), tokens, "Second update")

	// Test update for a different user
	err = tracker.UpdateTokenCount(ctx, "user2", 100, 50) // 100*1.0 + 50*1.5 = 175
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "user2")
	assert.Equal(t, float64(175), tokens, "Update for user2")

	// Test update for empty user (should do nothing)
	err = tracker.UpdateTokenCount(ctx, "", 5, 5)
	assert.NoError(t, err)
	tokens, _ = tracker.GetTokenCount(ctx, "")
	assert.Equal(t, float64(0), tokens, "Update for empty user should have no effect")
}

func TestInMemoryTokenTracker_UpdateTokenCount_WithWeights(t *testing.T) {
	config := VTCConfig{
		InputTokenWeight:  2.0,
		OutputTokenWeight: 0.5,
	}
	tracker := NewInMemoryTokenTracker(&config)
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
