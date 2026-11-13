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

package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

// mockPodLifecycleListener records every OnPodFlakyReAdd call for assertions. An
// optional onCall hook runs synchronously inside OnPodFlakyReAdd, e.g. to inspect
// cache state at the exact moment the listener fires.
type mockPodLifecycleListener struct {
	mu     sync.Mutex
	calls  []*v1.Pod
	onCall func(pod *v1.Pod)
}

func (m *mockPodLifecycleListener) OnPodFlakyReAdd(pod *v1.Pod) {
	m.mu.Lock()
	m.calls = append(m.calls, pod)
	m.mu.Unlock()
	if m.onCall != nil {
		m.onCall(pod)
	}
}

func (m *mockPodLifecycleListener) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// backdateDeletedPod ages a pod's deletedPodSnapshot to simulate a real gap having
// elapsed (avoids a real sleep) -- comfortably past flakyReAddMinObservedGap,
// comfortably under recentlyDeletedPodGracePeriod.
func backdateDeletedPod(t *testing.T, c *Store, namespace, podName string) {
	t.Helper()
	key := namespace + "/" + podName
	snap, ok := c.recentlyDeletedPods.Load(key)
	assert.True(t, ok)
	snap.deletedAt = snap.deletedAt.Add(-2 * flakyReAddMinObservedGap)
	c.recentlyDeletedPods.Store(key, snap)
}

// TestFlakyReAddNotifiesListenerAfterRealGap covers the core case
// OnPodFlakyReAdd exists for: a pod is genuinely absent from the cache for a
// measurable amount of time (a watch gap or service-discovery blip, not the
// synchronous delete-then-add pair updatePod performs for every in-place K8s pod
// update) and then reappears at the same IP. This is exactly the scenario that
// let requests route to a just-reappeared, unconfirmed-healthy pod with no
// proactive gate.
func TestFlakyReAddNotifiesListenerAfterRealGap(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	listener := &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(listener)

	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	assert.Equal(t, 0, listener.callCount(), "the initial add is not a re-add and must not notify")

	cache.deletePod(pod)
	backdateDeletedPod(t, cache, namespace, podName)

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))

	assert.Equal(t, 1, listener.callCount(), "a re-add after a real gap must notify listeners exactly once")
}

// A listener that unregisters is no longer notified, the others still are, and
// unregistering one that is not registered (or twice) does nothing.
func TestUnregisteredListenerIsNotNotified(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	gone, kept := &mockPodLifecycleListener{}, &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(gone)
	cache.RegisterPodLifecycleListener(kept)
	cache.UnregisterPodLifecycleListener(gone)
	cache.UnregisterPodLifecycleListener(gone)
	cache.UnregisterPodLifecycleListener(&mockPodLifecycleListener{})

	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	cache.deletePod(pod)
	backdateDeletedPod(t, cache, namespace, podName)
	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))

	assert.Equal(t, 0, gone.callCount(), "an unregistered listener must not be notified")
	assert.Equal(t, 1, kept.callCount(), "the other listener is unaffected")
}

// TestFlakyReAddViaUpdatePodNotifiesListener is the updatePod counterpart of the
// test above: a pod that was genuinely absent and then comes back through an
// update event (an informer relist can deliver the first sight of a pod as an
// update) is a flaky re-add too.
func TestFlakyReAddViaUpdatePodNotifiesListener(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	listener := &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(listener)

	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	cache.deletePod(pod)
	backdateDeletedPod(t, cache, namespace, podName)

	newPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.updatePod(pod, newPod)

	assert.Equal(t, 1, listener.callCount(), "a re-add after a real gap must notify through updatePod as well")
	assert.Same(t, newPod, listener.calls[0], "the listener is given the pod that came back")
}

// TestInPlaceUpdateDoesNotNotifyListener covers the failure mode a naive
// implementation (notifying on every recentlyDeletedPods resume, regardless of
// elapsed time) would hit: updatePod performs its own delete-then-add pair for
// every in-place K8s pod update (see addPodLocked's resume branch), microseconds
// apart. Without flakyReAddMinObservedGap, this would fire OnPodFlakyReAdd -- and
// therefore proactively exclude the pod from routing -- on every single routine
// status update to a perfectly healthy, continuously-serving pod.
func TestInPlaceUpdateDoesNotNotifyListener(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	listener := &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(listener)

	oldPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(oldPod)
	assert.Equal(t, 0, listener.callCount())

	newPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	newPod.Status.Phase = v1.PodRunning // an arbitrary in-place field change
	cache.updatePod(oldPod, newPod)

	assert.Equal(t, 0, listener.callCount(),
		"updatePod's own synchronous delete+add for an in-place update must not be mistaken for a flaky re-add")
}

// TestGenuineRecreationDoesNotNotifyListener covers a same-named pod recreated as
// a genuinely different backend (a new IP): addPodLocked's resume condition
// itself does not fire for an IP mismatch (see
// TestDoneRequestCountAfterSameNamePodRecreationDoesNotDecrementNewPod), so there
// is no flaky-re-add signal to report -- a fresh backend has no history to be
// proactively suspicious of.
func TestGenuineRecreationDoesNotNotifyListener(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
	)

	cache := NewForTest()
	listener := &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(listener)

	oldPod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.1")
	cache.addPod(oldPod)
	cache.deletePod(oldPod)
	backdateDeletedPod(t, cache, namespace, podName)

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, "10.0.0.2"))

	assert.Equal(t, 0, listener.callCount())
}

// TestFlakyReAddBelowMinGapDoesNotNotifyListener is the boundary check for
// flakyReAddMinObservedGap itself: a re-add whose observed gap is under the
// threshold must not notify, regardless of how it got there.
func TestFlakyReAddBelowMinGapDoesNotNotifyListener(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	listener := &mockPodLifecycleListener{}
	cache.RegisterPodLifecycleListener(listener)

	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	cache.deletePod(pod)

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))

	assert.Equal(t, 0, listener.callCount(),
		"a re-add with no real observed gap (the common case for this test's own execution speed) must not notify")

	// Sanity check on the threshold's own scale: it must be small enough for the
	// gap check above to be meaningful in a fast unit test, but not zero.
	assert.Greater(t, flakyReAddMinObservedGap, time.Duration(0))
}

// TestFlakyReAddListenerFiresAfterCacheLockReleased guards the contract that
// OnPodFlakyReAdd runs only once c.mu is released, on both the addPod and the
// updatePod path. A listener fired under c.mu would block every other pod/model
// event behind it, and would deadlock the moment it called back into the Cache.
//
// go test -race cannot catch this class of bug on its own: every individual
// access is already properly synchronized, so nothing trips the low-level race
// detector. This test instead directly checks the property the contract
// requires -- that c.mu is not held at the instant the listener runs -- via
// TryLock, which fails immediately (rather than deadlocking the test process)
// if the lock is still held.
func TestFlakyReAddListenerFiresAfterCacheLockReleased(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	for name, reAdd := range map[string]func(c *Store, oldPod *v1.Pod){
		"addPod": func(c *Store, _ *v1.Pod) {
			c.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
		},
		"updatePod": func(c *Store, oldPod *v1.Pod) {
			c.updatePod(oldPod, requestTrackerTestPod(podName, namespace, modelName, podIP))
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache := NewForTest()
			var lockHeldDuringCallback bool
			listener := &mockPodLifecycleListener{
				onCall: func(*v1.Pod) {
					if !cache.mu.TryLock() {
						lockHeldDuringCallback = true
						return
					}
					cache.mu.Unlock()
				},
			}
			cache.RegisterPodLifecycleListener(listener)

			pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
			cache.addPod(pod)
			cache.deletePod(pod)
			backdateDeletedPod(t, cache, namespace, podName)

			reAdd(cache, pod)

			assert.Equal(t, 1, listener.callCount(), "sanity check: the listener must actually have run")
			assert.False(t, lockHeldDuringCallback,
				"OnPodFlakyReAdd must fire only after c.mu is released, per PodLifecycleListener's contract")
		})
	}
}

// TestFlakyReAddConcurrentListenerRegistrationDoesNotRace guards against a data
// race between RegisterPodLifecycleListener (append) and the flaky-re-add
// notification path (addPod/updatePod iterate their copy of the listeners only
// after c.mu is released, per PodLifecycleListener's contract). Without
// RegisterPodLifecycleListener taking c.mu and the notification path copying the
// slice under c.mu before that unlock (podLifecycleListenersLocked), a
// concurrent Register call reallocating the backing array races with the
// unlocked range over c.podLifecycleListeners. Run with -race.
func TestFlakyReAddConcurrentListenerRegistrationDoesNotRace(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()

	// Bounded, not "spin until told to stop": an unbounded registration loop
	// would keep growing c.podLifecycleListeners for as long as it's scheduled,
	// which is unbounded CPU/memory work under -race's instrumentation. A fixed
	// count run concurrently with the loop below is enough to give the race
	// detector many chances to observe an overlap.
	const registrations = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < registrations; i++ {
			cache.RegisterPodLifecycleListener(&mockPodLifecycleListener{})
		}
	}()
	defer wg.Wait()

	// Drive several genuine flaky re-adds while registration is racing above,
	// to give the race detector many chances to observe an overlap between
	// the append and the post-unlock notification loop.
	for i := 0; i < 20; i++ {
		pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
		cache.addPod(pod)
		cache.deletePod(pod)
		backdateDeletedPod(t, cache, namespace, podName)

		cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	}
}
