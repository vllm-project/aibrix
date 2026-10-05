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
	"container/list"
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Sliding window configuration
const (
	defaultTokenTrackerWindowSize = 5      // Default window size in the configured time units, example: 5 minutes
	defaultTokenTrackerMinTokens  = 1000.0 // Sensible min default value for adaptive token tracking(see vtc_basic)
	defaultTokenTrackerMaxTokens  = 8000.0 // Sensible max default value for adaptive token tracking(see vtc_basic)
	defaultTimeUnit               = "minutes"
)

const (
	VTC_TOKEN_TRACKER_WINDOW_SIZE = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE"
	VTC_TOKEN_TRACKER_TIME_UNIT   = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT"
	VTC_TOKEN_TRACKER_MIN_TOKENS  = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS"
	VTC_TOKEN_TRACKER_MAX_TOKENS  = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS"
)

var (
	tokenTrackerWindowSize = utils.LoadEnvInt(VTC_TOKEN_TRACKER_WINDOW_SIZE, defaultTokenTrackerWindowSize)
	tokenTrackerMinTokens  = utils.LoadEnvFloat(VTC_TOKEN_TRACKER_MIN_TOKENS, defaultTokenTrackerMinTokens)
	tokenTrackerMaxTokens  = utils.LoadEnvFloat(VTC_TOKEN_TRACKER_MAX_TOKENS, defaultTokenTrackerMaxTokens)
	timeUnitStr            = utils.LoadEnv(VTC_TOKEN_TRACKER_TIME_UNIT, defaultTimeUnit)
)

type TimeUnit int

const (
	Minutes TimeUnit = iota
	Seconds
	Milliseconds
)

var timeUnitDuration = map[TimeUnit]time.Duration{
	Minutes:      time.Minute,
	Seconds:      time.Second,
	Milliseconds: time.Millisecond,
}

func (unit TimeUnit) toTimestamp(t time.Time) int64 {
	if unit == Milliseconds {
		return t.UnixNano() / int64(time.Millisecond)
	}
	return t.Unix()
}

// bucketNode stores the data for a single time bucket in the linked list.
type bucketNode struct {
	timestamp  int64
	tokenCount float64
}

// userBucketData holds the token tracking data structures for a single user.
type userBucketData struct {
	buckets *list.List              // Doubly linked list of *bucketNode, ordered by timestamp
	lookup  map[int64]*list.Element // Maps timestamp to list element for O(1) access
}

// InMemorySlidingWindowTokenTracker tracks tokens per user in a fixed-size sliding window (in-memory, thread-safe).
type InMemorySlidingWindowTokenTracker struct {
	mu         sync.RWMutex
	windowSize time.Duration
	// windowBuckets is the window size in units of bucketUnit, kept so a time
	// unit change can recompute windowSize.
	windowBuckets   int
	bucketUnit      TimeUnit
	userBucketStore map[string]*userBucketData // Stores bucket list and lookup map per user
	userTotals      map[string]float64
	// Efficient Min/Max Tracking
	totalsToUsers   map[float64]map[string]struct{} // total -> set of users
	minTrackedToken float64
	maxTrackedToken float64
	config          *VTCConfig
	// minTokens and maxTokens are the floors reported while the window holds
	// too little activity to derive them from. They are per tracker rather
	// than process-wide because a model config profile may lower or raise them
	// for the tracker it scoped to itself.
	minTokens float64
	maxTokens float64
}

// TokenTrackerOption is a function that configures a token tracker
type TokenTrackerOption func(*InMemorySlidingWindowTokenTracker)

// windowSizeDuration converts a bucket count and a time unit into a window
// duration. An extremely large configuration saturates at the maximum
// duration instead of wrapping the multiplication around into a negative
// window.
func windowSizeDuration(buckets int, unit TimeUnit) time.Duration {
	d := timeUnitDuration[unit]
	if buckets > 0 && d > 0 && time.Duration(buckets) > time.Duration(math.MaxInt64)/d {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(buckets) * d
}

// updateWindowSize recalculates the window size based on time unit
func (t *InMemorySlidingWindowTokenTracker) updateWindowSize() {
	// Set window size based on configured size and time unit
	t.windowSize = windowSizeDuration(t.windowBuckets, t.bucketUnit)
}

func WithWindowSize(size int) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		// Override this tracker's window size with the provided value. The
		// process-wide default is left alone: it belongs to the trackers the
		// environment configured, not to this one.
		t.windowBuckets = size
		t.updateWindowSize()
	}
}

func WithTimeUnit(unit TimeUnit) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		t.bucketUnit = unit
		t.updateWindowSize()
	}
}

// TimeUnitName canonicalizes a time unit name the environment or a model
// config profile configures, so that callers carry the name of a unit the
// tracker has instead of an arbitrary string. An unknown name falls back to
// minutes, which is what the environment loader has always done.
func TimeUnitName(name string) string {
	switch name {
	case "seconds":
		return "seconds"
	case "milliseconds":
		return "milliseconds"
	default:
		return defaultTimeUnit
	}
}

// timeUnitFromName maps a canonical time unit name to its TimeUnit.
func timeUnitFromName(name string) TimeUnit {
	switch TimeUnitName(name) {
	case "seconds":
		return Seconds
	case "milliseconds":
		return Milliseconds
	default:
		return Minutes
	}
}

// EnvTokenTrackerKnobs returns the window, time unit and token floors the
// environment configures. The routing algorithm package folds them into the
// process default table at startup, and a request whose model config profile
// sets none of them shares the tracker built from them.
func EnvTokenTrackerKnobs() types.VTCTokenTrackerOverrides {
	return types.VTCTokenTrackerOverrides{
		WindowSize: tokenTrackerWindowSize,
		TimeUnit:   TimeUnitName(timeUnitStr),
		MinTokens:  tokenTrackerMinTokens,
		MaxTokens:  tokenTrackerMaxTokens,
	}
}

// TODO: add redis token tracker so that state is shared across plugin instances
// NewInMemorySlidingWindowTokenTracker creates a new token tracker with configurable options
func NewInMemorySlidingWindowTokenTracker(config *VTCConfig, opts ...TokenTrackerOption) TokenTracker {
	return NewScopedInMemorySlidingWindowTokenTracker(config, EnvTokenTrackerKnobs(), opts...)
}

// NewScopedInMemorySlidingWindowTokenTracker creates a tracker whose window,
// bucket size and token floors come from knobs instead of the process-wide
// environment variables. It backs the tracker a model config profile scopes to
// itself, so the profile's window, weights and floors are read from and
// written to that tracker alone.
func NewScopedInMemorySlidingWindowTokenTracker(config *VTCConfig, knobs types.VTCTokenTrackerOverrides, opts ...TokenTrackerOption) TokenTracker {
	unit := timeUnitFromName(knobs.TimeUnit)

	tracker := &InMemorySlidingWindowTokenTracker{
		bucketUnit:      unit,
		windowBuckets:   knobs.WindowSize,
		windowSize:      windowSizeDuration(knobs.WindowSize, unit),
		userBucketStore: make(map[string]*userBucketData),
		userTotals:      make(map[string]float64),
		totalsToUsers:   make(map[float64]map[string]struct{}),
		minTrackedToken: math.MaxFloat64, // Start high so first positive value becomes min
		maxTrackedToken: 0.0,             // Start with zero as default max
		minTokens:       knobs.MinTokens,
		maxTokens:       knobs.MaxTokens,
		config:          config,
	}

	for _, opt := range opts {
		opt(tracker)
	}

	return tracker
}

func (t *InMemorySlidingWindowTokenTracker) getCutoffTimestamp() int64 {
	cutoffTime := time.Now().Add(-t.windowSize)
	return t.bucketUnit.toTimestamp(cutoffTime)
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) pruneExpiredBucketsAndUpdateState(user string, cutoff int64) {
	_, ok := t.userBucketStore[user]
	if !ok {
		return // User doesn't exist or has no buckets
	}

	bucketsList := t.userBucketStore[user].buckets
	lookupMap := t.userBucketStore[user].lookup
	modified := false
	oldTotal := t.userTotals[user]
	newTotal := oldTotal

	// Iterate from the front (oldest) of the list
	for el := bucketsList.Front(); el != nil; {
		node := el.Value.(*bucketNode)
		if node.timestamp < cutoff {
			// Remove expired bucket
			newTotal -= node.tokenCount
			delete(lookupMap, node.timestamp)
			next := el.Next()
			bucketsList.Remove(el)
			el = next
			modified = true
		} else {
			// Stop as soon as we find a non-expired bucket (list is ordered)
			break
		}
	}

	if modified {
		// Update user total and min/max tracking with correct old and new values
		t.updateUserTotalAndMinMax(user, oldTotal, newTotal)
	}
}

// Time: Avg O(1) (amortized), Worst O(B_u) where B_u = buckets for user u | Space: O(1)
func (t *InMemorySlidingWindowTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	t.mu.RLock()

	if user == "" {
		t.mu.RUnlock()
		return 0, nil
	}

	_, ok := t.userBucketStore[user]
	if !ok || t.userBucketStore[user].buckets.Len() == 0 {
		t.mu.RUnlock()
		// Return cached total if user exists but has no buckets currently, else 0
		if ok {
			return t.userTotals[user], nil
		}
		return 0, nil
	}

	total := t.userTotals[user]

	cutoff := t.getCutoffTimestamp()
	needsPruning := false

	oldestElement := t.userBucketStore[user].buckets.Front()
	if oldestElement != nil {
		oldestNode := oldestElement.Value.(*bucketNode)
		if oldestNode.timestamp < cutoff {
			needsPruning = true
		}
	}

	t.mu.RUnlock()

	// Only acquire write lock if we need to prune
	if needsPruning {
		t.mu.Lock()
		// Re-check user data existence in case it was deleted between RUnlock and Lock
		_, ok = t.userBucketStore[user]
		if ok {
			t.pruneExpiredBucketsAndUpdateState(user, cutoff)
			// Get the potentially updated total after pruning
			total = t.userTotals[user]
		}
		t.mu.Unlock()
	}

	return total, nil
}

// Time: O(1) | Space: O(1)
func (t *InMemorySlidingWindowTokenTracker) GetMinTokenCount(ctx context.Context) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Return the configured floor if no active users (minTrackedToken is still
	// at its initialization value)
	if t.minTrackedToken == math.MaxFloat64 {
		return t.minTokens, nil
	}
	return t.minTrackedToken, nil
}

// Time: O(1) | Space: O(1)
func (t *InMemorySlidingWindowTokenTracker) GetMaxTokenCount(ctx context.Context) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// If no active users or all have zero tokens, return the configured floor
	if t.maxTrackedToken == 0 {
		return t.maxTokens, nil
	}
	return t.maxTrackedToken, nil
}

// Time: Avg O(1) (amortized), Worst O(B_u) where B_u = buckets for user u | Space: O(1)
func (t *InMemorySlidingWindowTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	if user == "" {
		return fmt.Errorf("user ID cannot be empty")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	currentTimestamp := t.bucketUnit.toTimestamp(now)
	cutoff := t.getCutoffTimestamp()

	_, ok := t.userBucketStore[user]
	if !ok {
		t.userBucketStore[user] = &userBucketData{
			buckets: list.New(),
			lookup:  make(map[int64]*list.Element),
		}
	}

	oldTotal := t.userTotals[user]

	// Prune first before adding/updating to maintain window size constraint accurately
	t.pruneExpiredBucketsAndUpdateState(user, cutoff)

	totalAfterPruning := t.userTotals[user]

	// Clamp negative tokens to zero
	inputTokens = max(0, inputTokens)
	outputTokens = max(0, outputTokens)

	newTokens := inputTokens*t.config.InputTokenWeight + outputTokens*t.config.OutputTokenWeight

	// Check if a bucket for the current timestamp already exists
	if element, exists := t.userBucketStore[user].lookup[currentTimestamp]; exists {
		node := element.Value.(*bucketNode)
		node.tokenCount += newTokens
	} else {
		node := &bucketNode{timestamp: currentTimestamp, tokenCount: newTokens}
		element := t.userBucketStore[user].buckets.PushBack(node)
		t.userBucketStore[user].lookup[currentTimestamp] = element
	}

	// Calculate the final new total
	newTotal := totalAfterPruning + newTokens

	// Update user total and global min/max efficiently
	t.updateUserTotalAndMinMax(user, oldTotal, newTotal)

	return nil
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) updateUserTotalAndMinMax(user string, oldTotal, newTotal float64) {
	if oldTotal == newTotal {
		return
	}

	// Negative totals should be treated as 0
	newTotal = max(0, newTotal)

	t.userTotals[user] = newTotal
	t.removeFromTotals(oldTotal, user)
	t.addToTotals(newTotal, user)
	t.recalcMin(oldTotal, newTotal)
	t.recalcMax(oldTotal, newTotal)
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) removeFromTotals(oldTotal float64, user string) {
	if oldTotal <= 0 {
		return
	}
	if users, ok := t.totalsToUsers[oldTotal]; ok {
		delete(users, user)
		if len(users) == 0 {
			delete(t.totalsToUsers, oldTotal)
		}
	}
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) addToTotals(newTotal float64, user string) {
	if newTotal <= 0 {
		return
	}
	if _, ok := t.totalsToUsers[newTotal]; !ok {
		t.totalsToUsers[newTotal] = make(map[string]struct{})
	}
	t.totalsToUsers[newTotal][user] = struct{}{}
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) wasLastUserAtBoundary(value float64, userTotal float64) bool {
	// Returns true if userTotal matches the boundary value and no users remain at userTotal.
	return userTotal > 0 && userTotal == value && len(t.totalsToUsers[userTotal]) == 0
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) recalcMin(oldTotal, newTotal float64) {
	if t.wasLastUserAtBoundary(t.minTrackedToken, oldTotal) {
		// Find new minimum.
		newMin := math.MaxFloat64
		for total := range t.totalsToUsers {
			// keys in totalsToUsers are guaranteed > 0 by addToTotals
			if total < newMin {
				newMin = total
			}
		}
		t.minTrackedToken = newMin
	} else if newTotal > 0 && newTotal < t.minTrackedToken {
		t.minTrackedToken = newTotal
	}

	// Ensure minTrackedToken is MaxFloat64 if no users have positive totals
	if len(t.totalsToUsers) == 0 {
		t.minTrackedToken = math.MaxFloat64
	}
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) recalcMax(oldTotal, newTotal float64) {
	if t.wasLastUserAtBoundary(t.maxTrackedToken, oldTotal) {
		// Find new maximum
		t.maxTrackedToken = 0
		for total := range t.totalsToUsers {
			if total > t.maxTrackedToken {
				t.maxTrackedToken = total
			}
		}
	} else if newTotal > t.maxTrackedToken {
		t.maxTrackedToken = newTotal
	}
}
