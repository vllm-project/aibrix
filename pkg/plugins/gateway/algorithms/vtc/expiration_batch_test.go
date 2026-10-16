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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertExpirationBatchState(t *testing.T, tracker *InMemorySlidingWindowTokenTracker, totals map[string]float64, reverse map[float64]map[string]struct{}, minimum, maximum float64) {
	t.Helper()
	ctx := context.Background()
	lo, err := tracker.GetMinTokenCount(ctx)
	require.NoError(t, err)
	hi, err := tracker.GetMaxTokenCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, minimum, lo)
	assert.Equal(t, maximum, hi)
	assert.Equal(t, totals, tracker.userTotals)
	assert.Equal(t, reverse, tracker.totalsToUsers)
	require.Len(t, tracker.expirations, len(tracker.userBucketStore))
	seen := make(map[string]bool)
	for _, data := range tracker.expirations {
		require.False(t, seen[data.user], "one expiration entry per user")
		seen[data.user] = true
		require.Positive(t, data.buckets.Len())
		assert.Same(t, tracker.userBucketStore[data.user], data)
		root := tracker.expirations[0].buckets.Front().Value.(*bucketNode).timestamp
		assert.LessOrEqual(t, root, data.buckets.Front().Value.(*bucketNode).timestamp)
	}
}

func TestExpirationBatchRemovesFullPartialTiedAndZeroUsers(t *testing.T) {
	tracker := newExpirationEdgeTracker()
	ctx := context.Background()
	require.NoError(t, tracker.UpdateTokenCount(ctx, "partial", 100, 0))
	backdateOldestBucket(tracker, "partial", 10*time.Millisecond)
	require.NoError(t, tracker.UpdateTokenCount(ctx, "partial", 50, 0))
	require.NoError(t, tracker.UpdateTokenCount(ctx, "tied", 150, 0))
	require.NoError(t, tracker.UpdateTokenCount(ctx, "full", 300, 0))
	require.NoError(t, tracker.UpdateTokenCount(ctx, "zero", 0, 0))
	assertExpirationBatchState(t, tracker, map[string]float64{"partial": 150, "tied": 150, "full": 300},
		map[float64]map[string]struct{}{150: {"partial": {}, "tied": {}}, 300: {"full": {}}}, 150, 300)
	for _, user := range []string{"partial", "full", "zero"} {
		backdateOldestBucket(tracker, user, 2*time.Minute)
	}
	// A global read expires the whole due batch and reinserts the partial user.
	assertExpirationBatchState(t, tracker, map[string]float64{"partial": 50, "tied": 150},
		map[float64]map[string]struct{}{50: {"partial": {}}, 150: {"tied": {}}}, 50, 150)
	backdateOldestBucket(tracker, "tied", 2*time.Minute)
	assertExpirationBatchState(t, tracker, map[string]float64{"partial": 50},
		map[float64]map[string]struct{}{50: {"partial": {}}}, 50, 50)
	backdateOldestBucket(tracker, "partial", 2*time.Minute)
	assertExpirationBatchState(t, tracker, map[string]float64{}, map[float64]map[string]struct{}{}, 1000, 8000)
}

func TestRepeatedUpdatesKeepOneExpirationEntry(t *testing.T) {
	tracker := newExpirationEdgeTracker()
	for i := 0; i < 200; i++ {
		require.NoError(t, tracker.UpdateTokenCount(context.Background(), "live", 1, 0))
		require.Len(t, tracker.expirations, 1, "updates must not append duplicate user entries")
	}
	assertExpirationBatchState(t, tracker, map[string]float64{"live": 200},
		map[float64]map[string]struct{}{200: {"live": {}}}, 200, 200)
}

func TestConcurrentReadsAndUpdatesExpireInactiveExtreme(t *testing.T) {
	tracker := newExpirationEdgeTracker()
	ctx := context.Background()
	require.NoError(t, tracker.UpdateTokenCount(ctx, "departed", 100000, 0))
	backdateOldestBucket(tracker, "departed", 2*time.Minute)
	start := make(chan struct{})
	errors := make(chan error, 16)
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for i := 0; i < 20; i++ {
				if _, err := tracker.GetMaxTokenCount(ctx); err != nil {
					errors <- err
					return
				}
				if err := tracker.UpdateTokenCount(ctx, "live", 1, 0); err != nil {
					errors <- err
					return
				}
				count, err := tracker.GetTokenCount(ctx, "live")
				if err != nil || count < 1 || count > 320 {
					errors <- fmt.Errorf("live count=%v: %v", count, err)
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	count, err := tracker.GetTokenCount(ctx, "live")
	require.NoError(t, err)
	require.Equal(t, 320.0, count)
	assertExpirationBatchState(t, tracker, map[string]float64{"live": 320},
		map[float64]map[string]struct{}{320: {"live": {}}}, 320, 320)
}
