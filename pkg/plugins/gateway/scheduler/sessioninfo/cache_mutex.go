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
	"sync"
	"time"
)

// // SessionState holds all the scheduling-relevant information for a single session
type SessionState struct {
	SessionID               string        // The session ID
	CriticalPathServiceTime time.Duration // The critical path service time
	TotalWaitTime           time.Duration // The total wait time (anti-starvation)
	PodAffinity             string        // The pod affinity (later may needed)
	LastActivityTimestamp   time.Time     // The last activity timestamp
}

// MutexSessionCache is a thread-safe, in-memory store for session states
// using a sync.RWMutex.
type MutexSessionCache struct {
	mu       sync.RWMutex             // Protects the sessions map
	sessions map[string]*SessionState // sessionID -> *SessionState
}

// NewMutexSessionCache creates a new in-memory session cache protected by a mutex.
func NewMutexSessionCache() *MutexSessionCache {
	return &MutexSessionCache{
		sessions: make(map[string]*SessionState),
	}
}

// getState is a private helper that assumes a write lock is already held.
// It ensures a session state exists before any operation.
func (sc *MutexSessionCache) getState(sessionID string) *SessionState {
	state, exists := sc.sessions[sessionID]
	if !exists {
		state = &SessionState{
			SessionID:             sessionID,
			LastActivityTimestamp: time.Now(),
		}
		sc.sessions[sessionID] = state
	}
	return state
}

// GetState retrieves a copy of the state for a given sessionID
// for read-only purposes.
// It returns false if the session does not exist.
func (sc *MutexSessionCache) GetState(sessionID string) (SessionState, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	state, exists := sc.sessions[sessionID]
	if !exists {
		return SessionState{}, false
	}

	// Return a copy to ensure
	// the caller cannot modify the internal state without a lock,
	// which would cause a data race.
	return *state, true
}

// GetOrCreateForScheduler is the primary method for the scheduler
// to get the necessary info.
// It returns the inherited CST and total wait time for a new job.
func (sc *MutexSessionCache) GetOrCreateForScheduler(sessionID string) (
	time.Duration, time.Duration) {
	sc.mu.Lock() // Use a write lock because we might create a session.
	defer sc.mu.Unlock()

	state := sc.getState(sessionID)
	return state.CriticalPathServiceTime, state.TotalWaitTime
}

// UpdateState atomically updates the session state after a request is finished.
func (sc *MutexSessionCache) UpdateState(sessionID string, inheritedCST,
	executionTime, waitTime time.Duration) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	state := sc.getState(sessionID)

	// Atomically update total wait time
	state.TotalWaitTime += waitTime

	// Atomically update CriticalPathServiceTime (ATLAS logic)
	newPathLength := inheritedCST + executionTime
	if newPathLength > state.CriticalPathServiceTime {
		state.CriticalPathServiceTime = newPathLength
	}

	state.LastActivityTimestamp = time.Now()
}

// UpdateAffinity updates the pod affinity for a session.
func (sc *MutexSessionCache) UpdateAffinity(sessionID, podName string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	state := sc.getState(sessionID)
	state.PodAffinity = podName
}

// StartCleanupRoutine starts a background goroutine that periodically
// cleans up stale sessions.
// It returns a function that can be called to stop the routine.
func (sc *MutexSessionCache) StartCleanupRoutine(interval,
	timeout time.Duration) (stop func()) {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				sc.cleanup(timeout)
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	return func() {
		close(done)
	}
}

// cleanup removes sessions that have been inactive for longer than the timeout.
// This is a private method that assumes the caller handles locking.
func (sc *MutexSessionCache) cleanup(timeout time.Duration) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now()
	for sessionID, state := range sc.sessions {
		if now.Sub(state.LastActivityTimestamp) > timeout {
			delete(sc.sessions, sessionID)
		}
	}
}
