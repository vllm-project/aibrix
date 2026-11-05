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

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podWithReplicaInflight returns a pod whose model.aibrix.ai/config annotation sets
// requestsInflight on its default profile.
func podWithReplicaInflight(name string, inflight int64) *v1.Pod {
	return podWithReplicaLimits(name, 0, inflight)
}

// podWithReplicaLimits returns a pod whose model.aibrix.ai/config annotation sets
// requestsPerSecondPerReplica and/or requestsInflight on its default profile. Neither field
// has an env-var form: both are configured directly in the profile.
func podWithReplicaLimits(name string, rps float64, inflight int64) *v1.Pod {
	profile := map[string]any{}
	if rps > 0 {
		profile["requestsPerSecondPerReplica"] = rps
	}
	if inflight > 0 {
		profile["requestsInflight"] = inflight
	}
	var annotations map[string]string
	if len(profile) > 0 {
		cfg, err := json.Marshal(map[string]any{
			"defaultProfile": "default",
			"profiles":       map[string]any{"default": profile},
		})
		if err != nil {
			panic(err)
		}
		annotations = map[string]string{constants.ModelAnnoConfig: string(cfg)}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Annotations: annotations},
		Status: v1.PodStatus{
			PodIP:      "10.0.0." + name,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestApplyConfigProfile_ReplicaInflight(t *testing.T) {
	t.Run("inflight only", func(t *testing.T) {
		routingCtx := &types.RoutingContext{}
		applyConfigProfile(routingCtx, []*v1.Pod{podWithReplicaInflight("a", 4)})
		assert.Equal(t, int64(4), routingCtx.ConfigProfile.RequestsInflight)
		assert.Equal(t, int64(0), routingCtx.ConfigProfile.RequestsPerSecond)
		// Regression: without a forced routing strategy, HandleRequestBody resolves
		// RouterNotSet and never calls selectTargetPod, so the cap set above would
		// never actually be enforced. least-request must be forced here exactly as
		// it is for requestsPerSecondPerReplica.
		assert.Equal(t, string(routing.RouterLeastRequest), routingCtx.ConfigProfile.RoutingStrategy)
	})

	t.Run("inflight below replica rps is kept as configured, not raised", func(t *testing.T) {
		// Regression: requestsInflight and requestsPerSecondPerReplica are independent
		// limits (e.g. "at most 3 concurrent, and also no more than 5 rps"). Raising
		// inflight to make the RPS ceiling reachable would silently delete the
		// concurrency cap the user configured.
		routingCtx := &types.RoutingContext{}
		applyConfigProfile(routingCtx, []*v1.Pod{podWithReplicaLimits("a", 5, 3)})
		assert.Equal(t, int64(3), routingCtx.ConfigProfile.RequestsInflight)
		assert.Equal(t, int64(5), routingCtx.ConfigProfile.RequestsPerSecond)
		assert.Equal(t, string(routing.RouterLeastRequest), routingCtx.ConfigProfile.RoutingStrategy)
	})

	t.Run("inflight at or above replica rps is kept", func(t *testing.T) {
		routingCtx := &types.RoutingContext{}
		applyConfigProfile(routingCtx, []*v1.Pod{podWithReplicaLimits("a", 5, 8)})
		assert.Equal(t, int64(8), routingCtx.ConfigProfile.RequestsInflight)
		assert.Equal(t, int64(5), routingCtx.ConfigProfile.RequestsPerSecond)
	})

	t.Run("fractional replica rps does not affect inflight of 1", func(t *testing.T) {
		routingCtx := &types.RoutingContext{}
		applyConfigProfile(routingCtx, []*v1.Pod{podWithReplicaLimits("a", 0.5, 1)})
		assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsInflight)
		assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsPerSecond)
		assert.Equal(t, int64(2), routingCtx.ConfigProfile.RateWindowSeconds)
	})
}

// TestWarnIfReplicaInflightBelowRPS only exercises the non-mutating paths directly; the
// "does not mutate" guarantee itself is covered by TestApplyConfigProfile_ReplicaInflight
// above, which is what would break if a future change reintroduced clamping.
func TestWarnIfReplicaInflightBelowRPS(t *testing.T) {
	rc := &types.RoutingContext{}
	assert.NotPanics(t, func() { warnIfReplicaInflightBelowRPS(rc, 0, 5) })
	assert.NotPanics(t, func() { warnIfReplicaInflightBelowRPS(rc, 3, 0) })
	assert.NotPanics(t, func() { warnIfReplicaInflightBelowRPS(rc, 5, 5) })
	assert.NotPanics(t, func() { warnIfReplicaInflightBelowRPS(rc, 3, 5) })
}

func TestEnforceReplicaInflight_AdmitThenReject(t *testing.T) {
	mockCache := &MockCache{}
	s := &Server{cache: mockCache}
	pod := podWithReplicaInflight("a", 1)

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()
	routingCtx := types.NewRoutingContext(context.Background(), "", "m", "", "r1", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	routingCtx.SetTargetPod(pod)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	assert.True(t, routingCtx.ReplicaInflightAdmitted,
		"a successful admission must be recorded so addPodStats does not double-increment")

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(false, false, nil).Once()
	routingCtx2 := types.NewRoutingContext(context.Background(), "", "m", "", "r2", "")
	applyConfigProfile(routingCtx2, []*v1.Pod{pod})
	routingCtx2.SetTargetPod(pod)
	resp := s.enforceReplicaInflight(context.Background(), "m", routingCtx2)
	require.NotNil(t, resp)
	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, imm.GetStatus().GetCode())
	assert.Contains(t, imm.GetBody(), ErrorTypeOverloaded)
	assert.Contains(t, imm.GetBody(), ErrorCodeReplicaInflightExceeded)
	assert.False(t, routingCtx2.ReplicaInflightAdmitted)
	mockCache.AssertExpectations(t)
}

func TestEnforceReplicaInflight_NilRedisAdmissionIsNotCounted(t *testing.T) {
	pod := podWithReplicaInflight("a", 4)
	store := cache.NewWithPodsForTest([]*v1.Pod{pod}, "m")
	s := &Server{cache: store}
	routingCtx := types.NewRoutingContext(context.Background(), "", "m", "", "fallback", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	routingCtx.SetTargetPod(pod)
	require.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	require.False(t, routingCtx.ReplicaInflightAdmitted, "a local admission has no confirmed Redis increment")
}

func TestEnforceReplicaInflight_RedisFailureUsesNormalTracking(t *testing.T) {
	mr := miniredis.RunT(t)
	var refuseDials atomic.Bool
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		// The admit script runs on its own client and connection pool, so refusing new dials
		// while it runs fails only the admit, before anything is sent.
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if refuseDials.Load() {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())
	pod := podWithReplicaInflight("a", 4)
	store := cache.InitWithPods(cache.New(client, nil, nil), []*v1.Pod{pod}, "m")
	s := &Server{cache: store}
	routingCtx := types.NewRoutingContext(context.Background(), "", "m", "", "fallback", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	routingCtx.SetTargetPod(pod)
	refuseDials.Store(true)
	require.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	refuseDials.Store(false)
	require.False(t, routingCtx.ReplicaInflightAdmitted)

	term := store.AddRequestCount(routingCtx, routingCtx.RequestID, "m")
	key := "aibrix:rrq:" + utils.GeneratePodKey(pod.Namespace, pod.Name)
	var field string
	require.Eventually(t, func() bool {
		fields, err := mr.HKeys(key)
		if err != nil || len(fields) != 1 {
			return false
		}
		field = fields[0]
		return mr.HGet(key, field) == "1"
	}, 5*time.Second, 10*time.Millisecond, "normal tracking must increment after fallback admission")
	store.DoneRequestCount(routingCtx, routingCtx.RequestID, "m", term)
	require.Eventually(t, func() bool {
		return mr.HGet(key, field) == "0"
	}, 5*time.Second, 10*time.Millisecond, "completion must restore the baseline")
}

func TestEnforceReplicaInflight_TwoPodsIndependent(t *testing.T) {
	mockCache := &MockCache{}
	s := &Server{cache: mockCache}
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	pods := []*v1.Pod{podA, podB}

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()
	ctxA := types.NewRoutingContext(context.Background(), "", "m", "", "r1", "")
	applyConfigProfile(ctxA, pods)
	ctxA.SetTargetPod(podA)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", ctxA))

	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()
	ctxB := types.NewRoutingContext(context.Background(), "", "m", "", "r2", "")
	applyConfigProfile(ctxB, pods)
	ctxB.SetTargetPod(podB)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", ctxB))

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(false, false, nil).Once()
	ctxA2 := types.NewRoutingContext(context.Background(), "", "m", "", "r3", "")
	applyConfigProfile(ctxA2, pods)
	ctxA2.SetTargetPod(podA)
	assert.NotNil(t, s.enforceReplicaInflight(context.Background(), "m", ctxA2))

	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1), mock.Anything).Return(false, false, nil).Once()
	ctxB2 := types.NewRoutingContext(context.Background(), "", "m", "", "r4", "")
	applyConfigProfile(ctxB2, pods)
	ctxB2.SetTargetPod(podB)
	assert.NotNil(t, s.enforceReplicaInflight(context.Background(), "m", ctxB2))
	mockCache.AssertExpectations(t)
}

// TestEnforceReplicaInflight_MetricLookupErrorFailsOpen covers the benign race where
// the target pod (just selected from the same cache) can't be resolved for a live
// running-request count -- there is no external dependency to fail closed on, so this
// admits the request.
func TestEnforceReplicaInflight_MetricLookupErrorFailsOpen(t *testing.T) {
	mockCache := &MockCache{}
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(false, false, assert.AnError)
	s := &Server{cache: mockCache}
	pod := podWithReplicaInflight("a", 1)
	routingCtx := types.NewRoutingContext(context.Background(), "", "m", "", "r1", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	routingCtx.SetTargetPod(pod)

	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	assert.False(t, routingCtx.ReplicaInflightAdmitted,
		"failing open on a lookup error must not be confused with a real, already-counted admission")
}

func TestFilterSaturatedReplicaInflight(t *testing.T) {
	mockCache := &MockCache{}
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	mockCache.On("GetPodsRunningRequests", []*v1.Pod{podA, podB}).
		Return(map[string]int64{"ns/a": 1, "ns/b": 0}, nil)
	s := &Server{cache: mockCache}

	kept := s.filterSaturatedReplicaInflight(nil, []*v1.Pod{podA, podB}, 1)
	require.Len(t, kept, 1)
	assert.Equal(t, "b", kept[0].Name)
}

// Test_selectTargetPod_ReplicaInflightSaturated verifies the pre-routing gate wired into
// selectTargetPod: when every ready replica is already at requestsInflight's cap,
// selectTargetPod must fail with errReplicaInflightExceeded before a router is ever
// consulted, rather than routing to (and further overloading) a saturated pod.
// routeFunc is a types.Router backed by a function, for driving routeWithEarlyAdmission.
type routeFunc func(ctx *types.RoutingContext, pods types.PodList) (string, error)

func (f routeFunc) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	return f(ctx, pods)
}

func earlyAdmissionContext(pod *v1.Pod, requestID string) *types.RoutingContext {
	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m", "", requestID, "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	return routingCtx
}

// A router that admits its target and returns it must not be admitted a second time by
// the gateway's post-routing check: the mock allows exactly one admission.
func TestRouteWithEarlyAdmission_AdmitsOnce(t *testing.T) {
	mockCache := &MockCache{}
	s := &Server{cache: mockCache}
	pod := podWithReplicaInflight("a", 1)
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()

	routingCtx := earlyAdmissionContext(pod, "r1")
	router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
		if err := ctx.AdmitTarget(pod); err != nil {
			return "", err
		}
		ctx.SetTargetPod(pod)
		return ctx.TargetAddress(), nil
	})
	_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{pod}}, 1)
	require.NoError(t, err)
	assert.True(t, routingCtx.ReplicaInflightAdmitted)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	mockCache.AssertExpectations(t)
}

// A full target fails Route with ErrTargetReplicaInflightExceeded, which the gateway turns
// into the same 429 as its own check, and nothing is released since nothing was counted.
func TestRouteWithEarlyAdmission_FullTargetIs429(t *testing.T) {
	mockCache := &MockCache{}
	s := &Server{cache: mockCache}
	pod := podWithReplicaInflight("a", 1)
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(false, false, nil).Once()

	routingCtx := earlyAdmissionContext(pod, "r1")
	routed := false
	router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
		if err := ctx.AdmitTarget(pod); err != nil {
			return "", fmt.Errorf("wrapped: %w", err)
		}
		routed = true
		return "", nil
	})
	_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{pod}}, 1)
	require.True(t, errors.Is(err, types.ErrTargetReplicaInflightExceeded))
	assert.False(t, routed)

	resp := buildTargetSelectionErrorResponse(routingCtx, "r1", "m", err)
	require.NotNil(t, resp)
	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, imm.GetStatus().GetCode())
	assert.Contains(t, imm.GetBody(), ErrorCodeReplicaInflightExceeded)
	mockCache.AssertNotCalled(t, "ReleasePodRunningRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockCache.AssertExpectations(t)
}

// A counted admission whose Route then fails (the prefill leg failed) never reaches
// AddRequestCount, so the gateway has to release it itself.
func TestRouteWithEarlyAdmission_ReleasesWhenRouteFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		counted     bool
		wantRelease bool
	}{
		{name: "counted", counted: true, wantRelease: true},
		{name: "not counted", counted: false, wantRelease: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockCache := &MockCache{}
			s := &Server{cache: mockCache}
			pod := podWithReplicaInflight("a", 1)
			mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, tc.counted, nil).Once()
			if tc.wantRelease {
				mockCache.On("ReleasePodRunningRequest", "a", "ns", mock.Anything, mock.Anything).Return().Once()
			} else {
				// Without an expectation the mock returns before recording the call, and
				// AssertNotCalled below could never fail.
				mockCache.On("ReleasePodRunningRequest", "a", "ns", mock.Anything, mock.Anything).Return().Maybe()
			}

			routingCtx := earlyAdmissionContext(pod, "r1")
			prefillErr := errors.New("prefill request failed")
			router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
				if err := ctx.AdmitTarget(pod); err != nil {
					return "", err
				}
				return "", prefillErr
			})
			_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{pod}}, 1)
			require.ErrorIs(t, err, prefillErr)
			assert.False(t, routingCtx.ReplicaInflightAdmitted)
			if !tc.wantRelease {
				mockCache.AssertNotCalled(t, "ReleasePodRunningRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
			mockCache.AssertExpectations(t)
		})
	}
}

// A router that admits a second target must not leak the first one's slot: only the last
// admitted target keeps one, whether or not the second admission succeeds.
func TestRouteWithEarlyAdmission_ReadmitReleasesPrevious(t *testing.T) {
	for _, tc := range []struct {
		name         string
		secondAdmits bool
	}{
		{name: "second admitted", secondAdmits: true},
		{name: "second full", secondAdmits: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockCache := &MockCache{}
			s := &Server{cache: mockCache}
			first, second := podWithReplicaInflight("a", 1), podWithReplicaInflight("b", 1)
			mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()
			mockCache.On("ReleasePodRunningRequest", "a", "ns", mock.Anything, mock.Anything).Return().Once()
			mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1), mock.Anything).Return(tc.secondAdmits, tc.secondAdmits, nil).Once()

			routingCtx := earlyAdmissionContext(first, "r1")
			router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
				require.NoError(t, ctx.AdmitTarget(first))
				if err := ctx.AdmitTarget(second); err != nil {
					return "", err
				}
				ctx.SetTargetPod(second)
				return ctx.TargetAddress(), nil
			})
			_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{first, second}}, 1)
			if tc.secondAdmits {
				require.NoError(t, err)
				assert.True(t, routingCtx.ReplicaInflightAdmitted, "the second target keeps its slot")
			} else {
				require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
				assert.False(t, routingCtx.ReplicaInflightAdmitted)
			}
			mockCache.AssertNumberOfCalls(t, "ReleasePodRunningRequest", 1)
			mockCache.AssertExpectations(t)
		})
	}
}

// The release must take the +1 the admission put on the shared counter back off.
func TestRouteWithEarlyAdmission_ReleaseRestoresRedisCount(t *testing.T) {
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	pod := podWithReplicaInflight("a", 1)
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
	s := &Server{cache: store}
	key := "aibrix:rrq:" + utils.GeneratePodKey(pod.Namespace, pod.Name)

	router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
		if err := ctx.AdmitTarget(pod); err != nil {
			return "", err
		}
		fields, err := mr.HKeys(key)
		require.NoError(t, err)
		require.Len(t, fields, 1)
		require.Equal(t, "1", mr.HGet(key, fields[0]), "the admission holds a slot while the prefill runs")
		return "", errors.New("prefill request failed")
	})
	routingCtx := earlyAdmissionContext(pod, "r1")
	_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{pod}}, 1)
	require.Error(t, err)

	fields, err := mr.HKeys(key)
	require.NoError(t, err)
	require.Len(t, fields, 1)
	assert.Equal(t, "0", mr.HGet(key, fields[0]), "the failed request must not keep its slot")

	// The slot is free again: the next request is admitted.
	admitted, _, err := store.AdmitPodRunningRequest("a", "ns", 1, nil)
	require.NoError(t, err)
	assert.True(t, admitted)
}

// TestRouteWithEarlyAdmission_ReleaseUnclaimsAdmissionReservation is the local-counter
// half of TestRouteWithEarlyAdmission_ReleaseRestoresRedisCount: a released early
// admission must also free the slot that admission claimed on the pod's local
// running-requests counter (cache.Store.AdmitPodRunningRequest's reservation), for both
// a confirmed (counted) admission and one that fell back to the local decision. Until
// release guarded only counted admissions, a request whose prefill failed after a
// failed-over admit consumed cap on every route retry and nothing ever undid it --
// request tracking, whose completion decrement releases a tracked request, never runs
// for a routing failure. GetPodRunningRequests observes the count either way: the live
// Redis sum while Redis answers, the local counter fallback once it does not.
func TestRouteWithEarlyAdmission_ReleaseUnclaimsAdmissionReservation(t *testing.T) {
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	pod := podWithReplicaInflight("a", 1)
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
	s := &Server{cache: store}
	podCount := func() int64 {
		count, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
		require.NoError(t, err)
		return count
	}

	routeFailingPrefill := func(requestID string, wantCounted, wantReserved bool) *types.RoutingContext {
		router := routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
			if err := ctx.AdmitTarget(pod); err != nil {
				return "", err
			}
			require.EqualValues(t, int64(1), podCount(),
				"the admitted request holds its slot while the prefill runs")
			require.Equal(t, wantCounted, ctx.ReplicaInflightAdmitted, "the counted stamp mid-route")
			require.Equal(t, wantReserved, ctx.ReplicaInflightReserved, "the reservation stamp mid-route")
			return "", errors.New("prefill request failed")
		})
		routingCtx := earlyAdmissionContext(pod, requestID)
		_, err := s.routeWithEarlyAdmission(routingCtx, router, &utils.PodArray{Pods: []*v1.Pod{pod}}, 1)
		require.Error(t, err)
		return routingCtx
	}

	// A confirmed admission: the script committed the +1 to the hash as well, both halves
	// of the slot must release.
	routingCtx := routeFailingPrefill("r1", true, true)
	require.False(t, routingCtx.ReplicaInflightReserved,
		"the release must have reset the reservation stamp for the next target")
	require.False(t, routingCtx.ReplicaInflightAdmitted)
	require.EqualValues(t, int64(0), podCount(),
		"the released request must not keep a slot on the pod's live count")

	// A failed-over admission with Redis ALIVE: close only the derived admit client,
	// so the admit script fails (closed client) and falls back locally with counted=false,
	// while the shared client and Redis server remain fully operational.
	store.CloseAdmitRedisClientForTest()
	routingCtx = routeFailingPrefill("r2", false, true)
	require.False(t, routingCtx.ReplicaInflightReserved)
	require.False(t, routingCtx.ReplicaInflightAdmitted)
	require.EqualValues(t, int64(0), podCount(),
		"the released fallback admission must not keep a slot on the pod's local count")
	require.NoError(t, redisClient.Ping(context.Background()).Err(), "Redis must remain alive")
}

// TestReplicaInflight_Lifecycle asserts that for both counted and uncounted admissions,
// as well as the admit-after-track ordering (queue router / SLO path),
// the full admit -> AddRequestCount -> DoneRequestCount lifecycle returns both the
// local counter and the Redis hash to their baseline.
func TestReplicaInflight_Lifecycle(t *testing.T) {
	t.Run("counted_lifecycle", func(t *testing.T) {
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })
		pod := podWithReplicaInflight("a", 2)
		store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
		s := &Server{cache: store}

		ctx := earlyAdmissionContext(pod, "req-counted")
		require.True(t, s.admitReplicaInflight(ctx, pod, 2))
		require.True(t, ctx.ReplicaInflightAdmitted)
		require.True(t, ctx.ReplicaInflightReserved)

		// AddRequestCount: skips Redis increment because ReplicaInflightAdmitted=true
		ctx.SetTargetPod(pod)
		store.AddRequestCount(ctx, ctx.RequestID, "m")
		count, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)

		// DoneRequestCount: decrements both local and Redis
		store.DoneRequestCount(ctx, ctx.RequestID, "m", 0)
		countAfter, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
		require.NoError(t, err)
		require.EqualValues(t, 0, countAfter, "both local and Redis must return to 0")
	})

	t.Run("uncounted_fallback_lifecycle", func(t *testing.T) {
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })
		pod := podWithReplicaInflight("a", 2)
		store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
		s := &Server{cache: store}

		store.CloseAdmitRedisClientForTest()
		ctx := earlyAdmissionContext(pod, "req-uncounted")
		require.True(t, s.admitReplicaInflight(ctx, pod, 2))
		require.False(t, ctx.ReplicaInflightAdmitted)
		require.True(t, ctx.ReplicaInflightReserved)

		// AddRequestCount: increments Redis because ReplicaInflightAdmitted=false,
		// but skips local increment because ReplicaInflightReserved=true
		ctx.SetTargetPod(pod)
		store.AddRequestCount(ctx, ctx.RequestID, "m")
		time.Sleep(50 * time.Millisecond) // wait for async redis incr

		// DoneRequestCount: decrements both local and Redis
		store.DoneRequestCount(ctx, ctx.RequestID, "m", 0)
		time.Sleep(50 * time.Millisecond) // wait for async redis decr

		countAfter, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
		require.NoError(t, err)
		require.EqualValues(t, 0, countAfter, "both local and Redis must return to 0")
	})

	t.Run("admit_after_track_ordering", func(t *testing.T) {
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })
		pod := podWithReplicaInflight("a", 2)
		store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
		s := &Server{cache: store}

		ctx := earlyAdmissionContext(pod, "req-slo")
		ctx.SetTargetPod(pod)

		// 1. Initial track before admission (e.g. queueRouter.Route)
		store.AddRequestCount(ctx, ctx.RequestID, "m")
		time.Sleep(50 * time.Millisecond)

		// 2. Admission runs after track: skips reservation because StatsAdded() is true
		require.True(t, s.admitReplicaInflight(ctx, pod, 2))
		require.False(t, ctx.ReplicaInflightReserved, "must skip reservation when stats already added")

		// 3. Downstream AddRequestCount is a no-op dedup
		store.AddRequestCount(ctx, ctx.RequestID, "m")

		// 4. DoneRequestCount returns counters to baseline
		store.DoneRequestCount(ctx, ctx.RequestID, "m", 0)
		time.Sleep(50 * time.Millisecond)

		countAfter, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
		require.NoError(t, err)
		require.EqualValues(t, 0, countAfter, "both local and Redis must return to baseline 0")
	})
}

// selectTargetPod gives a PD request with a requestsInflight cap the early admission, so the
// router admits its decode pod before the prefill and the post-routing check is skipped.
func Test_selectTargetPod_PDAdmitsTargetInsideRoute(t *testing.T) {
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	pods := []*v1.Pod{podA, podB}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0, "ns/b": 0}, nil)
	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()

	var admitErr error
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterPD, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
			admitErr = ctx.AdmitTarget(podB)
			ctx.SetTargetPod(podB)
			return ctx.TargetAddress(), nil
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m", "", "r1", "")
	applyConfigProfile(routingCtx, pods)
	_, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	require.NoError(t, admitErr)
	assert.True(t, routingCtx.ReplicaInflightChecked, "the PD router should have admitted its target")
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	mockCache.AssertExpectations(t)
}

func Test_selectTargetPod_ReplicaInflightSaturated(t *testing.T) {
	mockCache := &MockCache{}
	pod := podWithReplicaInflight("a", 1)
	mockCache.On("GetPodsRunningRequests", []*v1.Pod{pod}).
		Return(map[string]int64{"ns/a": 1}, nil)
	s := &Server{cache: mockCache}

	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "", "r1", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	require.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsInflight)

	_, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded))
}

func TestHandleRequestBody_ReplicaInflightAdmitThenReject(t *testing.T) {
	mockCache := &MockCache{}
	registerTestRouter(new(mockRouter))

	pod := podWithReplicaInflight("a", 1)
	podList := &utils.PodArray{Pods: []*v1.Pod{pod}}

	mockCache.On("HasModel", "test-model").Return(true)
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1)).Once()
	// First request: filterSaturatedReplicaInflight's pre-routing check (batched,
	// GetPodsRunningRequests) sees the pod below the limit, and enforceReplicaInflight's
	// post-routing atomic admission (AdmitPodRunningRequest) admits it; the request_start
	// log reads the unrelated local metric slot. Second request: filterSaturatedReplicaInflight
	// now sees the pod at the limit and rejects it before enforceReplicaInflight or
	// request_start is ever reached.
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0}, nil).Once()
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1), mock.Anything).Return(true, true, nil).Once()
	mockCache.On("GetMetricValueByPod", "a", "ns", metrics.RealtimeNumRequestsRunning).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Once()
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 1}, nil).Once()

	s := &Server{
		cache:            mockCache,
		modelRateLimiter: ratelimiter.NewNoopRateLimiter(),
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
			},
		},
	}

	first := types.NewRoutingContext(context.Background(), "", "", "", "req-1", "u")
	first.ReqPath = PathChatCompletions
	first.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	resp, _, _, term := s.HandleRequestBody(context.Background(), first, "req-1", req, utils.User{Name: "u"})
	require.Nil(t, resp.GetImmediateResponse())
	assert.Equal(t, int64(1), term)

	second := types.NewRoutingContext(context.Background(), "", "", "", "req-2", "u")
	second.ReqPath = PathChatCompletions
	second.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	resp, _, _, term = s.HandleRequestBody(context.Background(), second, "req-2", req, utils.User{Name: "u"})
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, int64(0), term)
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeReplicaInflightExceeded)
	mockCache.AssertExpectations(t)
}

func TestHandleRequestBody_ReplicaInflightNotConsumedOnRoutingFailure(t *testing.T) {
	mockCache := &MockCache{}
	mockRouter := new(mockRouter)
	registerTestRouter(mockRouter)

	pods := []*v1.Pod{podWithReplicaInflight("a", 2), podWithReplicaInflight("b", 2)}
	podList := &utils.PodArray{Pods: pods}
	mockCache.On("HasModel", "test-model").Return(true).Once()
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil).Once()
	// filterSaturatedReplicaInflight (pre-routing) and the load-imbalance gate (both inside
	// selectTargetPod) read running-request counts even though the router itself will fail
	// to pick a target below; neither call count nor per-pod value matters for this test, so
	// GetPodsRunningRequests is left unstubbed and falls back to MockCache's lenient default.
	mockRouter.On("Route", mock.Anything, mock.Anything).Return("", errors.New("route selection failed")).Once()

	s := &Server{
		cache:            mockCache,
		modelRateLimiter: ratelimiter.NewNoopRateLimiter(),
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
			},
		},
	}
	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "req-1", "u")
	routingCtx.ReqPath = PathChatCompletions
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)

	resp, _, _, term := s.HandleRequestBody(context.Background(), routingCtx, "req-1", req, utils.User{Name: "u"})
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, int64(0), term)
	mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
	mockRouter.AssertExpectations(t)
}

func TestHandleRequestBody_ReplicaInflightCoexistsWithReplicaRPS(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	registerTestRouter(new(mockRouter))
	mockCache := &MockCache{}
	pod := podWithReplicaLimits("a", 1, 2)
	podList := &utils.PodArray{Pods: []*v1.Pod{pod}}
	mockCache.On("HasModel", "test-model").Return(true)
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1)).Once()
	// Inflight cap is 2 and stays well under it on both requests -- the second request
	// must be rejected by replica RPS (cap 1), not by inflight. GetPodsRunningRequests /
	// AdmitPodRunningRequest are left unstubbed and fall back to MockCache's lenient
	// admit-by-default, which is comfortably under the cap of 2 either way.
	mockCache.On("GetMetricValueByPod", "a", "ns", metrics.RealtimeNumRequestsRunning).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil)

	s := &Server{
		cache:            mockCache,
		modelRateLimiter: ratelimiter.NewRedisAccountRateLimiter("aibrix_model_test", client, time.Second),
	}

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
			},
		},
	}

	first := types.NewRoutingContext(context.Background(), "", "", "", "req-1", "u")
	first.ReqPath = PathChatCompletions
	first.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	resp, _, _, term := s.HandleRequestBody(context.Background(), first, "req-1", req, utils.User{Name: "u"})
	require.Nil(t, resp.GetImmediateResponse(), "first request should pass both RPS and inflight")
	assert.Equal(t, int64(1), term)

	second := types.NewRoutingContext(context.Background(), "", "", "", "req-2", "u")
	second.ReqPath = PathChatCompletions
	second.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	resp, _, _, _ = s.HandleRequestBody(context.Background(), second, "req-2", req, utils.User{Name: "u"})
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeRateLimitExceeded,
		"aggregate replica RPS of 1 should reject the second request even though inflight cap is 2")
}

// TestSelectTargetPod_SingleCandidateEmptyAddressReleasesAdmission verifies that in the single-candidate
// fast path, when a pod is admitted but its target address evaluates to empty (e.g. ModelClaim pod with
// port 0), the admission is released and both counters and routing context flags return to baseline.
func TestSelectTargetPod_SingleCandidateEmptyAddressReleasesAdmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		simulateDown bool
	}{
		{name: "counted", simulateDown: false},
		{name: "reserved_only", simulateDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })

			pod := podWithReplicaInflight("a", 2)
			pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"m"] = `{"model":"m","port":0}`

			store := cache.InitWithPods(cache.New(redisClient, nil, nil), []*v1.Pod{pod}, "m")
			if tc.simulateDown {
				store.CloseAdmitRedisClientForTest()
			}

			router := &postRouteFunc{routeFunc: func(*types.RoutingContext, types.PodList) (string, error) {
				t.Fatal("the single-candidate fast path must not call Route")
				return "", nil
			}}
			manager := routing.NewRouterManager()
			manager.RegisterProvider(routing.RouterPrefixCache, func(*types.RoutingContext) (types.Router, error) {
				return router, nil
			})
			s := &Server{cache: store, routerManager: manager}

			routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPrefixCache, "m", "", "r1", "")
			applyConfigProfile(routingCtx, []*v1.Pod{pod})

			addr, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")
			require.NoError(t, err)
			assert.Equal(t, "", addr, "target address should be empty")

			assert.False(t, routingCtx.ReplicaInflightAdmitted, "ReplicaInflightAdmitted should be reset")
			assert.False(t, routingCtx.ReplicaInflightReserved, "ReplicaInflightReserved should be reset")
			assert.EqualValues(t, 0, routingCtx.ReplicaInflightReservedGen, "ReplicaInflightReservedGen should be reset to 0")

			count, err := store.GetPodRunningRequests(pod.Name, pod.Namespace)
			require.NoError(t, err)
			assert.EqualValues(t, 0, count, "local counter should return to 0")

			key := "aibrix:rrq:" + utils.GeneratePodKey(pod.Namespace, pod.Name)
			if mr.Exists(key) {
				fields, err := mr.HKeys(key)
				require.NoError(t, err)
				if len(fields) > 0 {
					assert.Equal(t, "0", mr.HGet(key, fields[0]), "redis field should be 0")
				}
			}
		})
	}
}

// TestSelectTargetPod_EarlyAdmissionEmptyAddressReleasesAdmission verifies that in early admission,
// when a target pod is admitted by the router but its target address evaluates to empty
// (e.g. ModelClaim pod with port 0), selectTargetPod unwinds the admission and resets both counters
// and routing context flags.
func TestSelectTargetPod_EarlyAdmissionEmptyAddressReleasesAdmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		simulateDown bool
	}{
		{name: "counted", simulateDown: false},
		{name: "reserved_only", simulateDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })

			// Two pods so single-candidate bypass is not taken
			podA := podWithReplicaInflight("a", 2)
			podA.Annotations[constants.ModelClaimPodAnnotationPrefix+"m"] = `{"model":"m","port":0}`
			podB := podWithReplicaInflight("b", 2)
			pods := []*v1.Pod{podA, podB}

			store := cache.InitWithPods(cache.New(redisClient, nil, nil), pods, "m")
			if tc.simulateDown {
				store.CloseAdmitRedisClientForTest()
			}

			manager := routing.NewRouterManager()
			manager.RegisterProvider(routing.RouterPD, func(*types.RoutingContext) (types.Router, error) {
				return routeFunc(func(ctx *types.RoutingContext, _ types.PodList) (string, error) {
					if err := ctx.AdmitTarget(podA); err != nil {
						return "", err
					}
					ctx.SetTargetPod(podA)
					return ctx.TargetAddress(), nil
				}), nil
			})
			s := &Server{cache: store, routerManager: manager}

			routingCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m", "", "r1", "")
			applyConfigProfile(routingCtx, pods)

			addr, err := s.selectTargetPod(context.Background(), routingCtx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			assert.Equal(t, "", addr, "target address should be empty")

			assert.False(t, routingCtx.ReplicaInflightAdmitted, "ReplicaInflightAdmitted should be reset")
			assert.False(t, routingCtx.ReplicaInflightReserved, "ReplicaInflightReserved should be reset")
			assert.EqualValues(t, 0, routingCtx.ReplicaInflightReservedGen, "ReplicaInflightReservedGen should be reset to 0")

			count, err := store.GetPodRunningRequests(podA.Name, podA.Namespace)
			require.NoError(t, err)
			assert.EqualValues(t, 0, count, "local counter should return to 0")

			key := "aibrix:rrq:" + utils.GeneratePodKey(podA.Namespace, podA.Name)
			if mr.Exists(key) {
				fields, err := mr.HKeys(key)
				require.NoError(t, err)
				if len(fields) > 0 {
					assert.Equal(t, "0", mr.HGet(key, fields[0]), "redis field should be 0")
				}
			}
		})
	}
}
