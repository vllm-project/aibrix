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

	"github.com/vllm-project/aibrix/pkg/utils"
)

// Sliding window configuration
const (
	defaultTokenTrackerWindowSize = 5         // Default window size in the configured time units
	defaultTokenTrackerMinTokens  = 1000.0    // Based on benchmark showing 500-1000 bucket sizes had best monotonicity
	defaultTokenTrackerMaxTokens  = 8000.0    // Based on benchmark showing 8000-16000 bucket sizes had worse monotonicity
	defaultTimeUnit               = "minutes" // Default time unit (minutes, seconds, milliseconds)
)

const (
	VTC_TOKEN_TRACKER_WINDOW_SIZE = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE"
	VTC_TOKEN_TRACKER_TIME_UNIT   = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT"
	VTC_TOKEN_TRACKER_MIN_TOKENS  = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS"
	VTC_TOKEN_TRACKER_MAX_TOKENS  = "AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS"
)

var (
	tokenTrackerWindowSize = utils.LoadEnvInt(VTC_TOKEN_TRACKER_WINDOW_SIZE, defaultTokenTrackerWindowSize)
	// tokenTrackerMinTokens is the configured minimum token count for the token tracker
	tokenTrackerMinTokens = utils.LoadEnvFloat(VTC_TOKEN_TRACKER_MIN_TOKENS, defaultTokenTrackerMinTokens)
	// tokenTrackerMaxTokens is the configured maximum token count for the token tracker
	tokenTrackerMaxTokens = utils.LoadEnvFloat(VTC_TOKEN_TRACKER_MAX_TOKENS, defaultTokenTrackerMaxTokens)
	timeUnitStr           = utils.LoadEnv(VTC_TOKEN_TRACKER_TIME_UNIT, defaultTimeUnit)
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
	mu              sync.RWMutex
	windowSize      time.Duration
	bucketUnit      TimeUnit
	userBucketStore map[string]*userBucketData // Stores bucket list and lookup map per user
	userTotals      map[string]float64
	// Efficient Min/Max Tracking
	totalsToUsers   map[float64]map[string]struct{} // total -> set of users
	minTrackedToken float64
	maxTrackedToken float64
	config          *VTCConfig
}

// TokenTrackerOption is a function that configures a token tracker
type TokenTrackerOption func(*InMemorySlidingWindowTokenTracker)

// updateWindowSize recalculates the window size based on time unit
func (t *InMemorySlidingWindowTokenTracker) updateWindowSize() {
	// Set window size based on configured size and time unit
	t.windowSize = time.Duration(tokenTrackerWindowSize) * timeUnitDuration[t.bucketUnit]
}

func WithWindowSize(size int) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		// Override the default window size with the provided value
		tokenTrackerWindowSize = size
		t.updateWindowSize()
	}
}

func WithTimeUnit(unit TimeUnit) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		t.bucketUnit = unit
		t.updateWindowSize()
	}
}

// TODO: add redis token tracker so that state is shared across plugin instances
// NewInMemorySlidingWindowTokenTracker creates a new token tracker with configurable options
func NewInMemorySlidingWindowTokenTracker(config *VTCConfig, opts ...TokenTrackerOption) TokenTracker {
	// Set default time unit from environment variable
	defaultUnit := Minutes // Default to minutes
	switch timeUnitStr {
	case "seconds":
		defaultUnit = Seconds
	case "milliseconds":
		defaultUnit = Milliseconds
	}

	tracker := &InMemorySlidingWindowTokenTracker{
		bucketUnit:      defaultUnit, // Use environment-configured default
		userBucketStore: make(map[string]*userBucketData),
		userTotals:      make(map[string]float64),
		totalsToUsers:   make(map[float64]map[string]struct{}),
		minTrackedToken: math.MaxFloat64, // Start high so first positive value becomes min
		maxTrackedToken: 0.0,             // Start with zero as default max
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
			next := el.Next() // Get next before removing current
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

func (t *InMemorySlidingWindowTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	t.mu.RLock()

	if user == "" {
		t.mu.RUnlock()
		return 0, nil
	}

	// Check if user exists and has data
	_, ok := t.userBucketStore[user]
	if !ok || t.userBucketStore[user].buckets.Len() == 0 {
		t.mu.RUnlock()
		// Return cached total if user exists but has no buckets currently, else 0
		if ok {
			return t.userTotals[user], nil
		}
		return 0, nil
	}

	// Get the cached total
	total := t.userTotals[user]

	// Check if we need to prune (O(1) check using the oldest bucket)
	cutoff := t.getCutoffTimestamp()
	needsPruning := false

	// Quick check using the oldest bucket (front of the list)
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

func (t *InMemorySlidingWindowTokenTracker) GetMinTokenCount(ctx context.Context) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Return default min if no active users (minTrackedToken is still at initialization value)
	if t.minTrackedToken == math.MaxFloat64 {
		return tokenTrackerMinTokens, nil
	}
	return t.minTrackedToken, nil
}

func (t *InMemorySlidingWindowTokenTracker) GetMaxTokenCount(ctx context.Context) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// If no active users or all have zero tokens, return default max
	if t.maxTrackedToken == 0 {
		return tokenTrackerMaxTokens, nil
	}
	return t.maxTrackedToken, nil
}

func (t *InMemorySlidingWindowTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	if user == "" {
		return fmt.Errorf("user ID cannot be empty")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Get current timestamp and cutoff
	now := time.Now()
	currentTimestamp := t.bucketUnit.toTimestamp(now)
	cutoff := t.getCutoffTimestamp()

	// Ensure user data structures exist
	_, ok := t.userBucketStore[user]
	if !ok {
		t.userBucketStore[user] = &userBucketData{
			buckets: list.New(),
			lookup:  make(map[int64]*list.Element),
		}
	}

	// Get the total *before* any potential pruning or updates
	oldTotal := t.userTotals[user]

	// Prune first before adding/updating to maintain window size constraint accurately
	// Note: prune might change the user's total internally and update min/max
	// We need the state *after* pruning but *before* adding the new tokens
	t.pruneExpiredBucketsAndUpdateState(user, cutoff)

	// Get total *after* pruning, before adding new tokens
	totalAfterPruning := t.userTotals[user]
	newTokens := inputTokens*t.config.InputTokenWeight + outputTokens*t.config.OutputTokenWeight

	// Check if a bucket for the current timestamp already exists
	if element, exists := t.userBucketStore[user].lookup[currentTimestamp]; exists {
		// Update existing bucket
		node := element.Value.(*bucketNode)
		node.tokenCount += newTokens
	} else {
		// Create and add new bucket to the back of the list
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

func (t *InMemorySlidingWindowTokenTracker) updateUserTotalAndMinMax(user string, oldTotal, newTotal float64) {
	if oldTotal == newTotal {
		return
	}
	if newTotal < 0 {
		newTotal = 0
	}
	t.userTotals[user] = newTotal
	t.removeFromTotals(oldTotal, user)
	t.addToTotals(newTotal, user)
	t.recalcMin(oldTotal, newTotal)
	t.recalcMax(oldTotal, newTotal)
}

// removeFromTotals removes a user from the old total bucket
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

// addToTotals adds a user to the new total bucket
func (t *InMemorySlidingWindowTokenTracker) addToTotals(newTotal float64, user string) {
	if newTotal <= 0 {
		return
	}
	if _, ok := t.totalsToUsers[newTotal]; !ok {
		t.totalsToUsers[newTotal] = make(map[string]struct{})
	}
	t.totalsToUsers[newTotal][user] = struct{}{}
}

// recalcMin recalculates the global minimum tracked token if needed
func (t *InMemorySlidingWindowTokenTracker) recalcMin(oldTotal, newTotal float64) {
	if oldTotal > 0 && oldTotal == t.minTrackedToken && len(t.totalsToUsers[oldTotal]) == 0 {
		t.minTrackedToken = math.Inf(1)
		for total := range t.totalsToUsers {
			if total < t.minTrackedToken {
				t.minTrackedToken = total
			}
		}
		if t.minTrackedToken == math.Inf(1) {
			t.minTrackedToken = math.MaxFloat64
		}
	} else if newTotal > 0 && newTotal < t.minTrackedToken {
		t.minTrackedToken = newTotal
	}
}

// recalcMax recalculates the global maximum tracked token if needed
func (t *InMemorySlidingWindowTokenTracker) recalcMax(oldTotal, newTotal float64) {
	if oldTotal > 0 && oldTotal == t.maxTrackedToken && len(t.totalsToUsers[oldTotal]) == 0 {
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
