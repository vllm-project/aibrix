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
	"hash/fnv"
	"time"
)

// SessionState holds all the scheduling-relevant information for a single session
type SessionState struct {
	SessionID               string        // The session ID
	CriticalPathServiceTime time.Duration // The critical path service time
	TotalWaitTime           time.Duration // The total wait time (anti-starvation)
	PodAffinity             string        // The pod affinity (later may needed)
	LastActivityTimestamp   time.Time     // The last activity timestamp
}

// --- Internal channel communication structs ---
type cacheOp int

const (
	opGetForScheduler cacheOp = iota
	opUpdateState
	opUpdateAffinity
)

type cacheRequest struct {
	op        cacheOp
	sessionID string
	payload   any
	respChan  chan any
}

type updatePayload struct {
	inheritedCST  time.Duration
	executionTime time.Duration
	waitTime      time.Duration
}

type schedulerInfoResponse struct {
	cst      time.Duration
	waitTime time.Duration
}

// --- Shard and ShardedCache implementation ---
const shardCount = 256 // Must be a power of 2 for bitwise AND optimization

type cacheShard struct {
	sessions map[string]*SessionState // sessionID -> *SessionState
	requests chan cacheRequest        // Channel for incoming requests
	done     chan struct{}            // Channel for shutdown
}

func (s *cacheShard) run() {
	for {
		select {
		case req := <-s.requests:
			state, exists := s.sessions[req.sessionID]
			if !exists {
				state = &SessionState{LastActivityTimestamp: time.Now()}
				s.sessions[req.sessionID] = state
			}

			switch req.op {
			case opGetForScheduler:
				// Return a struct with the required values.
				req.respChan <- schedulerInfoResponse{
					cst:      state.CriticalPathServiceTime,
					waitTime: state.TotalWaitTime,
				}
			case opUpdateState:
				payload := req.payload.(updatePayload)
				state.TotalWaitTime += payload.waitTime
				newPathLength := payload.inheritedCST + payload.executionTime
				if newPathLength > state.CriticalPathServiceTime {
					state.CriticalPathServiceTime = newPathLength
				}
				state.LastActivityTimestamp = time.Now()
			case opUpdateAffinity:
				state.PodAffinity = req.payload.(string)
			}
		case <-s.done:
			// close(s.requests)
			return
		}
	}
}

// ShardedSessionCache is a highly concurrent, channel-based session cache.
type ShardedSessionCache struct {
	shards []*cacheShard
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
		go shard.run()
	}
	return sc
}

func (sc *ShardedSessionCache) getShard(sessionID string) *cacheShard {
	hasher := fnv.New64a()
	hasher.Write([]byte(sessionID))
	return sc.shards[hasher.Sum64()&uint64(shardCount-1)]
}

// --- Public API ---

func (sc *ShardedSessionCache) GetOrCreateForScheduler(sessionID string) (time.Duration, time.Duration) {
	shard := sc.getShard(sessionID)
	respChan := make(chan any, 1)
	shard.requests <- cacheRequest{
		op:        opGetForScheduler,
		sessionID: sessionID,
		respChan:  respChan,
	}
	response := <-respChan
	info := response.(schedulerInfoResponse)
	return info.cst, info.waitTime
}

func (sc *ShardedSessionCache) UpdateState(sessionID string, inheritedCST, executionTime, waitTime time.Duration) {
	shard := sc.getShard(sessionID)
	shard.requests <- cacheRequest{
		op:        opUpdateState,
		sessionID: sessionID,
		payload: updatePayload{
			inheritedCST:  inheritedCST,
			executionTime: executionTime,
			waitTime:      waitTime,
		},
	}
}

func (sc *ShardedSessionCache) UpdateAffinity(sessionID, podName string) {
	shard := sc.getShard(sessionID)
	shard.requests <- cacheRequest{
		op:        opUpdateAffinity,
		sessionID: sessionID,
		payload:   podName,
	}
}

// GetState is provided for testing and debugging.
func (sc *ShardedSessionCache) GetState(sessionID string) SessionState {
	cst, waitTime := sc.GetOrCreateForScheduler(sessionID)
	// Note: This is a simplified GetState, it doesn't return affinity.
	// A proper GetState would need its own op code.
	return SessionState{CriticalPathServiceTime: cst, TotalWaitTime: waitTime}
}

// Close shuts down all shard goroutines, not elegantly yet.
// TODO(von):
//   - this is a global shutdown that has to be called from upper layers.
//   - we can consider a shard-level shutdown for better resource management.
//   - If hot upgrades and dynamic reloading are supported,
//     Close() should return an error/block until all goroutines have indeed exited.
//
// ...
func (sc *ShardedSessionCache) Close() {
	for _, shard := range sc.shards {
		close(shard.done)
	}
}
