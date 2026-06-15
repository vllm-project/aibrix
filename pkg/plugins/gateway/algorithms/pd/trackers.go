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

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// PrefillRequestTracker tracks the number of active prefill requests per pod.
// It is used by the prefill scorer to avoid routing new requests to pods that
// are already heavily loaded, and by the load-imbalance detector to select the
// least-loaded pod when the spread exceeds AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD.
//
// All methods are safe for concurrent use.
type PrefillRequestTracker struct {
	// podRequestCounts maps pod name → active prefill request count.
	podRequestCounts sync.Map // map[string]*atomic.Int32
	// requestToPod maps request ID → pod name for O(1) cleanup in RemovePrefillRequest.
	requestToPod sync.Map // map[string]string
}

// NewPrefillRequestTracker creates a new, empty PrefillRequestTracker.
func NewPrefillRequestTracker() *PrefillRequestTracker {
	return &PrefillRequestTracker{}
}

// AddPrefillRequest records that requestID has been dispatched to podName and
// increments that pod's active-request counter. Must be paired with a
// corresponding RemovePrefillRequest call (typically via defer).
func (t *PrefillRequestTracker) AddPrefillRequest(requestID, podName string) {
	countInterface, _ := t.podRequestCounts.LoadOrStore(podName, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)

	newCount := count.Add(1)
	t.requestToPod.Store(requestID, podName)

	klog.V(4).InfoS("prefill_request_added",
		"request_id", requestID,
		"pod_name", podName,
		"new_count", newCount)
}

// RemovePrefillRequest decrements the active-request counter for the pod that
// was assigned requestID and removes the request-to-pod mapping. It is a no-op
// if requestID was never added (e.g. the tracker was bypassed). The counter is
// clamped to zero if it would otherwise go negative.
func (t *PrefillRequestTracker) RemovePrefillRequest(requestID string) {
	podNameInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		klog.V(4).InfoS("prefill_request_not_found_for_removal", "request_id", requestID)
		return
	}

	podName := podNameInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podName)
	if !exists {
		klog.V(4).InfoS("pod_counter_not_found", "pod_name", podName, "request_id", requestID)
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
		"pod_name", podName,
		"new_count", newCount)
}

// GetPrefillRequestCountsForPods returns a map of pod name → active prefill
// request count for each pod in pods. Pods with no recorded requests are
// included with a count of 0.
func (t *PrefillRequestTracker) GetPrefillRequestCountsForPods(pods []*v1.Pod) map[string]int32 {
	counts := make(map[string]int32)
	for _, pod := range pods {
		countInterface, exists := t.podRequestCounts.Load(pod.Name)
		if !exists {
			counts[pod.Name] = 0
		} else {
			counts[pod.Name] = countInterface.(*atomic.Int32).Load()
		}
	}
	return counts
}

// GetPrefillRequestCountsForPod returns the current active prefill request
// count for podname, or 0 if no requests have been recorded for that pod.
func (t *PrefillRequestTracker) GetPrefillRequestCountsForPod(podname string) int {
	countInterface, exists := t.podRequestCounts.Load(podname)
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
// All methods are safe for concurrent use, including nil receivers (the tracker
// is optional and may be nil when disabled).
type PendingDecodeTracker struct {
	// podRequestCounts maps pod name → pending decode request count.
	podRequestCounts sync.Map // map[string]*atomic.Int32
	// requestToPod maps request ID → pod name for O(1) cleanup in RemovePendingDecode.
	requestToPod sync.Map // map[string]string
}

// NewPendingDecodeTracker creates a new, empty PendingDecodeTracker.
func NewPendingDecodeTracker() *PendingDecodeTracker {
	return &PendingDecodeTracker{}
}

// AddPendingDecode records that requestID has been assigned to podName and
// increments that pod's pending-decode counter. Must be paired with a
// corresponding RemovePendingDecode call (typically via defer in Route).
func (t *PendingDecodeTracker) AddPendingDecode(requestID, podName string) {
	if t == nil {
		return
	}
	countInterface, _ := t.podRequestCounts.LoadOrStore(podName, &atomic.Int32{})
	count := countInterface.(*atomic.Int32)
	count.Add(1)
	t.requestToPod.Store(requestID, podName)
}

// RemovePendingDecode decrements the pending-decode counter for the pod
// assigned to requestID and removes the request-to-pod mapping. It is a no-op
// if requestID is unknown. The counter is clamped to zero via a CAS loop to
// avoid erasing a concurrent Add(1) that races with the clamp.
func (t *PendingDecodeTracker) RemovePendingDecode(requestID string) {
	if t == nil {
		return
	}
	podNameInterface, exists := t.requestToPod.LoadAndDelete(requestID)
	if !exists {
		return
	}
	podName := podNameInterface.(string)
	countInterface, exists := t.podRequestCounts.Load(podName)
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
// podName as a float64 (for direct addition to metric values). Returns 0 for
// unknown pods or when the receiver is nil.
func (t *PendingDecodeTracker) GetPendingDecodeCount(podName string) float64 {
	if t == nil {
		return 0
	}
	countInterface, exists := t.podRequestCounts.Load(podName)
	if !exists {
		return 0
	}
	return float64(countInterface.(*atomic.Int32).Load())
}
