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

package error_injection

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// TraceStore defines the interface for storing and retrieving execution traces
type TraceStore interface {
	// AppendPoint adds a point record to the trace for the given job ID
	AppendPoint(jobID string, point PointRecord) error

	// Get retrieves the execution trace for the given job ID
	Get(jobID string) (*ExecutionTrace, error)

	// Delete removes the execution trace for the given job ID
	Delete(jobID string) error

	// List returns the most recent execution traces, limited to the specified count
	List(limit int) ([]*ExecutionTrace, error)
}

// Common errors for trace store operations
var (
	ErrTraceNotFound = errors.New("trace not found")
)

// InMemoryTraceStore implements TraceStore with an in-memory map
// This is for debugging only and does not persist to any database
type InMemoryTraceStore struct {
	traces    map[string]*ExecutionTrace
	mu        sync.RWMutex
	maxTraces int // Optional limit for LRU eviction
}

// NewInMemoryTraceStore creates a new in-memory trace store
func NewInMemoryTraceStore() *InMemoryTraceStore {
	return &InMemoryTraceStore{
		traces:    make(map[string]*ExecutionTrace),
		maxTraces: 0, // 0 means no limit
	}
}

// NewInMemoryTraceStoreWithLimit creates a new in-memory trace store with a maximum trace limit
// When the limit is exceeded, the oldest traces are evicted (LRU)
func NewInMemoryTraceStoreWithLimit(maxTraces int) *InMemoryTraceStore {
	return &InMemoryTraceStore{
		traces:    make(map[string]*ExecutionTrace),
		maxTraces: maxTraces,
	}
}

// AppendPoint adds a point record to the trace for the given job ID
// Thread-safe operation that creates a new trace if one doesn't exist
func (s *InMemoryTraceStore) AppendPoint(jobID string, point PointRecord) error {
	if jobID == "" {
		return errors.New("jobID is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	trace, exists := s.traces[jobID]
	if !exists {
		// Create new trace
		trace = &ExecutionTrace{
			JobID:     jobID,
			StartTime: point.Timestamp,
			Points:    []PointRecord{},
		}
		s.traces[jobID] = trace

		// Check if we need to evict old traces (LRU)
		if s.maxTraces > 0 && len(s.traces) > s.maxTraces {
			s.evictOldest()
		}
	}

	// Append the point
	trace.Points = append(trace.Points, point)

	// Update end time to the latest point's timestamp
	trace.EndTime = point.Timestamp

	// Ensure start time is the earliest point
	if point.Timestamp.Before(trace.StartTime) {
		trace.StartTime = point.Timestamp
	}

	return nil
}

// Get retrieves the execution trace for the given job ID
// Returns ErrTraceNotFound if the trace doesn't exist
func (s *InMemoryTraceStore) Get(jobID string) (*ExecutionTrace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	trace, exists := s.traces[jobID]
	if !exists {
		return nil, ErrTraceNotFound
	}

	return trace.Clone(), nil
}

// Delete removes the execution trace for the given job ID
// No error is returned if the trace doesn't exist
func (s *InMemoryTraceStore) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.traces, jobID)
	return nil
}

// List returns the most recent execution traces, sorted by StartTime (newest first)
// Limited to the specified count
func (s *InMemoryTraceStore) List(limit int) ([]*ExecutionTrace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect all traces
	traces := make([]*ExecutionTrace, 0, len(s.traces))
	for _, trace := range s.traces {
		traces = append(traces, trace.Clone())
	}

	// Sort by StartTime descending (newest first)
	sort.Slice(traces, func(i, j int) bool {
		return traces[i].StartTime.After(traces[j].StartTime)
	})

	// Apply limit
	if limit > 0 && len(traces) > limit {
		traces = traces[:limit]
	}

	return traces, nil
}

// evictOldest removes the oldest trace(s) when the limit is exceeded
// Must be called with lock already held
func (s *InMemoryTraceStore) evictOldest() {
	if len(s.traces) <= s.maxTraces || s.maxTraces == 0 {
		return
	}

	// Find the oldest trace by StartTime
	var oldestJobID string
	var oldestTime time.Time
	first := true

	for jobID, trace := range s.traces {
		if first || trace.StartTime.Before(oldestTime) {
			oldestTime = trace.StartTime
			oldestJobID = jobID
			first = false
		}
	}

	// Delete the oldest trace
	if oldestJobID != "" {
		delete(s.traces, oldestJobID)
	}
}

// Count returns the number of traces currently stored
func (s *InMemoryTraceStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.traces)
}

// Clear removes all traces from the store
func (s *InMemoryTraceStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces = make(map[string]*ExecutionTrace)
}
