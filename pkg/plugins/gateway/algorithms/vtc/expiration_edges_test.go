/*
Copyright 2026 The Aibrix Team.

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
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
)

func newExpirationEdgeTracker() *InMemorySlidingWindowTokenTracker {
	config := VTCConfig{InputTokenWeight: 1, OutputTokenWeight: 1}
	return NewScopedInMemorySlidingWindowTokenTracker(&config, types.VTCTokenTrackerOverrides{
		WindowSize: 60000, TimeUnit: "milliseconds", MinTokens: 1000, MaxTokens: 8000,
	}).(*InMemorySlidingWindowTokenTracker)
}

func TestGlobalExtremaExpireWithoutUserReadOrUpdate(t *testing.T) {
	for _, first := range []string{"min", "max"} {
		t.Run(first, func(t *testing.T) {
			tracker := newExpirationEdgeTracker()
			ctx := context.Background()
			require.NoError(t, tracker.UpdateTokenCount(ctx, "low", 10, 0))
			require.NoError(t, tracker.UpdateTokenCount(ctx, "high", 100000, 0))
			backdateOldestBucket(tracker, "low", 2*time.Minute)
			backdateOldestBucket(tracker, "high", 2*time.Minute)
			// Either global getter must expire inactive users on its own.
			if first == "min" {
				minimum, err := tracker.GetMinTokenCount(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1000.0, minimum)
			} else {
				maximum, err := tracker.GetMaxTokenCount(ctx)
				require.NoError(t, err)
				assert.Equal(t, 8000.0, maximum)
			}
			minimum, err := tracker.GetMinTokenCount(ctx)
			require.NoError(t, err)
			maximum, err := tracker.GetMaxTokenCount(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1000.0, minimum)
			assert.Equal(t, 8000.0, maximum)
			assert.Empty(t, tracker.userBucketStore, "expired inactive users must be reclaimed")
		})
	}
}

func TestPartialExpirationReplacesTiedExtrema(t *testing.T) {
	tracker := newExpirationEdgeTracker()
	ctx := context.Background()
	require.NoError(t, tracker.UpdateTokenCount(ctx, "partial", 100, 0))
	backdateOldestBucket(tracker, "partial", 10*time.Millisecond)
	require.NoError(t, tracker.UpdateTokenCount(ctx, "partial", 50, 0))
	require.NoError(t, tracker.UpdateTokenCount(ctx, "tied", 150, 0))
	backdateOldestBucket(tracker, "partial", 2*time.Minute)
	minimum, err := tracker.GetMinTokenCount(ctx)
	require.NoError(t, err)
	maximum, err := tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 50.0, minimum)
	assert.Equal(t, 150.0, maximum, "the other user still owns the tied maximum")
	backdateOldestBucket(tracker, "tied", 2*time.Minute)
	maximum, err = tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 50.0, maximum, "the live partial bucket replaces the departed maximum")
	remaining, err := tracker.GetTokenCount(ctx, "partial")
	require.NoError(t, err)
	assert.Equal(t, 50.0, remaining)
}

func TestExpiredZeroUserCanReappear(t *testing.T) {
	tracker := newExpirationEdgeTracker()
	ctx := context.Background()
	require.NoError(t, tracker.UpdateTokenCount(ctx, "returning", 0, 0))
	backdateOldestBucket(tracker, "returning", 2*time.Minute)
	maximum, err := tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 8000.0, maximum)
	assert.Empty(t, tracker.userBucketStore)
	assert.Empty(t, tracker.userTotals)
	require.NoError(t, tracker.UpdateTokenCount(ctx, "returning", 7, 0))
	count, err := tracker.GetTokenCount(ctx, "returning")
	require.NoError(t, err)
	assert.Equal(t, 7.0, count)
	minimum, err := tracker.GetMinTokenCount(ctx)
	require.NoError(t, err)
	maximum, err = tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7.0, minimum)
	assert.Equal(t, 7.0, maximum)
}

func TestExpirationCutoffRetainsEqualTimestamp(t *testing.T) {
	for _, unit := range []TimeUnit{Minutes, Seconds, Milliseconds} {
		t.Run(timeUnitDuration[unit].String(), func(t *testing.T) {
			config := VTCConfig{InputTokenWeight: 1, OutputTokenWeight: 1}
			tracker := NewInMemorySlidingWindowTokenTracker(&config,
				WithWindowSize(60000), WithTimeUnit(unit)).(*InMemorySlidingWindowTokenTracker)
			ctx := context.Background()
			require.NoError(t, tracker.UpdateTokenCount(ctx, "boundary", 5, 0))
			cutoff := tracker.userBucketStore["boundary"].buckets.Front().Value.(*bucketNode).timestamp
			// Supply an exact cutoff instead of relying on wall-clock scheduling.
			tracker.mu.Lock()
			tracker.expireUsers(cutoff)
			tracker.mu.Unlock()
			count, err := tracker.GetTokenCount(ctx, "boundary")
			require.NoError(t, err)
			assert.Equal(t, 5.0, count, "a bucket exactly at cutoff remains active")
			tracker.mu.Lock()
			tracker.expireUsers(cutoff + 1)
			tracker.mu.Unlock()
			count, err = tracker.GetTokenCount(ctx, "boundary")
			require.NoError(t, err)
			assert.Zero(t, count, "a bucket strictly before cutoff expires")
		})
	}
}
