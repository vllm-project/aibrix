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

package pd

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// DefaultPrefillConsecutiveFailureThreshold is the default number of consecutive
	// prefill failures that triggers pod ejection.
	DefaultPrefillConsecutiveFailureThreshold = 5

	// DefaultPrefillFailureBaseBackoff is the default initial ejection duration.
	DefaultPrefillFailureBaseBackoff = 10 * time.Second

	// DefaultPrefillFailureMaxBackoff is the maximum ejection duration under exponential backoff.
	DefaultPrefillFailureMaxBackoff = 60 * time.Second
)

var (
	envPrefillConsecutiveFailureThreshold = utils.LoadEnvNonNegativeInt("AIBRIX_PREFILL_CONSECUTIVE_FAILURE_THRESHOLD", DefaultPrefillConsecutiveFailureThreshold)
	envPrefillFailureBaseBackoffSeconds   = utils.LoadEnvInt("AIBRIX_PREFILL_FAILURE_BASE_BACKOFF_SECONDS", int(DefaultPrefillFailureBaseBackoff/time.Second))
	envPrefillFailureMaxBackoffSeconds    = utils.LoadEnvInt("AIBRIX_PREFILL_FAILURE_MAX_BACKOFF_SECONDS", int(DefaultPrefillFailureMaxBackoff/time.Second))
)

type prefillFailureState struct {
	consecutiveFailures int
	ejectionCount       int
	ejectedUntil        time.Time
}

// PrefillRequestTracker tracks the number of active prefill requests per pod.
// It is used by the prefill scorer to avoid routing new requests to pods that
// are already heavily loaded, and by the load-imbalance detector to select the
// least-loaded pod when the spread exceeds AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD.
//
// Pods are identified by their pod key, "namespace/name" (see utils.GeneratePodKey): the
// tracker is shared by every model the router serves, and pods in different
// namespaces may share a name.
//
// All methods are safe for concurrent use.
type PrefillRequestTracker struct {
	// podRequestCounts maps pod key -> active prefill request count.
	podRequestCounts sync.Map // map[string]*atomic.Int32
	// requestToPod maps request ID -> pod key for O(1) cleanup in RemovePrefillRequest.
	requestToPod sync.Map // map[string]string

	failureMu                   sync.RWMutex
	podFailures                 map[string]*prefillFailureState
	consecutiveFailureThreshold int
	baseBackoff                 time.Duration
	maxBackoff                  time.Duration
}

// NewPrefillRequestTracker creates a new, empty PrefillRequestTracker.
func NewPrefillRequestTracker() *PrefillRequestTracker {
	baseBackoff := time.Duration(envPrefillFailureBaseBackoffSeconds) * time.Second
	if baseBackoff <= 0 {
		baseBackoff = DefaultPrefillFailureBaseBackoff
	}
	maxBackoff := time.Duration(envPrefillFailureMaxBackoffSeconds) * time.Second
	if maxBackoff <= 0 {
		maxBackoff = DefaultPrefillFailureMaxBackoff
	}
	return &PrefillRequestTracker{
		podFailures:                 make(map[string]*prefillFailureState),
		consecutiveFailureThreshold: envPrefillConsecutiveFailureThreshold,
		baseBackoff:                 baseBackoff,
		maxBackoff:                  maxBackoff,
	}
}

// SetFailureEjectionConfig configures consecutive failure threshold and backoff durations.
func (t *PrefillRequestTracker) SetFailureEjectionConfig(threshold int, baseBackoff, maxBackoff time.Duration) {
	if t == nil {
		return
	}
	t.failureMu.Lock()
	defer t.failureMu.Unlock()
	t.consecutiveFailureThreshold = threshold
	t.baseBackoff = baseBackoff
	t.maxBackoff = maxBackoff
}

// RecordPrefillFailure records a failed prefill attempt for podKey. If consecutive failures
// reach the threshold, the pod is ejected until now + backoff.
func (t *PrefillRequestTracker) RecordPrefillFailure(podKey string, now time.Time) {
	if t == nil {
		return
	}
	t.failureMu.Lock()
	defer t.failureMu.Unlock()

	threshold := t.consecutiveFailureThreshold
	if threshold <= 0 {
		return
	}
	if t.podFailures == nil {
		t.podFailures = make(map[string]*prefillFailureState)
	}
	state, exists := t.podFailures[podKey]
	if !exists {
		state = &prefillFailureState{}
		t.podFailures[podKey] = state
	}
	// If the pod is already currently ejected, ignore in-flight failures
	// so concurrent requests that were already in flight do not stack ejectionCount.
	if !state.ejectedUntil.IsZero() && now.Before(state.ejectedUntil) {
		return
	}
	state.consecutiveFailures++
	if state.consecutiveFailures >= threshold {
		state.ejectionCount++
		base := t.baseBackoff
		if base <= 0 {
			base = DefaultPrefillFailureBaseBackoff
		}
		maxB := t.maxBackoff
		if maxB <= 0 {
			maxB = DefaultPrefillFailureMaxBackoff
		}
		if base > maxB {
			base = maxB
		}
		backoff := base
		if state.ejectionCount > 1 {
			shift := state.ejectionCount - 1
			if shift > 30 {
				shift = 30
			}
			multiplier := 1 << shift
			backoff = time.Duration(multiplier) * base
			if backoff > maxB || backoff <= 0 {
				backoff = maxB
			}
		}
		state.ejectedUntil = now.Add(backoff)
		klog.Warningf("prefill pod %s ejected after %d consecutive failures (ejection %d, backoff %s until %s)",
			podKey, state.consecutiveFailures, state.ejectionCount, backoff, state.ejectedUntil)
	}
}

// RecordPrefillSuccess records a successful prefill for podKey, resetting its failure and ejection state.
func (t *PrefillRequestTracker) RecordPrefillSuccess(podKey string) {
	if t == nil {
		return
	}
	t.failureMu.Lock()
	defer t.failureMu.Unlock()

	if t.podFailures == nil {
		return
	}
	state, exists := t.podFailures[podKey]
	if !exists {
		return
	}
	if state.consecutiveFailures > 0 || state.ejectionCount > 0 {
		klog.InfoS("prefill_pod_recovered", "pod", podKey,
			"previous_failures", state.consecutiveFailures,
			"previous_ejections", state.ejectionCount)
		state.consecutiveFailures = 0
		state.ejectionCount = 0
		state.ejectedUntil = time.Time{}
	}
}

// IsPrefillPodEjected reports whether the pod is currently ejected at time now.
func (t *PrefillRequestTracker) IsPrefillPodEjected(podKey string, now time.Time) bool {
	if t == nil {
		return false
	}
	t.failureMu.RLock()
	defer t.failureMu.RUnlock()

	if t.consecutiveFailureThreshold <= 0 || t.podFailures == nil {
		return false
	}
	state, exists := t.podFailures[podKey]
	if !exists {
		return false
	}
	return state.ejectedUntil.After(now)
}

// FilterEligiblePrefillPods returns candidate prefill pods excluding ejected ones.
// If all candidate pods would be ejected, it returns all pods to preserve availability.
func (t *PrefillRequestTracker) FilterEligiblePrefillPods(pods []*v1.Pod, now time.Time) []*v1.Pod {
	if t == nil || len(pods) <= 1 {
		return pods
	}
	t.failureMu.RLock()
	defer t.failureMu.RUnlock()

	if t.consecutiveFailureThreshold <= 0 || t.podFailures == nil {
		return pods
	}

	eligible := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		state, exists := t.podFailures[podKey]
		if !exists || !state.ejectedUntil.After(now) {
			eligible = append(eligible, pod)
		}
	}
	if len(eligible) == 0 {
		// All candidate pods are currently ejected; do not eject 100% of the fleet.
		return pods
	}
	return eligible
}

// GetPrefillPodFailureState returns (consecutiveFailures, ejectionCount, isEjected) for podKey.
func (t *PrefillRequestTracker) GetPrefillPodFailureState(podKey string, now time.Time) (int, int, bool) {
	if t == nil {
		return 0, 0, false
	}
	t.failureMu.RLock()
	defer t.failureMu.RUnlock()

	if t.podFailures == nil {
		return 0, 0, false
	}
	state, exists := t.podFailures[podKey]
	if !exists {
		return 0, 0, false
	}
	return state.consecutiveFailures, state.ejectionCount, state.ejectedUntil.After(now)
}

// AddPrefillRequest records that requestID has been assigned to the pod
// identified by podKey (see utils.GeneratePodKey) and increments that pod's active-request
// counter. Called by the pod selector as soon as the prefill pod is chosen,
// under the same lock as the selection, so concurrent routing sees in-flight
// assignments. Must be paired with RemovePrefillRequest when prefill
// completes or the assignment is abandoned.
func (t *PrefillRequestTracker) AddPrefillRequest(requestID, podKey string) {
	countInterface, _ := t.podRequestCounts.LoadOrStore(podKey, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)

	newCount := count.Add(1)
	t.requestToPod.Store(requestID, podKey)

	klog.V(4).InfoS("prefill_request_added",
		"request_id", requestID,
		"pod", podKey,
		"new_count", newCount)
}

// RemovePrefillRequest decrements the active-request counter for the pod that
// was assigned requestID and removes the request-to-pod mapping. It is a no-op
// if requestID was never added (e.g. the tracker was bypassed). The counter is
// clamped to zero if it would otherwise go negative.
func (t *PrefillRequestTracker) RemovePrefillRequest(requestID string) {
	podKeyInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		klog.V(4).InfoS("prefill_request_not_found_for_removal", "request_id", requestID)
		return
	}

	podKey := podKeyInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podKey)
	if !exists {
		klog.V(4).InfoS("pod_counter_not_found", "pod", podKey, "request_id", requestID)
		return
	}

	count := countInterface.(*atomic.Int32)
	newCount := count.Add(-1)

	if newCount < 0 {
		for {
			v := count.Load()
			if v >= 0 {
				newCount = v
				break
			}
			if count.CompareAndSwap(v, 0) {
				newCount = 0
				break
			}
		}
	}

	klog.V(4).InfoS("prefill_request_removed",
		"request_id", requestID,
		"pod", podKey,
		"new_count", newCount)
}

// GetPrefillRequestCountsForPods returns a map of pod key (namespace/name) →
// active prefill request count for each pod in pods. Pods with no recorded
// requests are included with a count of 0.
func (t *PrefillRequestTracker) GetPrefillRequestCountsForPods(pods []*v1.Pod) map[string]int32 {
	counts := make(map[string]int32)
	for _, pod := range pods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		countInterface, exists := t.podRequestCounts.Load(podKey)
		if !exists {
			counts[podKey] = 0
		} else {
			counts[podKey] = countInterface.(*atomic.Int32).Load()
		}
	}
	return counts
}

// GetPrefillRequestCountsForPod returns the current active prefill request
// count for the pod identified by podKey, or 0 if no requests have been
// recorded for that pod.
func (t *PrefillRequestTracker) GetPrefillRequestCountsForPod(podKey string) int {
	countInterface, exists := t.podRequestCounts.Load(podKey)
	if !exists {
		return 0
	}
	return int(countInterface.(*atomic.Int32).Load())
}

// PendingDecodeTracker tracks decode pods that have been selected for a request
// but whose RealtimeNumRequestsRunning metric has not yet been incremented by
// the metrics scrape. This bridges the gap between decode pod selection (during
// routing) and the moment the decode pod actually starts processing the request,
// preventing concurrent requests from all being routed to the same decode pod
// during the prefill phase when the metric is still stale.
//
// Pods are identified by their pod key, "namespace/name" (see utils.GeneratePodKey), for
// the same reason as in PrefillRequestTracker.
//
// All methods are safe for concurrent use, including nil receivers (the tracker
// is optional and may be nil when disabled).
type PendingDecodeTracker struct {
	// podRequestCounts maps pod key → pending decode request count.
	podRequestCounts sync.Map // map[string]*atomic.Int32
	// requestToPod maps request ID → pod key for O(1) cleanup in RemovePendingDecode.
	requestToPod sync.Map // map[string]string
}

// NewPendingDecodeTracker creates a new, empty PendingDecodeTracker.
func NewPendingDecodeTracker() *PendingDecodeTracker {
	return &PendingDecodeTracker{}
}

// AddPendingDecode records that requestID has been assigned to the pod
// identified by podKey (see utils.GeneratePodKey) and increments that pod's pending-decode
// counter. Called by the pod selector as soon as the decode pod is chosen,
// under the same lock as the selection. RemovePendingDecode is deferred in
// Route.
func (t *PendingDecodeTracker) AddPendingDecode(requestID, podKey string) {
	if t == nil {
		return
	}
	countInterface, _ := t.podRequestCounts.LoadOrStore(podKey, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)
	count.Add(1)
	t.requestToPod.Store(requestID, podKey)
}

// RemovePendingDecode decrements the pending-decode counter for the pod
// assigned to requestID and removes the request-to-pod mapping. It is a no-op
// if requestID is unknown. The counter is clamped to zero via a CAS loop to
// avoid erasing a concurrent Add(1) that races with the clamp.
func (t *PendingDecodeTracker) RemovePendingDecode(requestID string) {
	if t == nil {
		return
	}
	podKeyInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		return
	}
	podKey := podKeyInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podKey)
	if !exists {
		return
	}
	count := countInterface.(*atomic.Int32)
	if newCount := count.Add(-1); newCount < 0 {
		// CAS loop: clamp to 0 only while the value is still negative so that
		// a concurrent Add(1) is not silently erased by a plain Store(0).
		for {
			v := count.Load()
			if v >= 0 {
				return
			}
			if count.CompareAndSwap(v, 0) {
				return
			}
		}
	}
}

// GetPendingDecodeCount returns the current pending-decode request count for
// the pod identified by podKey as a float64 (for direct addition to metric
// values). Returns 0 for unknown pods or when the receiver is nil.
func (t *PendingDecodeTracker) GetPendingDecodeCount(podKey string) float64 {
	if t == nil {
		return 0
	}
	countInterface, exists := t.podRequestCounts.Load(podKey)
	if !exists {
		return 0
	}
	return float64(countInterface.(*atomic.Int32).Load())
}
