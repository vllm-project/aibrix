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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRequestTrackers_SameNameInTwoNamespaces: the request trackers are shared
// by every model the router serves, so same-named pods in two namespaces must
// be counted separately.
func TestRequestTrackers_SameNameInTwoNamespaces(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "team-a"}}
	podB := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "team-b"}}
	keyA := utils.GeneratePodKey(podA.Namespace, podA.Name)
	keyB := utils.GeneratePodKey(podB.Namespace, podB.Name)

	prefill := NewPrefillRequestTracker()
	prefill.AddPrefillRequest("req-1", keyA)
	prefill.AddPrefillRequest("req-2", keyA)
	assert.Equal(t, 2, prefill.GetPrefillRequestCountsForPod(keyA))
	assert.Equal(t, 0, prefill.GetPrefillRequestCountsForPod(keyB))
	assert.Equal(t, map[string]int32{keyA: 2, keyB: 0}, prefill.GetPrefillRequestCountsForPods([]*v1.Pod{podA, podB}))
	prefill.RemovePrefillRequest("req-1")
	assert.Equal(t, 1, prefill.GetPrefillRequestCountsForPod(keyA))

	pending := NewPendingDecodeTracker()
	pending.AddPendingDecode("req-1", keyA)
	assert.Equal(t, float64(1), pending.GetPendingDecodeCount(keyA))
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(keyB))
	pending.RemovePendingDecode("req-1")
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(keyA))
}

func TestPrefillRequestTracker_FailureEjectionAndRecovery(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "default"}}
	podB := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-1", Namespace: "default"}}
	keyA := utils.GeneratePodKey(podA.Namespace, podA.Name)
	keyB := utils.GeneratePodKey(podB.Namespace, podB.Name)

	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	tracker := NewPrefillRequestTracker()
	tracker.SetFailureEjectionConfig(3, 10*time.Second, 60*time.Second)

	// Initially neither pod is ejected
	assert.False(t, tracker.IsPrefillPodEjected(keyA, now))
	assert.False(t, tracker.IsPrefillPodEjected(keyB, now))
	assert.Equal(t, []*v1.Pod{podA, podB}, tracker.FilterEligiblePrefillPods([]*v1.Pod{podA, podB}, now))

	// Pod A fails 2 times (below threshold 3)
	tracker.RecordPrefillFailure(keyA, now)
	tracker.RecordPrefillFailure(keyA, now)
	fails, ejections, ejected := tracker.GetPrefillPodFailureState(keyA, now)
	assert.Equal(t, 2, fails)
	assert.Equal(t, 0, ejections)
	assert.False(t, ejected)

	// Pod A fails 3rd time -> triggers ejection
	tracker.RecordPrefillFailure(keyA, now)
	fails, ejections, ejected = tracker.GetPrefillPodFailureState(keyA, now)
	assert.Equal(t, 3, fails)
	assert.Equal(t, 1, ejections)
	assert.True(t, ejected)
	assert.True(t, tracker.IsPrefillPodEjected(keyA, now))

	// While Pod A is ejected, FilterEligiblePrefillPods returns only Pod B
	assert.Equal(t, []*v1.Pod{podB}, tracker.FilterEligiblePrefillPods([]*v1.Pod{podA, podB}, now))

	// Pod B also fails 3 times -> both pods now ejected
	tracker.RecordPrefillFailure(keyB, now)
	tracker.RecordPrefillFailure(keyB, now)
	tracker.RecordPrefillFailure(keyB, now)
	assert.True(t, tracker.IsPrefillPodEjected(keyB, now))

	// Safety property: when all candidate pods are ejected, do not eject 100% of fleet
	assert.Equal(t, []*v1.Pod{podA, podB}, tracker.FilterEligiblePrefillPods([]*v1.Pod{podA, podB}, now))

	// After base backoff (10s), Pod A is no longer ejected
	afterBackoff := now.Add(11 * time.Second)
	assert.False(t, tracker.IsPrefillPodEjected(keyA, afterBackoff))
	assert.False(t, tracker.IsPrefillPodEjected(keyB, afterBackoff))

	// Pod A fails again -> repeat ejection gets 2x backoff (20s)
	tracker.RecordPrefillFailure(keyA, afterBackoff)
	_, ejections, ejected = tracker.GetPrefillPodFailureState(keyA, afterBackoff)
	assert.Equal(t, 2, ejections)
	assert.True(t, ejected)
	// Still ejected at +15s (26s total)
	assert.True(t, tracker.IsPrefillPodEjected(keyA, afterBackoff.Add(15*time.Second)))
	// Recovered after 20s backoff (at 32s total)
	assert.False(t, tracker.IsPrefillPodEjected(keyA, afterBackoff.Add(21*time.Second)))

	// Success clears failure and ejection state
	tracker.RecordPrefillSuccess(keyA)
	fails, ejections, ejected = tracker.GetPrefillPodFailureState(keyA, afterBackoff)
	assert.Equal(t, 0, fails)
	assert.Equal(t, 0, ejections)
	assert.False(t, ejected)
}

func TestPrefillRequestTracker_InFlightFailuresDoNotStackEjection(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "default"}}
	keyA := utils.GeneratePodKey(podA.Namespace, podA.Name)

	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	tracker := NewPrefillRequestTracker()
	tracker.SetFailureEjectionConfig(3, 10*time.Second, 60*time.Second)

	// Trigger ejection after 3 failures
	tracker.RecordPrefillFailure(keyA, now)
	tracker.RecordPrefillFailure(keyA, now)
	tracker.RecordPrefillFailure(keyA, now)

	fails, ejections, ejected := tracker.GetPrefillPodFailureState(keyA, now)
	assert.Equal(t, 3, fails)
	assert.Equal(t, 1, ejections)
	assert.True(t, ejected)

	// Concurrent in-flight requests that fail while the pod is already ejected
	// must NOT increment ejectionCount or push the backoff further out.
	tracker.RecordPrefillFailure(keyA, now.Add(1*time.Second))
	tracker.RecordPrefillFailure(keyA, now.Add(2*time.Second))
	tracker.RecordPrefillFailure(keyA, now.Add(3*time.Second))

	fails, ejections, ejected = tracker.GetPrefillPodFailureState(keyA, now.Add(3*time.Second))
	assert.Equal(t, 3, fails)
	assert.Equal(t, 1, ejections)
	assert.True(t, ejected)

	// Backoff expires at now + 10s
	assert.False(t, tracker.IsPrefillPodEjected(keyA, now.Add(11*time.Second)))
}

func TestPrefillRequestTracker_ThresholdZeroDisablesEjection(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "default"}}
	keyA := utils.GeneratePodKey(podA.Namespace, podA.Name)

	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	tracker := NewPrefillRequestTracker()
	tracker.SetFailureEjectionConfig(0, 10*time.Second, 60*time.Second)

	// Even with many failures, pod is never ejected
	for i := 0; i < 10; i++ {
		tracker.RecordPrefillFailure(keyA, now)
	}

	assert.False(t, tracker.IsPrefillPodEjected(keyA, now))
	assert.Equal(t, []*v1.Pod{podA}, tracker.FilterEligiblePrefillPods([]*v1.Pod{podA}, now))
}
