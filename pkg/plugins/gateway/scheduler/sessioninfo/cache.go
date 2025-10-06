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

import "time"

// SessionCache is the interface for session state management.
// It provides thread-safe operations for tracking session states
// across multiple concurrent requests.
//
// Implementations:
//   - MutexSessionCache: Simple mutex-based implementation for low to medium concurrency
//   - ShardedSessionCache: High-performance sharded implementation for high concurrency
type SessionCache interface {
	// GetOrCreateForScheduler returns the inherited CST and total wait time
	// for a new job in the given session. If the session doesn't exist,
	// it will be created with zero values.
	//
	// This is the primary method used by the scheduler to get scheduling
	// information for a new request.
	//
	// Returns:
	//   - cst: The critical path service time of the session
	//   - waitTime: The total accumulated wait time of the session
	GetOrCreateForScheduler(sessionID string) (cst, waitTime time.Duration)

	// UpdateState atomically updates the session state after a request completes.
	//
	// The update logic follows the ATLAS algorithm:
	//   - TotalWaitTime is accumulated: totalWaitTime += waitTime
	//   - CriticalPathServiceTime is updated to max(current, inheritedCST + executionTime)
	//
	// Parameters:
	//   - sessionID: The session identifier
	//   - inheritedCST: The CST value inherited when the request started
	//   - executionTime: The actual execution time of this request
	//   - waitTime: The time this request spent waiting in the queue
	UpdateState(sessionID string, inheritedCST, executionTime, waitTime time.Duration)

	// UpdateAffinity updates the pod affinity hint for a session.
	// This can be used to optimize cache hits by routing subsequent
	// requests from the same session to the same pod.
	//
	// Parameters:
	//   - sessionID: The session identifier
	//   - podName: The name of the pod to set as affinity hint
	UpdateAffinity(sessionID, podName string)

	// GetState retrieves a copy of the full session state.
	// This method is primarily used for testing and debugging.
	//
	// Returns:
	//   - state: A copy of the session state
	//   - exists: false if the session doesn't exist
	GetState(sessionID string) (state SessionState, exists bool)

	// StartCleanupRoutine starts a background goroutine that periodically
	// cleans up stale sessions that have been inactive for longer than timeout.
	//
	// The cleanup runs at the specified interval. Calling this method multiple
	// times will start multiple cleanup routines (caller should avoid this).
	//
	// Parameters:
	//   - interval: How often to run the cleanup
	//   - timeout: Sessions inactive for longer than this will be removed
	//
	// Returns:
	//   - stop: A function that stops the cleanup routine when called
	StartCleanupRoutine(interval, timeout time.Duration) (stop func())
}

