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

package types

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func statsAdded(s *StatsUpdate) bool {
	select {
	case <-s.added:
		return true
	default:
		return false
	}
}

// TestStatsUpdateRecycledWinnerCannotCloseNextRequest covers a TryAdd winner that is
// still running when its RoutingContext goes back to the pool and is reset for the next
// request, e.g. the queue router's serve goroutine after the requester timed out and
// released the context. If the channel lived on the RoutingContext itself, the stale
// winner's DoneAdd would close the next request's channel, releasing its waiters early,
// and the next request's own winner would then panic closing it again.
func TestStatsUpdateRecycledWinnerCannotCloseNextRequest(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("slo"), "m", "msg", "req-1", "")
	stale := ctx.StatsUpdate()
	require.True(t, stale.TryAdd())

	// The context is recycled for the next request while the stale winner is running.
	ctx.reset(context.Background(), RoutingAlgorithm("slo"), "m", "msg", "req-2", "")
	next := ctx.StatsUpdate()
	require.NotSame(t, stale, next)

	stale.DoneAdd()
	assert.True(t, statsAdded(stale))
	assert.False(t, statsAdded(next), "a stale winner must not release the next request's waiters")

	require.True(t, next.TryAdd(), "the next request must get its own winner")
	assert.NotPanics(t, next.DoneAdd)
	assert.True(t, statsAdded(next))
}

func TestStatsUpdateLoserWaitsForWinner(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("slo"), "m", "msg", "req-1", "")
	stats := ctx.StatsUpdate()
	require.True(t, stats.TryAdd())
	require.False(t, stats.TryAdd())
	assert.False(t, statsAdded(stats))

	stats.DoneAdd()
	stats.WaitAdded() // Returns once the winner is done.
	assert.True(t, ctx.CanDoneStats())
	assert.False(t, ctx.CanDoneStats())
}

// The gateway builds bare struct literals for metric emission. They have not been through
// reset(), so the state is created on first use instead of being nil.
func TestStatsUpdateBareRoutingContext(t *testing.T) {
	bare := &RoutingContext{}
	stats := bare.StatsUpdate()
	require.NotNil(t, stats)
	assert.Same(t, stats, bare.StatsUpdate())

	require.True(t, stats.TryAdd())
	assert.NotPanics(t, stats.DoneAdd)
	stats.WaitAdded()
	assert.True(t, bare.CanDoneStats())
}
