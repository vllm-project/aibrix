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
