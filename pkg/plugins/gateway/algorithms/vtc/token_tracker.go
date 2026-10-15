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
	"container/heap"
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
	user    string
	buckets *list.List              // Doubly linked list of *bucketNode, ordered by timestamp
	lookup  map[int64]*list.Element // Maps timestamp to list element for O(1) access
}

// One entry per nonempty user lets reads find expired usage without scanning
// unrelated users. The oldest bucket is the next one that can expire.
type userExpirationHeap []*userBucketData

func (h userExpirationHeap) Len() int { return len(h) }
func (h userExpirationHeap) Less(i, j int) bool {
	return h[i].buckets.Front().Value.(*bucketNode).timestamp < h[j].buckets.Front().Value.(*bucketNode).timestamp
}
func (h userExpirationHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}
func (h *userExpirationHeap) Push(x any) {
	data := x.(*userBucketData)
	*h = append(*h, data)
}
func (h *userExpirationHeap) Pop() any {
	last := len(*h) - 1
	data := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	return data
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
	expirations     userExpirationHeap
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

// Caller must hold at least the read lock.
func (t *InMemorySlidingWindowTokenTracker) hasExpiredUsers(cutoff int64) bool {
	return len(t.expirations) > 0 && t.expirations[0].buckets.Front().Value.(*bucketNode).timestamp < cutoff
}

// Caller must hold the write lock. Recalculate invalidated extrema once per
// batch, rather than rescanning the totals after each expired user.
func (t *InMemorySlidingWindowTokenTracker) expireUsers(cutoff int64) {
	if !t.hasExpiredUsers(cutoff) {
		return
	}
	oldMin, oldMax := t.minTrackedToken, t.maxTrackedToken
	newMin := math.MaxFloat64
	for t.hasExpiredUsers(cutoff) {
		data := heap.Pop(&t.expirations).(*userBucketData)
		user := data.user
		oldTotal := t.userTotals[user]
		newTotal := t.pruneExpiredBuckets(user, cutoff)
		t.removeFromTotals(oldTotal, user)
		if data.buckets.Len() == 0 {
			delete(t.userBucketStore, user)
			delete(t.userTotals, user)
		} else {
			t.userTotals[user] = newTotal
			t.addToTotals(newTotal, user)
			if newTotal > 0 {
				newMin = min(newMin, newTotal)
			}
			heap.Push(&t.expirations, data)
		}
	}
	t.recalcMin(oldMin, newMin)
	t.recalcMax(oldMax, 0)
}

// Return with the read lock held. Most reads only inspect the heap root;
// acquiring the write lock is necessary only when some usage has expired.
func (t *InMemorySlidingWindowTokenTracker) rlockCurrentWindow() {
	t.mu.RLock()
	if !t.hasExpiredUsers(t.getCutoffTimestamp()) {
		return
	}
	t.mu.RUnlock()
	t.mu.Lock()
	t.expireUsers(t.getCutoffTimestamp())
	t.mu.Unlock()
	t.mu.RLock()
}

// pruneExpiredBuckets removes the user's buckets older than cutoff and returns
// the user's total without them. It leaves userTotals and the min/max tracking
// to the caller. Caller must hold the write lock.
func (t *InMemorySlidingWindowTokenTracker) pruneExpiredBuckets(user string, cutoff int64) float64 {
	total := t.userTotals[user]
	_, ok := t.userBucketStore[user]
	if !ok {
		return total // User doesn't exist or has no buckets
	}

	bucketsList := t.userBucketStore[user].buckets
	lookupMap := t.userBucketStore[user].lookup

	// Iterate from the front (oldest) of the list
	for el := bucketsList.Front(); el != nil; {
		node := el.Value.(*bucketNode)
		if node.timestamp < cutoff {
			// Remove expired bucket
			total -= node.tokenCount
			delete(lookupMap, node.timestamp)
			next := el.Next()
			bucketsList.Remove(el)
			el = next
		} else {
			// Stop as soon as we find a non-expired bucket (list is ordered)
			break
		}
	}
	return max(0, total)
}

// Reads are O(1) unless usage has expired; cleanup is shared across all users.
func (t *InMemorySlidingWindowTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	t.rlockCurrentWindow()
	defer t.mu.RUnlock()
	return t.userTotals[user], nil
}

func (t *InMemorySlidingWindowTokenTracker) GetMinTokenCount(ctx context.Context) (float64, error) {
	t.rlockCurrentWindow()
	defer t.mu.RUnlock()

	// Return the configured floor if no active users (minTrackedToken is still
	// at its initialization value)
	if t.minTrackedToken == math.MaxFloat64 {
		return t.minTokens, nil
	}
	return t.minTrackedToken, nil
}

func (t *InMemorySlidingWindowTokenTracker) GetMaxTokenCount(ctx context.Context) (float64, error) {
	t.rlockCurrentWindow()
	defer t.mu.RUnlock()

	// If no active users or all have zero tokens, return the configured floor
	if t.maxTrackedToken == 0 {
		return t.maxTokens, nil
	}
	return t.maxTrackedToken, nil
}

func (t *InMemorySlidingWindowTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	if user == "" {
		return fmt.Errorf("user ID cannot be empty")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	currentTimestamp := t.bucketUnit.toTimestamp(now)
	t.expireUsers(t.bucketUnit.toTimestamp(now.Add(-t.windowSize)))

	_, ok := t.userBucketStore[user]
	if !ok {
		t.userBucketStore[user] = &userBucketData{
			user:    user,
			buckets: list.New(),
			lookup:  make(map[int64]*list.Element),
		}
	}

	oldTotal := t.userTotals[user]

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
	if !ok {
		heap.Push(&t.expirations, t.userBucketStore[user])
	}

	// Calculate the final new total
	newTotal := oldTotal + newTokens

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
