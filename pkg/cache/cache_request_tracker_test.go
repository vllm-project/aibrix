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
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockRequestTracker implements RequestTracker for testing
type mockRequestTracker struct {
	addCalled       bool
	doneCalled      bool
	doneTraceCalled bool
	lastCtx         *types.RoutingContext
}

func requestTrackerTestPod(name, namespace, model, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				modelIdentifier: model,
			},
		},
		Status: v1.PodStatus{
			PodIP: ip,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
}

func (m *mockRequestTracker) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) int64 {
	m.addCalled = true
	m.lastCtx = ctx
	return 1
}

func (m *mockRequestTracker) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	m.doneCalled = true
	m.lastCtx = ctx
}

func (m *mockRequestTracker) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens int64, outputTokens int64, traceTerm int64) {
	m.doneTraceCalled = true
	m.lastCtx = ctx
}

// TestDoneRequestCount_NilContext verifies that DoneRequestCount handles nil context
// without panicking. This is critical for the panic fix where context cancellation
// leads to nil RoutingContext being passed to DoneRequestCount.
func TestDoneRequestCount_NilContext(t *testing.T) {
	cache := NewForTest()
	tracker := &mockRequestTracker{}
	cache.RegisterRequestTracker(tracker)

	t.Run("nil context should not panic", func(t *testing.T) {
		assert.NotPanics(t, func() {
			cache.DoneRequestCount(nil, "test-request", "test-model", 0)
		})

		assert.True(t, tracker.doneCalled, "tracker should be called")
		assert.Nil(t, tracker.lastCtx, "tracker should receive nil context")
	})

	t.Run("valid context should work normally", func(t *testing.T) {
		tracker.doneCalled = false
		ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "", "test-request", "")

		assert.NotPanics(t, func() {
			cache.DoneRequestCount(ctx, "test-request", "test-model", 0)
		})

		assert.True(t, tracker.doneCalled, "tracker should be called")
		assert.NotNil(t, tracker.lastCtx, "tracker should receive valid context")
		assert.Equal(t, "test-model", tracker.lastCtx.Model)
	})
}

// TestDoneRequestTrace_NilContext verifies that DoneRequestTrace handles nil context
// without panicking.
func TestDoneRequestTrace_NilContext(t *testing.T) {
	cache := NewForTest()
	tracker := &mockRequestTracker{}
	cache.RegisterRequestTracker(tracker)

	t.Run("nil context should not panic", func(t *testing.T) {
		assert.NotPanics(t, func() {
			cache.DoneRequestTrace(nil, "test-request", "test-model", 100, 50, 0)
		})

		assert.True(t, tracker.doneTraceCalled, "tracker should be called")
		assert.Nil(t, tracker.lastCtx, "tracker should receive nil context")
	})

	t.Run("valid context should work normally", func(t *testing.T) {
		tracker.doneTraceCalled = false
		ctx := types.NewRoutingContext(context.Background(), "test-algorithm", "test-model", "", "test-request", "")

		assert.NotPanics(t, func() {
			cache.DoneRequestTrace(ctx, "test-request", "test-model", 100, 50, 0)
		})

		assert.True(t, tracker.doneTraceCalled, "tracker should be called")
		assert.NotNil(t, tracker.lastCtx, "tracker should receive valid context")
	})
}

// TestAddRequestCount_NilContext verifies that AddRequestCount handles nil context
// gracefully.
func TestAddRequestCount_NilContext(t *testing.T) {
	cache := InitWithRequestTrace(NewForTest())
	tracker := &mockRequestTracker{}
	cache.RegisterRequestTracker(tracker)

	t.Run("nil context should not panic", func(t *testing.T) {
		assert.NotPanics(t, func() {
			traceTerm := cache.AddRequestCount(nil, "test-request", "test-model")
			assert.Greater(t, traceTerm, int64(0), "should return valid trace term")
		})

		assert.True(t, tracker.addCalled, "tracker should be called")
		assert.Nil(t, tracker.lastCtx, "tracker should receive nil context")
	})
}

// TestRequestTrackerChain_NilContext tests that multiple registered trackers
// all handle nil context correctly.
func TestRequestTrackerChain_NilContext(t *testing.T) {
	cache := NewForTest()
	tracker1 := &mockRequestTracker{}
	tracker2 := &mockRequestTracker{}
	cache.RegisterRequestTracker(tracker1)
	cache.RegisterRequestTracker(tracker2)

	t.Run("all trackers receive nil context", func(t *testing.T) {
		assert.NotPanics(t, func() {
			cache.DoneRequestCount(nil, "test-request", "test-model", 0)
		})

		assert.True(t, tracker1.doneCalled, "tracker1 should be called")
		assert.True(t, tracker2.doneCalled, "tracker2 should be called")
		assert.Nil(t, tracker1.lastCtx, "tracker1 should receive nil context")
		assert.Nil(t, tracker2.lastCtx, "tracker2 should receive nil context")
	})
}

// TestContextCancellation_RealWorldScenario simulates the actual panic scenario:
// 1. Request starts with valid context
// 2. Context gets cancelled (client disconnect, timeout, etc.)
// 3. Gateway calls DoneRequestCount with potentially nil or cancelled context
func TestContextCancellation_RealWorldScenario(t *testing.T) {
	cache := InitWithRequestTrace(NewForTest())
	tracker := &mockRequestTracker{}
	cache.RegisterRequestTracker(tracker)

	t.Run("context cancelled before routing completes", func(t *testing.T) {
		// Create cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		routingCtx := types.NewRoutingContext(ctx, "test-algorithm", "test-model", "", "test-request", "")

		// Cancel immediately (simulating early cancellation)
		cancel()

		// In gateway.go:183, 241, 250, DoneRequestCount is called even with cancelled context
		assert.NotPanics(t, func() {
			cache.DoneRequestCount(routingCtx, "test-request", "test-model", 0)
		}, "should handle cancelled context gracefully")
	})

	t.Run("nil context passed directly", func(t *testing.T) {
		// In some error paths, routerCtx might be nil
		assert.NotPanics(t, func() {
			cache.DoneRequestCount(nil, "test-request", "test-model", 0)
		}, "should handle nil context gracefully")
	})
}

func TestDoneRequestCountAfterSameNamePodRecreationDoesNotDecrementNewPod(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		requestID = "request-before-recreate"
	)

	cache := NewForTest()
	oldPod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.1")
	cache.addPod(oldPod)
	oldMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(oldPod)

	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&oldMetaPod.runningRequests))

	cache.deletePod(oldPod)
	newPod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.2")
	cache.addPod(newPod)
	newMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	assert.NotSame(t, oldMetaPod, newMetaPod)

	cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)

	assert.Equal(t, int32(0), atomic.LoadInt32(&oldMetaPod.runningRequests))
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests))
}

// TestAddPodAfterFlakyDeleteWithSameIPPreservesRunningRequests covers the
// opposite scenario from the recreation test above: the pod key is deleted
// and re-added at the *same* IP, e.g. a transient health-check flap on a pod
// that never actually stopped serving. The re-added pod should resume its
// running-request count instead of restarting at zero, and a request that
// was already in flight before the flap must still correctly decrement it on
// completion rather than the decrement silently landing on the orphaned
// pre-flap object.
func TestAddPodAfterFlakyDeleteWithSameIPPreservesRunningRequests(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		requestID = "request-across-flap"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	oldPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(oldPod)
	oldMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(oldPod)

	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&oldMetaPod.runningRequests))

	// The pod is briefly dropped from the cache and re-added moments later
	// at the same IP -- the in-flight request above is still running on the
	// real engine the whole time; only the gateway's bookkeeping blipped.
	cache.deletePod(oldPod)
	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))

	newMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	assert.NotSame(t, oldMetaPod, newMetaPod)
	assert.Equal(t, int32(1), atomic.LoadInt32(&newMetaPod.runningRequests),
		"re-adding the same pod (same IP) after a flaky delete should preserve its running-request count instead of resetting to zero")

	cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)

	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests),
		"a request in flight across the flap must decrement the current pod entry, not vanish into the orphaned pre-flap object")
	// The pre-flap object is orphaned (no longer reachable via metaPods) and
	// is never touched again -- the decrement above redirected to newMetaPod
	// instead, so this stays at whatever it was when the flap happened.
	assert.Equal(t, int32(1), atomic.LoadInt32(&oldMetaPod.runningRequests))
}

// gapTestLoadProvider is a CappedLoadProvider that returns a fixed consumption
// so tests can seed pending-load without a GPU profile.
type gapTestLoadProvider struct {
	load float64
}

func (p gapTestLoadProvider) GetUtilization(*types.RoutingContext, *v1.Pod) (float64, error) {
	return 0, nil
}

func (p gapTestLoadProvider) GetConsumption(*types.RoutingContext, *v1.Pod) (float64, error) {
	return p.load, nil
}

func (p gapTestLoadProvider) Cap() float64 { return 1 }

// TestDoneRequestCountDuringDeleteAddGapDecrementsPendingSnapshot covers a
// request that completes *inside* the delete/re-add gap itself -- the pod
// key is deleted but hasn't been re-added yet when DoneRequestCount runs.
// This is the realistic flap timing, not just the happy-path ordering in
// TestAddPodAfterFlakyDeleteWithSameIPPreservesRunningRequests (which only
// exercises completion *after* the re-add): a pod flap is exactly a
// delete/add window, and in-flight requests can finish at any point inside
// it. Without decrementing the pending snapshot in place, the eventual
// re-add would resume from a count that still includes this already-finished
// request, permanently inflating it by one.
//
// Also covers the snapshot path's other mutation, which the local
// running-request assertion alone would not catch: pending-load is
// decremented on the snapshot and resumed onto the re-added pod.
func TestDoneRequestCountDuringDeleteAddGapDecrementsPendingSnapshot(t *testing.T) {
	const (
		modelName   = "test-model"
		namespace   = "default"
		podName     = "decode-0"
		requestID   = "request-during-gap"
		podIP       = "10.0.0.1"
		pendingLoad = 0.25
	)

	cache := NewForTest()
	cache.pendingLoadProvider = gapTestLoadProvider{load: pendingLoad}

	oldPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(oldPod)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(oldPod)
	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)

	oldMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	assert.Equal(t, pendingLoad, oldMetaPod.pendingLoadUtilization.Load())

	// The pod is dropped from the cache (flap) but hasn't reappeared yet --
	// no addPod call in between.
	cache.deletePod(oldPod)
	_, ok = cache.metaPods.Load(namespace + "/" + podName)
	assert.False(t, ok, "pod should be absent from the cache mid-gap")

	snap, ok := cache.recentlyDeletedPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&snap.runningRequests))
	assert.Equal(t, pendingLoad, snap.pendingLoadUtilization.Load(),
		"delete should snapshot pending-load so a mid-gap completion can decrement it in place")

	// The in-flight request finishes while the pod is still gone.
	cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)

	assert.Equal(t, int32(0), atomic.LoadInt32(&snap.runningRequests),
		"a completion mid-gap should decrement the pending snapshot in place")
	assert.Equal(t, 0.0, snap.pendingLoadUtilization.Load(),
		"a completion mid-gap should decrement snapshot pending-load, not leave it for the re-add to resume")

	// The pod reappears at the same IP.
	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	newMetaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests),
		"re-add should resume from the already-decremented count, not the stale count from delete time")
	assert.Equal(t, 0.0, newMetaPod.pendingLoadUtilization.Load(),
		"re-add should resume from the already-decremented pending-load, not the stale value from delete time")
}

// TestAddPodAfterDeleteGracePeriodExpiryDoesNotPreserveRunningRequests
// verifies the preservation in TestAddPodAfterFlakyDeleteWithSameIPPreservesRunningRequests
// is bounded: a pod key that stays gone longer than recentlyDeletedPodGracePeriod
// is presumed genuinely dead, not just slow to answer a health probe, so a
// later re-add must not resurrect a stale count.
func TestAddPodAfterDeleteGracePeriodExpiryDoesNotPreserveRunningRequests(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	metaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)
	atomic.StoreInt32(&metaPod.runningRequests, 5)

	cache.deletePod(pod)

	// Backdate the snapshot past the grace period instead of sleeping.
	key := namespace + "/" + podName
	snap, ok := cache.recentlyDeletedPods.Load(key)
	assert.True(t, ok)
	snap.deletedAt = snap.deletedAt.Add(-recentlyDeletedPodGracePeriod)
	cache.recentlyDeletedPods.Store(key, snap)

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	newMetaPod, ok := cache.metaPods.Load(key)
	assert.True(t, ok)
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests))
}

func TestDoneRequestWithNilContextCleansPreviouslyAddedPodStats(t *testing.T) {
	for _, tt := range []struct {
		name string
		done func(cache *Store, requestID, modelName string, traceTerm int64)
	}{
		{
			name: "DoneRequestCount",
			done: func(cache *Store, requestID, modelName string, traceTerm int64) {
				cache.DoneRequestCount(nil, requestID, modelName, traceTerm)
			},
		},
		{
			name: "DoneRequestTrace",
			done: func(cache *Store, requestID, modelName string, traceTerm int64) {
				cache.DoneRequestTrace(nil, requestID, modelName, 10, 20, traceTerm)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const (
				modelName = "test-model"
				namespace = "default"
				podName   = "decode-0"
				requestID = "request-with-nil-done-context"
			)

			cache := NewForTest()
			pod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.1")
			cache.addPod(pod)
			metaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
			assert.True(t, ok)

			routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
			routingCtx.SetTargetPod(pod)

			traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
			assert.Equal(t, int32(1), atomic.LoadInt32(&metaPod.runningRequests))
			// Primary attempt key plus nil-ctx requestID alias.
			assert.Equal(t, 2, cache.podStats.Len())

			tt.done(cache, requestID, modelName, traceTerm)

			assert.Equal(t, int32(0), atomic.LoadInt32(&metaPod.runningRequests))
			assert.Equal(t, 0, cache.podStats.Len())
		})
	}
}

func TestDoubleCleanupOnlyDecrementsPodStatsOnce(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		requestID = "request-with-double-cleanup"
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, "10.0.0.1")
	cache.addPod(pod)
	metaPod, ok := cache.metaPods.Load(namespace + "/" + podName)
	assert.True(t, ok)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(pod)

	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&metaPod.runningRequests))
	// Primary attempt key plus nil-ctx requestID alias.
	assert.Equal(t, 2, cache.podStats.Len())

	cache.DoneRequestTrace(routingCtx, requestID, modelName, 10, 20, traceTerm)
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPod.runningRequests))
	assert.Equal(t, 0, cache.podStats.Len())

	assert.NotPanics(t, func() {
		cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
	})
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPod.runningRequests))
	assert.Equal(t, 0, cache.podStats.Len())
}

// TestConcurrentRequestsAndPodFlapDoNotDriftRunningRequests is a regression test for
// a production incident where a pod's gateway-local running-request counter drifted
// permanently negative after enough requests completed while the pod was being
// repeatedly deleted/re-added by the informer's updatePod handler, which runs on
// every in-place pod update, not just genuine flaps.
//
// addPodStats/donePodStats (this file's cache.AddRequestCount/DoneRequestCount) and
// deletePodLocked/addPodLocked (informers.go) each read-then-act on the same pod key
// across two separate maps (metaPods and recentlyDeletedPods) without previously
// sharing a lock, so a delete+re-add could land in the gap and misdirect or lose a
// completing request's decrement. This race is invisible to `go test -race`: every
// individual memory access is already atomic (atomic.AddInt32 / sync.Map), so nothing
// trips the low-level race detector -- only asserting the final, business-logic-level
// count catches it. Run with -race anyway to make sure the fix's new locking itself
// introduces no genuine memory race.
//
// Every AddRequestCount below is paired with exactly one DoneRequestCount, so a
// correct implementation must always settle back to exactly zero -- concurrently with
// a goroutine continuously flapping the same pod key -- regardless of how many
// requester goroutines and delete/re-add cycles interleave.
//
// The final counter value alone cannot catch every regression here: donePodStats
// decrements through decrementClamped, which floors at 0, so a bug that issues one
// extra decrement per lost pairing would settle at the same visible 0 as a correctly
// balanced run -- silently masked by the very floor meant to keep the incident's
// symptom (a negative count) from reaching production. clampedRunningRequestDecrements
// is the unmasked signal: it counts every time the floor actually had to fire, which
// should be exactly zero here since no decrement is ever unpaired.
func TestConcurrentRequestsAndPodFlapDoNotDriftRunningRequests(t *testing.T) {
	const (
		modelName         = "test-model"
		namespace         = "default"
		podName           = "decode-0"
		podIP             = "10.0.0.1"
		numRequesters     = 20
		itersPerRequester = 200
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)

	clampedBefore := atomic.LoadInt64(&clampedRunningRequestDecrements)

	stopFlapping := make(chan struct{})
	var flapWG sync.WaitGroup
	flapWG.Add(1)
	go func() {
		defer flapWG.Done()
		for {
			select {
			case <-stopFlapping:
				return
			default:
				cache.deletePod(requestTrackerTestPod(podName, namespace, modelName, podIP))
				cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
			}
		}
	}()

	var reqWG sync.WaitGroup
	for r := 0; r < numRequesters; r++ {
		reqWG.Add(1)
		go func(r int) {
			defer reqWG.Done()
			for i := 0; i < itersPerRequester; i++ {
				requestID := fmt.Sprintf("req-%d-%d", r, i)
				routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
				routingCtx.SetTargetPod(pod)
				traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
				cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
			}
		}(r)
	}
	reqWG.Wait()
	close(stopFlapping)
	flapWG.Wait()

	// Whatever object (live pod, or a snapshot pending re-add) currently represents
	// the pod key holds the ground truth.
	key := namespace + "/" + podName
	var finalCount int32
	if metaPod, ok := cache.metaPods.Load(key); ok {
		finalCount = atomic.LoadInt32(&metaPod.runningRequests)
	} else if snap, ok := cache.recentlyDeletedPods.Load(key); ok {
		finalCount = atomic.LoadInt32(&snap.runningRequests)
	} else {
		t.Fatal("pod key missing from both metaPods and recentlyDeletedPods after the flap loop stopped")
	}

	assert.GreaterOrEqual(t, finalCount, int32(0), "running-request count must never go negative")
	assert.Equal(t, int32(0), finalCount,
		"every AddRequestCount was matched by a DoneRequestCount, so the running-request "+
			"count must settle back to exactly zero even under concurrent pod flapping -- a "+
			"nonzero result means a completion's decrement was lost or misdirected by a race "+
			"against the concurrent delete/re-add cycle")

	clampedAfter := atomic.LoadInt64(&clampedRunningRequestDecrements)
	assert.Equal(t, clampedBefore, clampedAfter,
		"decrementClamped must never hit its floor when every AddRequestCount is matched "+
			"by exactly one DoneRequestCount -- a nonzero delta means an extra decrement "+
			"occurred and was silently floored to 0 instead of surfacing as the negative "+
			"count that finalCount alone can no longer catch once clamping is in place; "+
			"this is exactly the production incident this test guards against")
}

// BenchmarkAddDoneRequestCountWithPodFlap is the same shape as
// TestConcurrentRequestsAndPodFlapDoNotDriftRunningRequests -- concurrent
// AddRequestCount/DoneRequestCount pairs racing a continuous pod delete/re-add
// flap -- but driven by the standard go test -bench harness instead of a fixed
// iteration count, so it can be run at whatever load -benchtime/-cpu ask for
// to hunt for the race under more (or less) contention than a fixed-size test
// loop provides.
//
// It doubles as a correctness check, not just a timing measurement: ns/op
// here is secondary (the flap goroutine competes for CPU with the timed loop,
// so throughput numbers are noisy) and the real signal is whether the
// benchmark fails at all. After all b.N pairs finish, it asserts the
// running-request count has settled back to exactly zero and that
// decrementClamped never had to floor a stray decrement -- see
// TestConcurrentRequestsAndPodFlapDoNotDriftRunningRequests's doc comment for
// why both checks are needed (the clamp added by this fix would otherwise
// mask a regression that reintroduces an extra decrement behind a
// business-logic-level 0 that looks identical to a correctly balanced run).
//
// Run with -race to check the locking itself introduces no data race, and
// scale -benchtime / -cpu to vary how much pressure the flap puts on the
// counter:
//
//	go test ./pkg/cache/ -run '^$' -bench BenchmarkAddDoneRequestCountWithPodFlap -benchtime=200000x -race
func BenchmarkAddDoneRequestCountWithPodFlap(b *testing.B) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "pod-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)

	clampedBefore := atomic.LoadInt64(&clampedRunningRequestDecrements)

	stopFlapping := make(chan struct{})
	var flapWG sync.WaitGroup
	flapWG.Add(1)
	go func() {
		defer flapWG.Done()
		for {
			select {
			case <-stopFlapping:
				return
			default:
				cache.deletePod(requestTrackerTestPod(podName, namespace, modelName, podIP))
				cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
			}
		}
	}()

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := atomic.AddInt64(&counter, 1)
			requestID := fmt.Sprintf("bench-req-%d", id)
			routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
			routingCtx.SetTargetPod(pod)
			traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)
			cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
		}
	})
	b.StopTimer()

	close(stopFlapping)
	flapWG.Wait()

	// Whatever object (live pod, or a snapshot pending re-add) currently
	// represents the pod key holds the ground truth.
	key := namespace + "/" + podName
	var finalCount int32
	if metaPod, ok := cache.metaPods.Load(key); ok {
		finalCount = atomic.LoadInt32(&metaPod.runningRequests)
	} else if snap, ok := cache.recentlyDeletedPods.Load(key); ok {
		finalCount = atomic.LoadInt32(&snap.runningRequests)
	} else {
		b.Fatal("pod key missing from both metaPods and recentlyDeletedPods after the flap loop stopped")
	}

	if finalCount != 0 {
		b.Fatalf("running-request count drifted to %d after %d paired Add/Done calls under concurrent "+
			"pod flapping (want 0) -- a completion's decrement was lost or misdirected by a race against "+
			"the concurrent delete/re-add cycle", finalCount, b.N)
	}

	if clampedAfter := atomic.LoadInt64(&clampedRunningRequestDecrements); clampedAfter != clampedBefore {
		b.Fatalf("decrementClamped floored %d stray decrement(s) during the benchmark -- an extra "+
			"decrement occurred and was silently clamped to 0 instead of surfacing as a negative count",
			clampedAfter-clampedBefore)
	}
}

// TestRetriedRequestReusingIDDoesNotLeakPodStats covers a retried request that
// reuses the original requestID while the first attempt is still in flight
// (e.g. the client retries after its own timeout, or the upstream engine
// rejects the retry as "already running"). Before podStatsAttemptKey, the
// second addPodStats call would silently overwrite the first attempt's
// c.podStats entry, permanently leaking its pod-stats increment once the
// first attempt eventually completed and found nothing to clean up.
func TestRetriedRequestReusingIDDoesNotLeakPodStats(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		requestID = "request-retried-same-id"
	)

	cache := NewForTest()
	podA := requestTrackerTestPod("decode-a", namespace, modelName, "10.0.0.1")
	podB := requestTrackerTestPod("decode-b", namespace, modelName, "10.0.0.2")
	cache.addPod(podA)
	cache.addPod(podB)
	metaPodA, ok := cache.metaPods.Load(namespace + "/decode-a")
	assert.True(t, ok)
	metaPodB, ok := cache.metaPods.Load(namespace + "/decode-b")
	assert.True(t, ok)

	// Attempt A routes to podA.
	ctxA := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	ctxA.SetTargetPod(podA)
	traceTermA := cache.AddRequestCount(ctxA, requestID, modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&metaPodA.runningRequests))

	// Attempt B is a retry reusing the same requestID, routed to podB while A
	// is still in flight.
	ctxB := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	ctxB.SetTargetPod(podB)
	traceTermB := cache.AddRequestCount(ctxB, requestID, modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&metaPodB.runningRequests))
	// Two attempt keys plus the nil-ctx alias held by A (LoadOrStore first-wins).
	assert.Equal(t, 3, cache.podStats.Len(), "both attempts should have their own tracked entry")

	// B finishes first (out of order) -- must decrement podB, not podA.
	cache.DoneRequestTrace(ctxB, requestID, modelName, 10, 20, traceTermB)
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPodB.runningRequests))
	assert.Equal(t, int32(1), atomic.LoadInt32(&metaPodA.runningRequests), "A's entry must survive B's cleanup")
	assert.Equal(t, 2, cache.podStats.Len())

	// A finishes -- must still find and decrement its own entry, not leak it.
	cache.DoneRequestTrace(ctxA, requestID, modelName, 10, 20, traceTermA)
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPodA.runningRequests))
	assert.Equal(t, 0, cache.podStats.Len())
}

// TestAddRequestCountDuringDeleteAddGapIncrementsPendingSnapshot is the
// counterpart of TestDoneRequestCountDuringDeleteAddGapDecrementsPendingSnapshot:
// a request that *starts* inside the delete/re-add window must increment the
// pending snapshot (and store podStats) instead of returning as "can't find
// routing pod". Otherwise the request is invisible to least-request scoring
// for its whole lifetime, and the matching Done is a no-op.
func TestAddRequestCountDuringDeleteAddGapIncrementsPendingSnapshot(t *testing.T) {
	const (
		modelName   = "test-model"
		namespace   = "default"
		podName     = "decode-0"
		requestID   = "request-add-during-gap"
		podIP       = "10.0.0.1"
		pendingLoad = 0.25
	)

	cache := NewForTest()
	cache.pendingLoadProvider = gapTestLoadProvider{load: pendingLoad}

	oldPod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(oldPod)
	key := namespace + "/" + podName

	cache.deletePod(oldPod)
	_, ok := cache.metaPods.Load(key)
	assert.False(t, ok, "pod should be absent from the cache mid-gap")

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(oldPod)
	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)

	snap, ok := cache.recentlyDeletedPods.Load(key)
	assert.True(t, ok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&snap.runningRequests),
		"a request that starts mid-gap should increment the pending snapshot in place")
	assert.Equal(t, pendingLoad, snap.pendingLoadUtilization.Load(),
		"a request that starts mid-gap should apply pending-load to the snapshot")

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	newMetaPod, ok := cache.metaPods.Load(key)
	assert.True(t, ok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&newMetaPod.runningRequests),
		"re-add should resume the mid-gap increment instead of dropping the request")
	assert.Equal(t, pendingLoad, newMetaPod.pendingLoadUtilization.Load(),
		"re-add should resume the mid-gap pending-load")

	cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests))
	assert.Equal(t, 0.0, newMetaPod.pendingLoadUtilization.Load())
	assert.Equal(t, 0, cache.podStats.Len())
}

// TestDoneRequestCountAfterGracePeriodExpiryDoesNotDecrementNewGeneration
// extends TestAddPodAfterDeleteGracePeriodExpiryDoesNotPreserveRunningRequests:
// after the grace period a same-IP re-add is a new statsGeneration, so a
// stale Done of a pre-delete request must not decrement the new object --
// including when the new generation already has its own in-flight requests.
func TestDoneRequestCountAfterGracePeriodExpiryDoesNotDecrementNewGeneration(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)
	key := namespace + "/" + podName
	oldMetaPod, ok := cache.metaPods.Load(key)
	assert.True(t, ok)

	oldCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", "request-before-expiry", "")
	oldCtx.SetTargetPod(pod)
	oldTerm := cache.AddRequestCount(oldCtx, "request-before-expiry", modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&oldMetaPod.runningRequests))

	cache.deletePod(pod)
	snap, ok := cache.recentlyDeletedPods.Load(key)
	assert.True(t, ok)
	snap.deletedAt = snap.deletedAt.Add(-recentlyDeletedPodGracePeriod)
	cache.recentlyDeletedPods.Store(key, snap)

	cache.addPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	newMetaPod, ok := cache.metaPods.Load(key)
	assert.True(t, ok)
	assert.NotSame(t, oldMetaPod, newMetaPod)
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests))
	assert.NotEqual(t, oldMetaPod.statsGeneration, newMetaPod.statsGeneration)

	newCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", "request-after-expiry", "")
	newCtx.SetTargetPod(requestTrackerTestPod(podName, namespace, modelName, podIP))
	newTerm := cache.AddRequestCount(newCtx, "request-after-expiry", modelName)
	assert.Equal(t, int32(1), atomic.LoadInt32(&newMetaPod.runningRequests))

	clampedBefore := atomic.LoadInt64(&clampedRunningRequestDecrements)
	cache.DoneRequestCount(oldCtx, "request-before-expiry", modelName, oldTerm)
	assert.Equal(t, int32(1), atomic.LoadInt32(&newMetaPod.runningRequests),
		"a stale completion after grace expiry must not decrement the new generation")
	assert.Equal(t, clampedBefore, atomic.LoadInt64(&clampedRunningRequestDecrements),
		"the stale completion must land on the orphaned object, not clamp the new generation")

	cache.DoneRequestCount(newCtx, "request-after-expiry", modelName, newTerm)
	assert.Equal(t, int32(0), atomic.LoadInt32(&newMetaPod.runningRequests))
}

// TestConcurrentDoneOfRetriedRequestDoesNotLeakPodStats covers the race
// TestRetriedRequestReusingIDDoesNotLeakPodStats cannot: both attempts of a
// reused requestID completing at the same time. The old LoadAndDelete +
// put-back of the shared requestID key could drop the original's record in
// that hole and leak its increment.
func TestConcurrentDoneOfRetriedRequestDoesNotLeakPodStats(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		iters     = 500
	)

	cache := NewForTest()
	podA := requestTrackerTestPod("decode-a", namespace, modelName, "10.0.0.1")
	podB := requestTrackerTestPod("decode-b", namespace, modelName, "10.0.0.2")
	cache.addPod(podA)
	cache.addPod(podB)
	metaPodA, ok := cache.metaPods.Load(namespace + "/decode-a")
	assert.True(t, ok)
	metaPodB, ok := cache.metaPods.Load(namespace + "/decode-b")
	assert.True(t, ok)

	clampedBefore := atomic.LoadInt64(&clampedRunningRequestDecrements)

	for i := 0; i < iters; i++ {
		requestID := fmt.Sprintf("request-retried-concurrent-%d", i)
		ctxA := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
		ctxA.SetTargetPod(podA)
		ctxB := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
		ctxB.SetTargetPod(podB)
		termA := cache.AddRequestCount(ctxA, requestID, modelName)
		termB := cache.AddRequestCount(ctxB, requestID, modelName)

		var wg sync.WaitGroup
		wg.Add(2)
		go func(ctx *types.RoutingContext, requestID string, term int64) {
			defer wg.Done()
			cache.DoneRequestCount(ctx, requestID, modelName, term)
		}(ctxA, requestID, termA)
		go func(ctx *types.RoutingContext, requestID string, term int64) {
			defer wg.Done()
			cache.DoneRequestCount(ctx, requestID, modelName, term)
		}(ctxB, requestID, termB)
		wg.Wait()
	}

	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPodA.runningRequests),
		"every attempt on podA must have been decremented")
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPodB.runningRequests),
		"every attempt on podB must have been decremented")
	assert.Equal(t, 0, cache.podStats.Len())
	assert.Equal(t, clampedBefore, atomic.LoadInt64(&clampedRunningRequestDecrements),
		"no extra decrement should have been clamped away")
}
