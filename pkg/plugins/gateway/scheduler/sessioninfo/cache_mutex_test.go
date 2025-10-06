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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestMutexCache_GetOrCreateForScheduler_NewSession tests the GetOrCreateForScheduler method.
func TestMutexCache_GetOrCreateForScheduler_NewSession(t *testing.T) {
	cache := NewMutexSessionCache()
	cst, waitTime := cache.GetOrCreateForScheduler("session1")

	assert.Equal(t, time.Duration(0), cst)
	assert.Equal(t, time.Duration(0), waitTime)
}

// TestMutexCache_UpdateState_Single tests the UpdateState method.
func TestMutexCache_UpdateState_Single(t *testing.T) {
	cache := NewMutexSessionCache()

	// First update (like a serial request)
	cache.UpdateState("session1", 0, 5*time.Second, 2*time.Second)
	state, _ := cache.GetState("session1")
	assert.Equal(t, 5*time.Second, state.CriticalPathServiceTime)
	assert.Equal(t, 2*time.Second, state.TotalWaitTime)

	// Second update (another serial request)
	// InheritedCST should be the CST from the previous state (5s)
	cache.UpdateState("session1", 5*time.Second, 3*time.Second, 1*time.Second)
	state, _ = cache.GetState("session1")
	assert.Equal(t, 8*time.Second, state.CriticalPathServiceTime) // 5s + 3s
	assert.Equal(t, 3*time.Second, state.TotalWaitTime)           // 2s + 1s
}

// TestMutexCache_UpdateState_Concurrent tests the UpdateState method.
func TestMutexCache_UpdateState_Concurrent(t *testing.T) {
	cache := NewMutexSessionCache()
	concurrency := 1000
	var wg sync.WaitGroup
	wg.Add(concurrency)

	// Simulate 100 parallel requests for the same session finishing.
	// All inherited CST=0, as they started when the session's CST was 0.
	for i := 0; i < concurrency; i++ {
		go func(execTimeMs int) {
			defer wg.Done()
			cache.UpdateState("session1", 0,
				time.Duration(execTimeMs)*time.Millisecond,
				10*time.Millisecond)
		}(i + 1)
	}
	wg.Wait()

	state, exists := cache.GetState("session1")
	assert.True(t, exists)

	// Final CST should be the max of all new path lengths,
	// which is max(0+1ms, 0+2ms, ... 0+100ms, ..., 0+1000ms) = 1000ms
	assert.Equal(t, 1000*time.Millisecond, state.CriticalPathServiceTime)

	// Total wait time should be the sum of all wait times:
	// 1000 * 10ms = 10000ms
	assert.Equal(t, 10000*time.Millisecond, state.TotalWaitTime)
}

// TestMutexCache_UpdateAffinity_Concurrent tests the UpdateAffinity method.
func TestMutexCache_UpdateAffinity_Concurrent(t *testing.T) {
	cache := NewMutexSessionCache()
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(podNum int) {
			defer wg.Done()
			cache.UpdateAffinity("session1",
				fmt.Sprintf("pod%d", podNum))
		}(i)
	}
	wg.Wait()

	state, exists := cache.GetState("session1")
	assert.True(t, exists)
	// Due to the race, we can't know the final value,
	// but it must be one of the values we set.
	assert.Contains(t, []string{"pod0", "pod1", "pod2", "pod3",
		"pod4", "pod5", "pod6", "pod7", "pod8", "pod9"},
		state.PodAffinity)
}

// TestMutexCache_Cleanup tests the Cleanup method.
func TestMutexCache_Cleanup(t *testing.T) {
	cache := NewMutexSessionCache()

	// Create session1
	cache.UpdateState("session1", 0, 1*time.Second, 0)
	// state1, _ := cache.GetState("session1")
	// t.Logf("Session 1 LastActivity: %v", state1.LastActivityTimestamp)

	// Wait for 2 seconds, making session1 stale relative to a 1.5s timeout
	time.Sleep(2 * time.Second)

	// Create/update session2, making it fresh
	cache.UpdateState("session2", 0, 1*time.Second, 0)
	// state2, _ := cache.GetState("session2")
	// t.Logf("Session 2 LastActivity: %v", state2.LastActivityTimestamp)

	// Now, cleanup sessions older than 1.5 seconds
	cache.cleanup(1500 * time.Millisecond)

	// session1 should be gone because it's ~2 seconds old
	_, exists := cache.GetState("session1")
	assert.False(t, exists, "session1 should be stale and cleaned up")

	// session2 should still exist because it's very fresh
	_, exists = cache.GetState("session2")
	assert.True(t, exists, "session2 should be fresh and remain")
}
