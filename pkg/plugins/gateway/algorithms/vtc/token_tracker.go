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

package vtc

import (
	"context"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// Sliding window configuration (minutes)
const (
	VTC_TOKEN_TRACKER_WINDOW_MINUTES = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_MINUTES"
	defaultTokenTrackerWindowMinutes = 60
)

var (
	tokenTrackerWindowMinutes = utils.LoadEnvInt(VTC_TOKEN_TRACKER_WINDOW_MINUTES, defaultTokenTrackerWindowMinutes)
)

type TimeUnit int

const (
	Minutes TimeUnit = iota
	Seconds
	Milliseconds
)

// Time duration mapping for each time unit
var timeUnitDuration = map[TimeUnit]time.Duration{
	Minutes:      time.Minute,
	Seconds:      time.Second,
	Milliseconds: time.Millisecond,
}

// Helper functions for time unit operations
func (unit TimeUnit) truncateTime(t time.Time) time.Time {
	return t.Truncate(timeUnitDuration[unit])
}

func (unit TimeUnit) toTimestamp(t time.Time) int64 {
	if unit == Milliseconds {
		return t.UnixNano() / int64(time.Millisecond)
	}
	return t.Unix()
}

// InMemorySlidingWindowTokenTracker tracks tokens per user in a fixed-size sliding window (in-memory, thread-safe).
type InMemorySlidingWindowTokenTracker struct {
	mu          sync.RWMutex
	windowSize  time.Duration
	buckets     int
	bucketUnit  TimeUnit
	userBuckets map[string]map[int64]float64 // user -> windowStart -> tokenSum
	config      *VTCConfig
}

// TokenTrackerOption is a function that configures a token tracker
type TokenTrackerOption func(*InMemorySlidingWindowTokenTracker)

func WithWindowSize(size int) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		t.buckets = size
		t.windowSize = time.Duration(size) * timeUnitDuration[t.bucketUnit]
	}
}

func WithTimeUnit(unit TimeUnit) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		t.bucketUnit = unit
		t.windowSize = time.Duration(t.buckets) * timeUnitDuration[unit]
	}
}

// TODO: add redis token tracker so that state is shared across plugin instances
// NewInMemorySlidingWindowTokenTracker creates a new token tracker with configurable options
func NewInMemorySlidingWindowTokenTracker(config *VTCConfig, opts ...TokenTrackerOption) *InMemorySlidingWindowTokenTracker {
	// Default values
	size := tokenTrackerWindowMinutes
	if size < 1 {
		size = defaultTokenTrackerWindowMinutes
	}

	tracker := &InMemorySlidingWindowTokenTracker{
		windowSize:  time.Duration(size) * timeUnitDuration[Minutes],
		buckets:     size,
		bucketUnit:  Minutes,
		userBuckets: make(map[string]map[int64]float64),
		config:      config,
	}

	for _, opt := range opts {
		opt(tracker)
	}

	return tracker
}

func (t *InMemorySlidingWindowTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if user == "" {
		return 0, nil
	}
	buckets, ok := t.userBuckets[user]
	if !ok {
		return 0, nil
	}

	cutoffTime := time.Now().Add(-t.windowSize)
	cutoff := t.bucketUnit.toTimestamp(cutoffTime)

	sum := float64(0)
	for ts, val := range buckets {
		if ts >= cutoff {
			sum += val
		}
	}

	// Prune old buckets
	for ts := range buckets {
		if ts < cutoff {
			delete(buckets, ts)
		}
	}
	return sum, nil
}

func (t *InMemorySlidingWindowTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if user == "" {
		return nil
	}
	now := time.Now()

	truncatedTime := t.bucketUnit.truncateTime(now)
	windowStart := t.bucketUnit.toTimestamp(truncatedTime)

	tokens := inputTokens*t.config.InputTokenWeight + outputTokens*t.config.OutputTokenWeight
	buckets, ok := t.userBuckets[user]
	if !ok {
		buckets = make(map[int64]float64)
		t.userBuckets[user] = buckets
	}
	buckets[windowStart] += tokens

	cutoffTime := now.Add(-t.windowSize)
	cutoff := t.bucketUnit.toTimestamp(cutoffTime)

	// Prune old buckets
	for ts := range buckets {
		if ts < cutoff {
			delete(buckets, ts)
		}
	}
	return nil
}
