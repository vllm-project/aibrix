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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(true, nil).Once()
	routingCtx := types.NewRoutingContext(context.Background(), "", "m", "", "r1", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	routingCtx.SetTargetPod(pod)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", routingCtx))
	assert.True(t, routingCtx.ReplicaInflightAdmitted,
		"a successful admission must be recorded so addPodStats does not double-increment")

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(false, nil).Once()
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

func TestEnforceReplicaInflight_TwoPodsIndependent(t *testing.T) {
	mockCache := &MockCache{}
	s := &Server{cache: mockCache}
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	pods := []*v1.Pod{podA, podB}

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(true, nil).Once()
	ctxA := types.NewRoutingContext(context.Background(), "", "m", "", "r1", "")
	applyConfigProfile(ctxA, pods)
	ctxA.SetTargetPod(podA)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", ctxA))

	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1)).Return(true, nil).Once()
	ctxB := types.NewRoutingContext(context.Background(), "", "m", "", "r2", "")
	applyConfigProfile(ctxB, pods)
	ctxB.SetTargetPod(podB)
	assert.Nil(t, s.enforceReplicaInflight(context.Background(), "m", ctxB))

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(false, nil).Once()
	ctxA2 := types.NewRoutingContext(context.Background(), "", "m", "", "r3", "")
	applyConfigProfile(ctxA2, pods)
	ctxA2.SetTargetPod(podA)
	assert.NotNil(t, s.enforceReplicaInflight(context.Background(), "m", ctxA2))

	mockCache.On("AdmitPodRunningRequest", "b", "ns", int64(1)).Return(false, nil).Once()
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
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(false, assert.AnError)
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

	kept := s.filterSaturatedReplicaInflight([]*v1.Pod{podA, podB}, 1)
	require.Len(t, kept, 1)
	assert.Equal(t, "b", kept[0].Name)
}

// Test_selectTargetPod_ReplicaInflightSaturated verifies the pre-routing gate wired into
// selectTargetPod: when every ready replica is already at requestsInflight's cap,
// selectTargetPod must fail with errReplicaInflightExceeded before a router is ever
// consulted, rather than routing to (and further overloading) a saturated pod.
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
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(true, nil).Once()
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
