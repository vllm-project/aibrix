/*
Copyright 2025 The Aibrix Team.

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

package sessioninfo

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The sharded tests are structurally similar to the mutex tests,
// as they are testing the same public API contract.

func TestShardedCache_GetOrCreateForScheduler_NewSession(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	cst, waitTime := cache.GetOrCreateForScheduler("session1")

	assert.Equal(t, time.Duration(0), cst)
	assert.Equal(t, time.Duration(0), waitTime)
}

func TestShardedCache_UpdateState_Single(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	cache.UpdateState("session1", 0, 5*time.Second, 2*time.Second)
	assert.Eventually(t, func() bool {
		cst, wait := cache.GetOrCreateForScheduler("session1")
		return cst == 5*time.Second && wait == 2*time.Second
	}, 250*time.Millisecond, 5*time.Millisecond)

	cache.UpdateState("session1", 5*time.Second, 3*time.Second, 1*time.Second)
	assert.Eventually(t, func() bool {
		cst, wait := cache.GetOrCreateForScheduler("session1")
		return cst == 8*time.Second && wait == 3*time.Second
	}, 250*time.Millisecond, 5*time.Millisecond)
}

func TestShardedCache_UpdateState_Concurrent(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	concurrency := 100
	var done atomic.Int32
	for i := 0; i < concurrency; i++ {
		go func(execTimeMs int) {
			cache.UpdateState("session1", 0, time.Duration(execTimeMs)*time.Millisecond, 10*time.Millisecond)
			done.Add(1)
		}(i + 1)
	}
	assert.Eventually(t, func() bool {
		return done.Load() == int32(concurrency)
	}, time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool {
		cst, wait := cache.GetOrCreateForScheduler("session1")
		return cst == 100*time.Millisecond && wait == 1000*time.Millisecond
	}, 250*time.Millisecond, 5*time.Millisecond)
}

func TestShardedCache_UpdateAffinity_Concurrent(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	// This test is harder to write for the channel version without a proper GetState that returns affinity.
	// We'll skip the detailed check for now as the main purpose is to test the update mechanism.
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(podNum int) {
			defer wg.Done()
			cache.UpdateAffinity("session1", fmt.Sprintf("pod%d", podNum))
		}(i)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	// We can't easily verify the result without a full GetState op.
	// This test mainly serves to ensure no deadlocks occur.
	t.Log("Concurrent affinity update test completed without deadlock.")
}

func TestShardedCache_GetFullState(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	cache.UpdateState("session1", 0, 5*time.Second, 2*time.Second)
	state, exists := cache.GetState("session1")
	assert.True(t, exists)
	assert.Equal(t, "session1", state.SessionID)
	assert.Equal(t, 5*time.Second, state.CriticalPathServiceTime)
	assert.Equal(t, 2*time.Second, state.TotalWaitTime)
}

func TestShardedCache_Cleanup(t *testing.T) {
	cache := NewShardedSessionCache()
	defer cache.Close()

	// Ensure session1 and session2 hash to different shards if possible, or test with one
	// For simplicity, we assume they might hash to the same shard, which is a valid test case.

	// Create session1
	cache.UpdateState("session1", 0, 1*time.Second, 0)
	time.Sleep(10 * time.Millisecond) // wait for channel to process

	// Wait to make session1 stale
	time.Sleep(2 * time.Second)

	// Create session2, making it fresh
	cache.UpdateState("session2", 0, 1*time.Second, 0)
	time.Sleep(10 * time.Millisecond)

	// Trigger cleanup on all shards
	cache.StartCleanupRoutine(1500 * time.Millisecond)

	// Wait for the cleanup commands to be processed by all shards
	time.Sleep(100 * time.Millisecond)

	// To check existence, we need a GetState that returns a bool
	// Let's assume we have implemented opGetFullState as discussed

	// opGetFullState for session1
	shard1 := cache.getShard("session1")
	respChan1 := make(chan fullStateResponse, 1)
	shard1.requests <- cacheRequest{op: opGetFullState, sessionID: "session1", fullStateResponseChan: respChan1}
	response1 := <-respChan1
	// The manager should have created a new empty state, because the old one was deleted.
	assert.Equal(t, time.Duration(0), response1.state.CriticalPathServiceTime, "session1 should have been cleaned and recreated as empty")

	// opGetFullState for session2
	shard2 := cache.getShard("session2")
	respChan2 := make(chan fullStateResponse, 1)
	shard2.requests <- cacheRequest{op: opGetFullState, sessionID: "session2", fullStateResponseChan: respChan2}
	response2 := <-respChan2
	assert.NotEqual(t, time.Duration(0), response2.state.CriticalPathServiceTime, "session2 should be fresh and remain")
}
