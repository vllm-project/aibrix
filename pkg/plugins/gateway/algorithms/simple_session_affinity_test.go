/*
Copyright 2025 The Aibrix Team.

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

package routingalgorithms

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSessionAffinityRouter(t *testing.T) {
	tests := []struct {
		name                string
		reqHeaders          map[string]string
		readyPods           []*v1.Pod
		expectErr           bool
		expectPossibleAddrs []string // all valid target addresses (IP:port) that may be selected
	}{
		{
			name: "valid session ID matches ready pod",
			reqHeaders: map[string]string{
				constants.HeaderSessionID: base64.StdEncoding.EncodeToString([]byte("10.0.0.2:8000")),
			},
			readyPods: []*v1.Pod{
				newPod("pod1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod3", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.0.0.2:8000"},
		},
		{
			name:       "no session ID → fallback to any ready pod",
			reqHeaders: nil,
			readyPods: []*v1.Pod{
				newPod("pod1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
		},
		{
			name: "invalid base64 session ID → fallback",
			reqHeaders: map[string]string{
				constants.HeaderSessionID: "%%%INVALID_BASE64%%%",
			},
			readyPods: []*v1.Pod{
				newPod("a", "192.168.1.10", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("b", "192.168.1.11", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"192.168.1.10:8000", "192.168.1.11:8000"},
		},
		{
			name: "session ID points to non-existent address → fallback",
			reqHeaders: map[string]string{
				constants.HeaderSessionID: base64.StdEncoding.EncodeToString([]byte("10.99.99.99:8000")), // non-existent IP
			},
			readyPods: []*v1.Pod{
				newPod("x", "10.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("y", "10.1.1.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.1.1.1:8000", "10.1.1.2:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &sessionAffinityRouter{}

			ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
			ctx.ReqHeaders = tt.reqHeaders

			podList := newMockPodList(tt.readyPods, nil)

			addr, err := router.Route(ctx, podList)

			if tt.expectErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.NotNil(t, ctx.RespHeaders, "RespHeaders should not be nil")
			assert.Contains(t, ctx.RespHeaders, constants.HeaderSessionID, "Response must include session ID header")

			// verify the returned address is one of the expected ready pod addresses
			assert.Contains(t, tt.expectPossibleAddrs, addr, "selected address must be one of the ready pods' IP:port")

			// verify that the session ID in the response decodes to the same address
			sessionB64 := ctx.RespHeaders[constants.HeaderSessionID]
			sessionBytes, decodeErr := base64.StdEncoding.DecodeString(sessionB64)
			assert.NoError(t, decodeErr, "session ID must be valid base64")
			actualSessionAddr := string(sessionBytes)

			assert.Equal(t, addr, actualSessionAddr, "session ID must encode the same address as returned by Route()")
		})
	}
}

func TestSessionAffinity_ScoreAll(t *testing.T) {
	podA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pA", Labels: map[string]string{"model.aibrix.ai/port": "8000"}},
		Status:     v1.PodStatus{PodIP: "1.1.1.1", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
	}
	podB := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pB", Labels: map[string]string{"model.aibrix.ai/port": "8000"}},
		Status:     v1.PodStatus{PodIP: "2.2.2.2", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
	}

	podList := newMockPodList([]*v1.Pod{podA, podB}, nil)
	router, _ := NewSessionAffinityRouter()

	// Need to assert router satisfies types.PodScorer interface
	scorer, ok := router.(types.PodScorer)
	assert.True(t, ok)

	// 1. Without session ID
	ctx1 := types.NewRoutingContext(context.Background(), "test", "m1", "", "req", "")
	scores, scored, err := scorer.ScoreAll(ctx1, podList)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(scores))
	assert.Equal(t, 2, len(scored))
	for _, s := range scores {
		assert.Equal(t, float64(0), s)
	}

	// 2. With session ID targeting pA
	ctx2 := types.NewRoutingContext(context.Background(), "test", "m1", "", "req", "")
	ctx2.ReqHeaders = make(map[string]string)
	ctx2.ReqHeaders[constants.HeaderSessionID] = base64.StdEncoding.EncodeToString([]byte("1.1.1.1:8000"))

	scores, _, err = scorer.ScoreAll(ctx2, podList)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), scores[0]) // pA score should be 1
	assert.Equal(t, float64(0), scores[1]) // pB score should be 0

	// Check polarity
	assert.Equal(t, types.PolarityMost, scorer.Polarity())
}

func TestSessionAffinityPostRouteUpdateFollowsFinalTargetPod(t *testing.T) {
	podA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pA", Labels: map[string]string{"model.aibrix.ai/port": "8000"}},
		Status:     v1.PodStatus{PodIP: "1.1.1.1", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
	}
	podB := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pB", Labels: map[string]string{"model.aibrix.ai/port": "8000"}},
		Status:     v1.PodStatus{PodIP: "2.2.2.2", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
	}
	podList := newMockPodList([]*v1.Pod{podA, podB}, nil)
	router := &sessionAffinityRouter{}
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "", "req", "")
	ctx.ReqHeaders = map[string]string{
		constants.HeaderSessionID: base64.StdEncoding.EncodeToString([]byte("1.1.1.1:8000")),
	}

	err := router.PostRouteUpdate(ctx, podList, podB)
	assert.NoError(t, err)

	sessionBytes, decodeErr := base64.StdEncoding.DecodeString(ctx.RespHeaders[constants.HeaderSessionID])
	assert.NoError(t, decodeErr)
	assert.Equal(t, "2.2.2.2:8000", string(sessionBytes))
}

func TestSessionAffinityOpaqueKeyIsDeterministic(t *testing.T) {
	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podC := newPod("pod-c", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"})
	router := &sessionAffinityRouter{}

	route := func(pods []*v1.Pod) string {
		ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
		ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: "agent-session-42"}
		addr, err := router.Route(ctx, newMockPodList(pods, nil))
		assert.NoError(t, err)
		assert.NotEmpty(t, ctx.RespHeaders[constants.HeaderSessionID])
		assert.NotContains(t, ctx.RespHeaders, constants.HeaderSessionKey)
		return addr
	}

	first := route([]*v1.Pod{podA, podB, podC})
	assert.Equal(t, first, route([]*v1.Pod{podC, podA, podB}))
	assert.Equal(t, first, route([]*v1.Pod{podB, podC, podA}))
}

func TestSessionAffinityCookieTakesPrecedenceOverOpaqueKey(t *testing.T) {
	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	router := &sessionAffinityRouter{}
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{
		constants.HeaderSessionID:  base64.StdEncoding.EncodeToString([]byte("10.0.0.2:8000")),
		constants.HeaderSessionKey: "agent-session-42",
	}

	addr, err := router.Route(ctx, newMockPodList([]*v1.Pod{podA, podB}, nil))
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.2:8000", addr)
}

func TestSessionAffinityOpaqueKeyScoreAll(t *testing.T) {
	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	pods := []*v1.Pod{podA, podB}
	router := &sessionAffinityRouter{}
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: "agent-session-42"}

	scores, scored, err := router.ScoreAll(ctx, newMockPodList(pods, nil))
	assert.NoError(t, err)
	assert.Equal(t, []bool{true, true}, scored)
	assert.Equal(t, 1, int(scores[0]+scores[1]))

	selected := rendezvousPod(ctx, pods, "agent-session-42")
	if selected == podA {
		assert.Equal(t, []float64{1, 0}, scores)
	} else {
		assert.Equal(t, []float64{0, 1}, scores)
	}
}

func TestSessionAffinityRejectsOversizedOpaqueKey(t *testing.T) {
	pod := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	assert.Nil(t, rendezvousPod(ctx, []*v1.Pod{pod}, string(make([]byte, maxSessionKeyLen+1))))
}

func newTestSessionAffinityRedis(t testing.TB) (*sessionAffinityRouter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &sessionAffinityRouter{redisClient: client}, mr
}

func waitSessionKeyInRedis(t *testing.T, mr *miniredis.Miniredis, cacheKey, wantAddr string) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := mr.Get(sessionAffinityRedisKey(cacheKey))
		return err == nil && got == wantAddr
	}, 2*time.Second, 10*time.Millisecond)
}

func sessionAffinityRoute(t *testing.T, router *sessionAffinityRouter, sessionKey string, pods []*v1.Pod) string {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
	addr, err := router.Route(ctx, newMockPodList(pods, nil))
	require.NoError(t, err)
	return addr
}

func TestSessionAffinityRedisPinIsHonoredByAnotherReplica(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	pods := []*v1.Pod{
		newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	const sessionKey = "agent-run-shared"

	addr := sessionAffinityRoute(t, routerA, sessionKey, pods)
	waitSessionKeyInRedis(t, mr, sessionCacheKey("model1", sessionKey), addr)

	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	assert.Equal(t, addr, sessionAffinityRoute(t, routerB, sessionKey, pods))
	cached, _, ok := routerB.loadCachedAddr(sessionCacheKey("model1", sessionKey))
	assert.True(t, ok)
	assert.Equal(t, addr, cached)
}

func TestSessionAffinityRedisCacheHitSurvivesPodSetChange(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podC := newPod("pod-c", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"})
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")

	sessionKey, original, afterScale := sessionKeyWhoseRendezvousMoves(t, ctx, []*v1.Pod{podA, podB}, podC)
	addr := sessionAffinityRoute(t, router, sessionKey, []*v1.Pod{podA, podB})
	assert.Equal(t, original, addr)
	waitSessionKeyInRedis(t, mr, sessionCacheKey("model1", sessionKey), addr)

	assert.Equal(t, original, sessionAffinityRoute(t, router, sessionKey, []*v1.Pod{podA, podB, podC}),
		"local pin must survive a pod-set change that would move rendezvous to %s", afterScale)
}

// TestSessionAffinityScopesCacheKeyByModel is a regression test for two models sharing one
// Redis instance (or a caller re-using the same x-aibrix-session-key value across models):
// before cache keys were scoped to ctx.Model, both models' pinnings raced to overwrite the
// same Redis entry, so neither model's session ever stabilized.
func TestSessionAffinityScopesCacheKeyByModel(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const sessionKey = "shared-session-key"
	modelAPod := newPod("model-a-pod", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	modelBPod := newPod("model-b-pod", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})

	routeFor := func(model string, pods []*v1.Pod) string {
		ctx := types.NewRoutingContext(context.Background(), "test", model, "", "", "")
		ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
		addr, err := router.Route(ctx, newMockPodList(pods, nil))
		require.NoError(t, err)
		return addr
	}

	addrA := routeFor("model-a", []*v1.Pod{modelAPod})
	assert.Equal(t, "10.0.0.1:8000", addrA)
	waitSessionKeyInRedis(t, mr, sessionCacheKey("model-a", sessionKey), addrA)

	addrB := routeFor("model-b", []*v1.Pod{modelBPod})
	assert.Equal(t, "10.0.0.2:8000", addrB)
	waitSessionKeyInRedis(t, mr, sessionCacheKey("model-b", sessionKey), addrB)

	got, err := mr.Get(sessionAffinityRedisKey(sessionCacheKey("model-a", sessionKey)))
	require.NoError(t, err)
	assert.Equal(t, addrA, got, "model-b routing must not overwrite model-a's Redis entry for the same caller session key")

	assert.Equal(t, addrA, routeFor("model-a", []*v1.Pod{modelAPod}), "model-a session must still resolve to its own pinned pod")
}

// TestSessionAffinityLocalCacheHasHardCapacityBound is a regression test for the
// sessionKeyPods size cap: since x-aibrix-session-key is entirely caller-controlled, a client
// cycling through high-cardinality session keys must not be able to grow the local cache (and
// the periodic Redis sync work that scans it) without bound.
func TestSessionAffinityLocalCacheHasHardCapacityBound(t *testing.T) {
	router, _ := newTestSessionAffinityRedis(t)

	const limit = 5
	original := maxSessionKeyPodsEntries
	maxSessionKeyPodsEntries = limit
	t.Cleanup(func() { maxSessionKeyPodsEntries = original })

	pod := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	for i := 0; i < limit*20; i++ {
		sessionAffinityRoute(t, router, fmt.Sprintf("high-cardinality-%d", i), []*v1.Pod{pod})
	}

	size := 0
	router.sessionKeyPods.Range(func(_, _ any) bool {
		size++
		return true
	})
	assert.LessOrEqual(t, size, limit, "local session-key cache must not grow past the configured limit")
	assert.LessOrEqual(t, atomic.LoadInt64(&router.sessionKeyPodsSize), int64(limit))
}

func TestSessionAffinityNoRedisDoesNotPinLocally(t *testing.T) {
	router := &sessionAffinityRouter{}
	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podC := newPod("pod-c", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"})
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")

	sessionKey, original, afterScale := sessionKeyWhoseRendezvousMoves(t, ctx, []*v1.Pod{podA, podB}, podC)
	assert.Equal(t, original, sessionAffinityRoute(t, router, sessionKey, []*v1.Pod{podA, podB}))
	_, cached := router.sessionKeyPods.Load(sessionKey)
	assert.False(t, cached, "without Redis the local map must stay empty so replicas keep agreeing via rendezvous")
	assert.Equal(t, afterScale, sessionAffinityRoute(t, router, sessionKey, []*v1.Pod{podA, podB, podC}))
}

func sessionKeyWhoseRendezvousMoves(t *testing.T, ctx *types.RoutingContext, original []*v1.Pod, extra *v1.Pod) (sessionKey, beforeAddr, afterAddr string) {
	t.Helper()
	scaled := append(append([]*v1.Pod{}, original...), extra)
	for i := 0; i < 256; i++ {
		key := fmt.Sprintf("move-%d", i)
		before := rendezvousPod(ctx, original, key)
		after := rendezvousPod(ctx, scaled, key)
		require.NotNil(t, before)
		require.NotNil(t, after)
		beforeAddr = net.JoinHostPort(before.Status.PodIP, "8000")
		afterAddr = net.JoinHostPort(after.Status.PodIP, "8000")
		if beforeAddr != afterAddr {
			return key, beforeAddr, afterAddr
		}
	}
	t.Fatal("could not find a session key whose rendezvous winner moves when a pod is added")
	return "", "", ""
}

func TestSyncSessionKeyPodsFromRedis_SelfHealsUnconfirmedEntry(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const sessionKey = "unconfirmed-pin"
	const addr = "10.0.0.1:8000"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{addr: addr, confirmed: false, storedAt: time.Now()})
	_, err := mr.Get(sessionAffinityRedisKey(sessionKey))
	require.Error(t, err, "precondition: redis must not already have this key")

	router.syncSessionKeyPodsFromRedis()

	cached, _, ok := router.loadCachedAddr(sessionKey)
	require.True(t, ok, "unconfirmed entry must survive a redis miss, not be treated as an authoritative deletion")
	assert.Equal(t, addr, cached)
	item, _ := router.sessionKeyPods.Load(sessionKey)
	assert.True(t, item.(sessionKeyCacheItem).confirmed, "sync must retry the write and mark the entry confirmed once it lands")
	got, err := mr.Get(sessionAffinityRedisKey(sessionKey))
	require.NoError(t, err)
	assert.Equal(t, addr, got)
}

func TestSyncSessionKeyPodsFromRedis_EvictsConfirmedMiss(t *testing.T) {
	router, _ := newTestSessionAffinityRedis(t)
	const sessionKey = "confirmed-gone"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{addr: "10.0.0.1:8000", confirmed: true, storedAt: time.Now()})

	router.syncSessionKeyPodsFromRedis()

	_, found := router.sessionKeyPods.Load(sessionKey)
	assert.False(t, found, "confirmed local entry must be evicted once Redis no longer has it")
}

func TestSyncSessionKeyPodsFromRedis_RefreshesChangedEntry(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const sessionKey = "re-pinned"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{addr: "10.0.0.1:8000", confirmed: true, storedAt: time.Now()})
	require.NoError(t, mr.Set(sessionAffinityRedisKey(sessionKey), "10.0.0.2:8000"))

	router.syncSessionKeyPodsFromRedis()

	cached, _, ok := router.loadCachedAddr(sessionKey)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.2:8000", cached)
	item, _ := router.sessionKeyPods.Load(sessionKey)
	assert.True(t, item.(sessionKeyCacheItem).confirmed)
}

func TestSyncSessionKeyPodsFromRedis_EvictsIdleEntry(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const sessionKey = "idle-pin"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{
		addr:      "10.0.0.1:8000",
		confirmed: true,
		storedAt:  time.Now().Add(-sessionAffinityTTL - time.Second),
	})
	require.NoError(t, mr.Set(sessionAffinityRedisKey(sessionKey), "10.0.0.1:8000"))

	router.syncSessionKeyPodsFromRedis()

	_, found := router.sessionKeyPods.Load(sessionKey)
	assert.False(t, found, "idle local entry must be dropped even if Redis still has it")
}

func TestSyncSessionKeyPodsFromRedis_EvictsIdleUnconfirmedEntry(t *testing.T) {
	router, _ := newTestSessionAffinityRedis(t)
	const sessionKey = "idle-unconfirmed"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{
		addr:      "10.0.0.1:8000",
		confirmed: false,
		storedAt:  time.Now().Add(-sessionAffinityTTL - time.Second),
	})

	router.syncSessionKeyPodsFromRedis()

	_, found := router.sessionKeyPods.Load(sessionKey)
	assert.False(t, found, "expired unconfirmed entry must not be retried indefinitely")
}

func TestSyncSessionKeyPodsFromRedis_LeavesLocalCacheUntouchedOnRedisError(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	const sessionKey = "outage-pin"
	router.sessionKeyPods.Store(sessionKey, sessionKeyCacheItem{addr: "10.0.0.1:8000", confirmed: true, storedAt: time.Now()})
	require.NoError(t, mr.Set(sessionAffinityRedisKey(sessionKey), "10.0.0.1:8000"))
	mr.Close()

	router.syncSessionKeyPodsFromRedis()

	cached, _, ok := router.loadCachedAddr(sessionKey)
	require.True(t, ok, "local entry must survive a whole-batch redis error")
	assert.Equal(t, "10.0.0.1:8000", cached)
}

func TestSessionAffinityOversizedKeyIsNotPinned(t *testing.T) {
	router, mr := newTestSessionAffinityRedis(t)
	pod := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	oversized := string(make([]byte, maxSessionKeyLen+1))

	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: oversized}
	_, err := router.Route(ctx, newMockPodList([]*v1.Pod{pod}, nil))
	require.NoError(t, err)
	_, cached := router.sessionKeyPods.Load(oversized)
	assert.False(t, cached)
	_, redisErr := mr.Get(sessionAffinityRedisKey(oversized))
	assert.Error(t, redisErr)

	ctx2 := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx2.ReqHeaders = map[string]string{constants.HeaderSessionKey: oversized}
	require.NoError(t, router.PostRouteUpdate(ctx2, newMockPodList([]*v1.Pod{pod}, nil), pod))
	_, cached = router.sessionKeyPods.Load(oversized)
	assert.False(t, cached, "PostRouteUpdate (auto-blend commit path) must honor maxSessionKeyLen")
	_, redisErr = mr.Get(sessionAffinityRedisKey(oversized))
	assert.Error(t, redisErr)
}

func TestSessionAffinityPostRouteUpdateNilHeaders(t *testing.T) {
	router := &sessionAffinityRouter{}
	pod := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	ctx := types.NewRoutingContext(context.Background(), "test", "m1", "", "req", "")
	ctx.ReqHeaders = nil
	assert.NoError(t, router.PostRouteUpdate(ctx, newMockPodList([]*v1.Pod{pod}, nil), pod))
}

func TestSessionAffinityStartNilRedisIsNoop(t *testing.T) {
	router := &sessionAffinityRouter{}
	router.Start(make(chan struct{}), nil)
	assert.Nil(t, router.redisClient)
}

func TestSessionAffinityStartWiresClient(t *testing.T) {
	router, _ := newTestSessionAffinityRedis(t)
	client := router.redisClient
	router.redisClient = nil
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	router.Start(stop, client)
	assert.Same(t, client, router.redisClient)
	router.Start(stop, &redis.Client{})
	assert.Same(t, client, router.redisClient, "Start is idempotent")
}

// BenchmarkSessionAffinityRouterConcurrent hammers one router instance from many goroutines at
// once -- Route and PostRouteUpdate (the two paths that write into sessionKeyPods and Redis),
// running concurrently with the background Redis sync loop that also walks sessionKeyPods --
// while mixing a small pool of "hot" session keys (repeated pins/refreshes, exercising the CAS
// loops in storeSessionKeyLocal and markSessionKeyConfirmed) with unique, ever-growing keys
// (exercising the capacity-bound new-key path added for the sessionKeyPodsSize cap) across a
// few different models (exercising sessionCacheKey's model scoping). It exists to catch data
// races in that concurrent bookkeeping, not to produce a stable throughput number:
//
//	go test ./pkg/plugins/gateway/algorithms/ -run '^$' -bench BenchmarkSessionAffinityRouterConcurrent -race
//
// A clean run (no "WARNING: DATA RACE" output, no fatal errors) is the pass condition.
func BenchmarkSessionAffinityRouterConcurrent(b *testing.B) {
	router, _ := newTestSessionAffinityRedis(b)

	pods := make([]*v1.Pod, 8)
	for i := range pods {
		pods[i] = newPod(fmt.Sprintf("pod-%d", i), fmt.Sprintf("10.0.0.%d", i+1), true, map[string]string{"model.aibrix.ai/port": "8000"})
	}
	podList := newMockPodList(pods, nil)

	stop := make(chan struct{})
	b.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				router.syncSessionKeyPodsFromRedis()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	var errCount int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			var sessionKey string
			if i%3 == 0 {
				sessionKey = fmt.Sprintf("hot-%d", i%16)
			} else {
				sessionKey = fmt.Sprintf("unique-%d-%d", i, time.Now().UnixNano())
			}
			model := fmt.Sprintf("model-%d", i%3)

			ctx := types.NewRoutingContext(context.Background(), "test", model, "", "", "")
			ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
			if _, err := router.Route(ctx, podList); err != nil {
				atomic.AddInt64(&errCount, 1)
			}

			ctx2 := types.NewRoutingContext(context.Background(), "test", model, "", "", "")
			ctx2.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
			if err := router.PostRouteUpdate(ctx2, podList, pods[i%len(pods)]); err != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}
	})

	if n := atomic.LoadInt64(&errCount); n > 0 {
		b.Fatalf("%d unexpected errors from Route/PostRouteUpdate under concurrent load", n)
	}
}

// TestSessionAffinityClaimDoesNotOverwriteExistingPin is a regression test for the
// initial-claim race described in the #2742 review: two replicas that both miss Redis and
// resolve different pods for the same session must not overwrite each other's pin. With a
// plain SET the second replica's claim silently replaced the first one.
func TestSessionAffinityClaimDoesNotOverwriteExistingPin(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	const sessionKey = "claim-race"
	cacheKey := sessionCacheKey("model1", sessionKey)

	require.True(t, routerA.persistSessionKeyToRedis(cacheKey, "10.0.0.1:8000", writeClaim))
	assert.False(t, routerB.persistSessionKeyToRedis(cacheKey, "10.0.0.2:8000", writeClaim),
		"the losing claim must report that Redis was not written")

	got, err := mr.Get(sessionAffinityRedisKey(cacheKey))
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", got,
		"a second replica must not overwrite a pin that is already claimed")
}

// TestSessionAffinitySyncRetryDoesNotClobberAnotherReplicasPin covers the same race on the
// sync-pass retry path: an unconfirmed local entry whose write never landed must not replace
// a pin that another replica owns by the time the retry runs.
func TestSessionAffinitySyncRetryDoesNotClobberAnotherReplicasPin(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	const sessionKey = "sync-retry-race"
	cacheKey := sessionCacheKey("model1", sessionKey)

	require.True(t, routerA.persistSessionKeyToRedis(cacheKey, "10.0.0.1:8000", writeClaim))
	routerB.storeSessionKeyLocal(cacheKey, "10.0.0.2:8000", false)
	routerB.handleSessionKeyCacheSyncMiss(cacheKey)

	got, err := mr.Get(sessionAffinityRedisKey(cacheKey))
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", got,
		"the sync retry must not overwrite a pin that another replica owns")

	cached, confirmed, ok := routerB.loadCachedAddr(cacheKey)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1:8000", cached, "the losing replica must converge on the winner")
	assert.True(t, confirmed)
}

// TestSessionAffinityRouteConvergesLostClaimToWinner covers the request path end to end: a
// replica whose local pin lost the claim race must not turn its unconfirmed entry into an
// unconditional refresh, and it must converge on the winner instead of waiting for the next
// sync pass.
func TestSessionAffinityRouteConvergesLostClaimToWinner(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	const sessionKey = "route-lost-claim"
	cacheKey := sessionCacheKey("model1", sessionKey)

	pods := []*v1.Pod{
		newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
		newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
	}
	require.True(t, routerA.persistSessionKeyToRedis(cacheKey, "10.0.0.1:8000", writeClaim))

	// Replica B holds an unconfirmed local pin for pod-b: its own claim never landed.
	routerB.storeSessionKeyLocal(cacheKey, "10.0.0.2:8000", false)
	assert.Equal(t, "10.0.0.2:8000", sessionAffinityRoute(t, routerB, sessionKey, pods),
		"the in-flight request itself still follows this replica's local pick")

	require.Eventually(t, func() bool {
		cached, confirmed, ok := routerB.loadCachedAddr(cacheKey)
		return ok && confirmed && cached == "10.0.0.1:8000"
	}, 2*time.Second, 10*time.Millisecond,
		"the losing replica must adopt the winner after reading it back")

	assert.Equal(t, "10.0.0.1:8000", sessionAffinityRoute(t, routerB, sessionKey, pods),
		"the next request must follow the winning pin")
	got, err := mr.Get(sessionAffinityRedisKey(cacheKey))
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", got, "the losing claim must never overwrite the winner")
}

// TestSessionAffinityPostRouteUpdateDoesNotOverwriteExistingPin covers the paths that bypass
// Route (the single-ready-pod fast path and blended commits): they cannot tell whether Redis
// already holds a pinning, so they must claim, not refresh.
func TestSessionAffinityPostRouteUpdateDoesNotOverwriteExistingPin(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	const sessionKey = "post-route-claim"
	cacheKey := sessionCacheKey("model1", sessionKey)
	require.True(t, routerA.persistSessionKeyToRedis(cacheKey, "10.0.0.1:8000", writeClaim))

	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
	require.NoError(t, routerB.PostRouteUpdate(ctx, newMockPodList([]*v1.Pod{podB}, nil), podB))

	require.Eventually(t, func() bool {
		cached, confirmed, ok := routerB.loadCachedAddr(cacheKey)
		return ok && confirmed && cached == "10.0.0.1:8000"
	}, 2*time.Second, 10*time.Millisecond,
		"a bypass path must converge on the winner, not overwrite it")

	got, err := mr.Get(sessionAffinityRedisKey(cacheKey))
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", got, "PostRouteUpdate must not overwrite an existing pin")
}

// TestSessionAffinityPostRouteUpdateSlidesTTL covers the steady state of the post-route
// paths: the claim write keeps failing against this replica's own pin, so the read-back must
// also slide the TTL, or an actively used session expires on the idle clock.
func TestSessionAffinityPostRouteUpdateSlidesTTL(t *testing.T) {
	routerA, mr := newTestSessionAffinityRedis(t)
	routerB := &sessionAffinityRouter{redisClient: routerA.redisClient}
	const sessionKey = "post-route-ttl"
	cacheKey := sessionCacheKey("model1", sessionKey)

	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	require.True(t, routerA.persistSessionKeyToRedis(cacheKey, "10.0.0.1:8000", writeClaim))

	// Let the pin age halfway to expiry, then run one request through the post-route path.
	mr.FastForward(sessionAffinityTTL / 2)

	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: sessionKey}
	require.NoError(t, routerB.PostRouteUpdate(ctx, newMockPodList([]*v1.Pod{podA}, nil), podA))

	require.Eventually(t, func() bool {
		return mr.TTL(sessionAffinityRedisKey(cacheKey)) > 3*sessionAffinityTTL/4
	}, 2*time.Second, 10*time.Millisecond,
		"an actively used pin must not age out on the idle clock")
}

// TestSessionAffinityResolveReportsPersistIntent pins the write intent resolveSessionPod reports
// for each case, since Route/PostRouteUpdate use it to decide between the atomic claim write and
// the unconditional write (the gated refresh and repin CAS build on this distinction next).
func TestSessionAffinityResolveReportsPersistIntent(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
	ctx.ReqHeaders = map[string]string{constants.HeaderSessionKey: "write-intent"}
	cacheKey := sessionCacheKey("model1", "write-intent")

	podA := newPod("pod-a", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"})
	podB := newPod("pod-b", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"})

	router, mr := newTestSessionAffinityRedis(t)

	// Nothing pinned anywhere: the rendezvous pick creates a new pinning.
	_, _, via, mode := router.resolveSessionPod(ctx, []*v1.Pod{podA, podB})
	require.Equal(t, "rendezvous", via)
	assert.Equal(t, writeClaim, mode)

	// Redis already pins a ready pod: the resolve re-affirms it.
	require.NoError(t, mr.Set(sessionAffinityRedisKey(cacheKey), "10.0.0.1:8000"))
	_, _, via, mode = router.resolveSessionPod(ctx, []*v1.Pod{podA, podB})
	require.Equal(t, "session-key-redis", via)
	assert.Equal(t, writeRefresh, mode)

	// Redis pins a pod that is gone from this replica's ready view: the rendezvous pick replaces it.
	fresh := &sessionAffinityRouter{redisClient: router.redisClient}
	require.NoError(t, mr.Set(sessionAffinityRedisKey(cacheKey), "10.0.0.9:8000"))
	_, _, via, mode = fresh.resolveSessionPod(ctx, []*v1.Pod{podA, podB})
	require.Equal(t, "rendezvous", via)
	assert.Equal(t, writeRepin, mode)

	// A local hit whose Redis write is not known to have landed is still a claim: persisting
	// it must not use the unconditional write.
	localLoser := &sessionAffinityRouter{redisClient: router.redisClient}
	localLoser.storeSessionKeyLocal(cacheKey, "10.0.0.1:8000", false)
	_, _, via, mode = localLoser.resolveSessionPod(ctx, []*v1.Pod{podA, podB})
	require.Equal(t, "session-key-cache", via)
	assert.Equal(t, writeClaim, mode)

	// A confirmed local hit is the one case where a plain TTL refresh is safe.
	localWinner := &sessionAffinityRouter{redisClient: router.redisClient}
	localWinner.storeSessionKeyLocal(cacheKey, "10.0.0.1:8000", true)
	_, _, via, mode = localWinner.resolveSessionPod(ctx, []*v1.Pod{podA, podB})
	require.Equal(t, "session-key-cache", via)
	assert.Equal(t, writeRefresh, mode)
}
