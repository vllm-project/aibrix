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
			assert.Equal(t, 1, cache.podStats.Len())

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
	assert.Equal(t, 1, cache.podStats.Len())

	cache.DoneRequestTrace(routingCtx, requestID, modelName, 10, 20, traceTerm)
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPod.runningRequests))
	assert.Equal(t, 0, cache.podStats.Len())

	assert.NotPanics(t, func() {
		cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
	})
	assert.Equal(t, int32(0), atomic.LoadInt32(&metaPod.runningRequests))
	assert.Equal(t, 0, cache.podStats.Len())
}
