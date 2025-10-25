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
	"hash"
	"hash/fnv"
	"sync"
	"time"
)

var hasherPool = sync.Pool{
	New: func() interface{} {
		return fnv.New64a()
	},
}

// --- Internal channel communication structs ---
type cacheOp int // operation code for cacheRequest

const (
	opGetForScheduler cacheOp = iota
	opGetFullState
	opUpdateState
	opUpdateAffinity
	opCleanup
)

// cacheRequest is the message format for all shard channels.
type cacheRequest struct {
	op                    cacheOp
	sessionID             string
	updatePayload         updatePayload
	affinityPayload       string
	cleanupPayload        cleanupPayload
	schedulerInfoRespChan chan schedulerInfoResponse
	fullStateResponseChan chan fullStateResponse
}

// updatePayload is the payload for opUpdateState
type updatePayload struct {
	inheritedCST  time.Duration
	executionTime time.Duration
	waitTime      time.Duration
}

// schedulerInfoResponse is the response for opGetForScheduler
type schedulerInfoResponse struct {
	cst      time.Duration
	waitTime time.Duration
}

// cleanupPayload is the payload for opCleanup
type cleanupPayload struct {
	timeout time.Duration
}

// fullStateResponse is the response for opGetFullState
type fullStateResponse struct {
	state *SessionState
}

// --- Shard and ShardedCache implementation ---

// shardCount is the number of independent shards used to reduce lock contention
// in high-concurrency scenarios.
//
// Purpose:
//   - Reduces lock contention probability to ~1/shardCount
//   - Enables parallel processing: multiple sessions can be accessed simultaneously
//   - Each shard has its own goroutine and processes requests via a channel (Actor Model)
//   - No locks needed within each shard (single-threaded access to its map)
//
// Why 256?
//   - Power of 2: Enables fast bitwise AND for hash-to-shard mapping (hash & 255)
//   - Balanced: Not too small (still contention) or too large (goroutine overhead)
//   - Multi-core friendly: Modern servers have 32-128 cores; 256 shards can fully utilize them
//
// Performance Trade-offs:
//   - Higher values: Better concurrency, more goroutines/memory overhead
//   - Lower values: Less overhead, more lock contention
//   - Recommended range: 64-512 (must be power of 2)
//
// When to adjust:
//   - Increase (e.g., 512) for extremely high concurrency (10K+ QPS)
//   - Decrease (e.g., 64, 128) for lower concurrency or memory-constrained environments
//
// Note: This is for single-process concurrency optimization, NOT distributed partitioning.
const shardCount = 256 // Must be a power of 2 for bitwise AND optimization

// cacheShard is a single shard of the sharded cache.
type cacheShard struct {
	sessions map[string]*SessionState // sessionID -> *SessionState
	requests chan cacheRequest        // Channel for requests to this shard
	done     chan struct{}            // Channel for shutdown
}

// run is the main loop for each shard goroutine.
func (s *cacheShard) run(wg *sync.WaitGroup) {
	defer wg.Done()
	for req := range s.requests {
		switch req.op {
		case opGetForScheduler:
			// Get or create session for scheduler
			state, exists := s.sessions[req.sessionID]
			if !exists {
				state = &SessionState{
					SessionID:             req.sessionID,
					LastActivityTimestamp: time.Now(),
				}
				s.sessions[req.sessionID] = state
			}
			req.schedulerInfoRespChan <- schedulerInfoResponse{
				cst:      state.CriticalPathServiceTime,
				waitTime: state.TotalWaitTime,
			}
		case opGetFullState:
			// Only return existing state, don't create
			state, exists := s.sessions[req.sessionID]
			if !exists {
				req.fullStateResponseChan <- fullStateResponse{state: nil}
				continue
			}
			stateCopy := *state
			req.fullStateResponseChan <- fullStateResponse{state: &stateCopy}
		case opUpdateState:
			// Get or create session for update
			state, exists := s.sessions[req.sessionID]
			if !exists {
				state = &SessionState{
					SessionID:             req.sessionID,
					LastActivityTimestamp: time.Now(),
				}
				s.sessions[req.sessionID] = state
			}
			payload := req.updatePayload
			state.TotalWaitTime += payload.waitTime
			newPathLength := payload.inheritedCST + payload.executionTime
			if newPathLength > state.CriticalPathServiceTime {
				state.CriticalPathServiceTime = newPathLength
			}
			state.LastActivityTimestamp = time.Now()
		case opUpdateAffinity:
			// Get or create session for affinity update
			state, exists := s.sessions[req.sessionID]
			if !exists {
				state = &SessionState{
					SessionID:             req.sessionID,
					LastActivityTimestamp: time.Now(),
				}
				s.sessions[req.sessionID] = state
			}
			state.PodAffinity = req.affinityPayload
			state.LastActivityTimestamp = time.Now()
		case opCleanup:
			payload := req.cleanupPayload
			now := time.Now()
			for sessionID, state := range s.sessions {
				if now.Sub(state.LastActivityTimestamp) > payload.timeout {
					delete(s.sessions, sessionID)
				}
			}
		}
	}
}

// ShardedSessionCache is a highly concurrent, channel-based session cache.
type ShardedSessionCache struct {
	shards []*cacheShard
	wg     sync.WaitGroup
}

// NewShardedSessionCache creates and starts all shard goroutines.
func NewShardedSessionCache() *ShardedSessionCache {
	sc := &ShardedSessionCache{
		shards: make([]*cacheShard, shardCount),
	}
	for i := 0; i < shardCount; i++ {
		shard := &cacheShard{
			sessions: make(map[string]*SessionState),
			requests: make(chan cacheRequest, 128), // Buffered channel per shard
			done:     make(chan struct{}),
		}
		sc.shards[i] = shard
		sc.wg.Add(1)
		go shard.run(&sc.wg)
	}
	return sc
}

// getShard returns the shard for a given sessionID.
func (sc *ShardedSessionCache) getShard(sessionID string) *cacheShard {
	hasher := hasherPool.Get().(hash.Hash64)
	defer hasherPool.Put(hasher)
	hasher.Reset()
	hasher.Write([]byte(sessionID))
	return sc.shards[hasher.Sum64()&uint64(shardCount-1)]
}

// --- Public API ---

// GetOrCreateForScheduler is the primary method for the scheduler
func (sc *ShardedSessionCache) GetOrCreateForScheduler(sessionID string) (time.Duration, time.Duration) {
	shard := sc.getShard(sessionID)
	respChan := make(chan schedulerInfoResponse, 1)
	shard.requests <- cacheRequest{
		op:                    opGetForScheduler,
		sessionID:             sessionID,
		schedulerInfoRespChan: respChan,
	}
	info := <-respChan
	return info.cst, info.waitTime
}

// UpdateState is the primary method for the executor
func (sc *ShardedSessionCache) UpdateState(sessionID string, inheritedCST, executionTime, waitTime time.Duration) {
	shard := sc.getShard(sessionID)
	shard.requests <- cacheRequest{
		op:        opUpdateState,
		sessionID: sessionID,
		updatePayload: updatePayload{
			inheritedCST:  inheritedCST,
			executionTime: executionTime,
			waitTime:      waitTime,
		},
	}
}

// UpdateAffinity is the primary method for the executor
func (sc *ShardedSessionCache) UpdateAffinity(sessionID, podName string) {
	shard := sc.getShard(sessionID)
	shard.requests <- cacheRequest{
		op:              opUpdateAffinity,
		sessionID:       sessionID,
		affinityPayload: podName,
	}
}

// GetState is provided for testing and debugging.
// Returns a copy of the session state to ensure thread safety.
func (sc *ShardedSessionCache) GetState(sessionID string) (SessionState, bool) {
	shard := sc.getShard(sessionID)
	respChan := make(chan fullStateResponse, 1)
	shard.requests <- cacheRequest{
		op:                    opGetFullState,
		sessionID:             sessionID,
		fullStateResponseChan: respChan,
	}
	info := <-respChan
	if info.state == nil {
		return SessionState{}, false
	}
	// Return a copy to match the interface signature
	return *info.state, true
}

// StartCleanupRoutine starts a background goroutine that periodically
// cleans up stale sessions across all shards.
// Returns a stop function that can be called to halt the cleanup routine.
func (sc *ShardedSessionCache) StartCleanupRoutine(interval, timeout time.Duration) (stop func()) {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Send cleanup request to all shards
				req := cacheRequest{
					op: opCleanup,
					cleanupPayload: cleanupPayload{
						timeout: timeout,
					},
				}
				for _, shard := range sc.shards {
					// Use select to avoid panic if channel is closed
					select {
					case shard.requests <- req:
					case <-done:
						// Stop signal received while sending
						return
					}
				}
			case <-done:
				return
			}
		}
	}()

	return func() {
		close(done)
	}
}

// Close shuts down all shard goroutines, not elegantly yet.
func (sc *ShardedSessionCache) Close() {
	for _, shard := range sc.shards {
		close(shard.requests)
	}
	sc.wg.Wait()
}
