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
	"github.com/stretchr/testify/require"
)

// TestInterfaceCompliance verifies that both implementations satisfy the SessionCache interface
func TestInterfaceCompliance(t *testing.T) {
	var _ SessionCache = (*MutexSessionCache)(nil)
	var _ SessionCache = (*ShardedSessionCache)(nil)
	t.Log("Both MutexSessionCache and ShardedSessionCache implement SessionCache interface")
}

// cacheTestSuite runs a comprehensive test suite against any SessionCache implementation
func cacheTestSuite(t *testing.T, name string, factory func() SessionCache, needsClose bool) {
	t.Run(name, func(t *testing.T) {
		t.Run("GetOrCreateForScheduler_NewSession", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			cst, waitTime := cache.GetOrCreateForScheduler("session1")
			assert.Equal(t, time.Duration(0), cst, "New session should have zero CST")
			assert.Equal(t, time.Duration(0), waitTime, "New session should have zero wait time")
		})

		t.Run("UpdateState_SingleRequest", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			// First request
			cache.UpdateState("session1", 0, 5*time.Second, 2*time.Second)

			// For sharded cache, need to wait for async processing
			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			state, exists := cache.GetState("session1")
			require.True(t, exists, "Session should exist after update")
			assert.Equal(t, 5*time.Second, state.CriticalPathServiceTime, "CST should be 5s")
			assert.Equal(t, 2*time.Second, state.TotalWaitTime, "Wait time should be 2s")

			// Second request (serial)
			cache.UpdateState("session1", 5*time.Second, 3*time.Second, 1*time.Second)

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			state, exists = cache.GetState("session1")
			require.True(t, exists)
			assert.Equal(t, 8*time.Second, state.CriticalPathServiceTime, "CST should be 5s + 3s = 8s")
			assert.Equal(t, 3*time.Second, state.TotalWaitTime, "Wait time should be 2s + 1s = 3s")
		})

		t.Run("UpdateState_ConcurrentRequests", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			concurrency := 100
			var wg sync.WaitGroup
			wg.Add(concurrency)

			// Simulate 100 parallel requests with different execution times
			for i := 0; i < concurrency; i++ {
				go func(execTimeMs int) {
					defer wg.Done()
					cache.UpdateState("session1", 0,
						time.Duration(execTimeMs)*time.Millisecond,
						10*time.Millisecond)
				}(i + 1)
			}
			wg.Wait()

			// Wait for async processing if needed
			if needsClose {
				time.Sleep(100 * time.Millisecond)
			}

			state, exists := cache.GetState("session1")
			require.True(t, exists)

			// CST should be max of all execution times: max(1ms, 2ms, ..., 100ms) = 100ms
			assert.Equal(t, 100*time.Millisecond, state.CriticalPathServiceTime,
				"CST should be the maximum execution time")

			// Total wait time should be sum: 100 * 10ms = 1000ms
			assert.Equal(t, 1000*time.Millisecond, state.TotalWaitTime,
				"Total wait time should be sum of all wait times")
		})

		t.Run("UpdateAffinity", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			cache.UpdateAffinity("session1", "pod-1")

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			state, exists := cache.GetState("session1")
			require.True(t, exists)
			assert.Equal(t, "pod-1", state.PodAffinity, "Pod affinity should be set")

			// Update to different pod
			cache.UpdateAffinity("session1", "pod-2")

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			state, exists = cache.GetState("session1")
			require.True(t, exists)
			assert.Equal(t, "pod-2", state.PodAffinity, "Pod affinity should be updated")
		})

		t.Run("GetState_NonExistentSession", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			state, exists := cache.GetState("non-existent")
			assert.False(t, exists, "Non-existent session should return false")
			assert.Equal(t, SessionState{}, state, "Should return zero value for non-existent session")
		})

		t.Run("MultipleSessionsIsolation", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			// Update multiple sessions
			cache.UpdateState("session1", 0, 5*time.Second, 1*time.Second)
			cache.UpdateState("session2", 0, 10*time.Second, 2*time.Second)
			cache.UpdateState("session3", 0, 3*time.Second, 1*time.Second)

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			// Verify each session has independent state
			state1, _ := cache.GetState("session1")
			state2, _ := cache.GetState("session2")
			state3, _ := cache.GetState("session3")

			assert.Equal(t, 5*time.Second, state1.CriticalPathServiceTime)
			assert.Equal(t, 10*time.Second, state2.CriticalPathServiceTime)
			assert.Equal(t, 3*time.Second, state3.CriticalPathServiceTime)
		})

		t.Run("StartCleanupRoutine", func(t *testing.T) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			// Create a session
			cache.UpdateState("session1", 0, 1*time.Second, 0)

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			// Verify it exists
			_, exists := cache.GetState("session1")
			require.True(t, exists, "Session should exist before cleanup")

			// Start cleanup with very short timeout
			stop := cache.StartCleanupRoutine(100*time.Millisecond, 50*time.Millisecond)
			defer stop()

			// Wait for session to become stale and cleanup to run
			time.Sleep(200 * time.Millisecond)

			// Session should be cleaned up
			_, exists = cache.GetState("session1")
			assert.False(t, exists, "Stale session should be cleaned up")

			// Create a new session after cleanup started
			cache.UpdateState("session2", 0, 1*time.Second, 0)

			if needsClose {
				time.Sleep(50 * time.Millisecond)
			}

			// It should exist (fresh)
			_, exists = cache.GetState("session2")
			assert.True(t, exists, "Fresh session should not be cleaned up")
		})
	})
}

// TestAllImplementations runs the test suite against all implementations
func TestAllImplementations(t *testing.T) {
	cacheTestSuite(t, "MutexSessionCache", func() SessionCache {
		return NewMutexSessionCache()
	}, false)

	cacheTestSuite(t, "ShardedSessionCache", func() SessionCache {
		return NewShardedSessionCache()
	}, true)
}

// BenchmarkImplementations compares performance of different implementations
func BenchmarkImplementations(b *testing.B) {
	benchmarkCache := func(b *testing.B, name string, factory func() SessionCache, needsClose bool) {
		b.Run(name, func(b *testing.B) {
			cache := factory()
			if needsClose {
				defer cache.(*ShardedSessionCache).Close()
			}

			b.Run("GetOrCreateForScheduler", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					i := 0
					for pb.Next() {
						sessionID := fmt.Sprintf("session-%d", i%1000)
						cache.GetOrCreateForScheduler(sessionID)
						i++
					}
				})
			})

			b.Run("UpdateState", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					i := 0
					for pb.Next() {
						sessionID := fmt.Sprintf("session-%d", i%1000)
						cache.UpdateState(sessionID, 0, time.Millisecond, time.Millisecond)
						i++
					}
				})
			})
		})
	}

	benchmarkCache(b, "MutexSessionCache", func() SessionCache {
		return NewMutexSessionCache()
	}, false)

	benchmarkCache(b, "ShardedSessionCache", func() SessionCache {
		return NewShardedSessionCache()
	}, true)
}

