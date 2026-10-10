/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildExternalRouterErrorResponse(t *testing.T) {
	ctx := types.NewRoutingContext(context.Background(), routing.RouterExternal, "model", "", "req", "")
	tests := []struct {
		name   string
		err    error
		status envoyTypePb.StatusCode
		code   string
	}{
		{name: "denied", err: routing.ErrExternalPolicyDenied, status: envoyTypePb.StatusCode_Forbidden, code: ErrorCodeExternalPolicyDenied},
		{name: "unavailable", err: fmt.Errorf("%w: upstream secret must stay hidden", routing.ErrExternalRouterUnavailable), status: envoyTypePb.StatusCode_ServiceUnavailable, code: ErrorCodeExternalRouterUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, ok := buildExternalRouterErrorResponse(ctx, "req", tt.err)
			require.True(t, ok)
			require.Equal(t, tt.status, response.GetImmediateResponse().GetStatus().GetCode())
			body := response.GetImmediateResponse().GetBody()
			require.Contains(t, body, tt.code)
			require.NotContains(t, body, "upstream secret")
		})
	}
}

type singleCandidatePolicyRouter struct {
	bypass bool
	called bool
}

func (r *singleCandidatePolicyRouter) BypassSingleCandidate() bool { return r.bypass }

func (r *singleCandidatePolicyRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	r.called = true
	ctx.SetTargetPod(pods.All()[0])
	ctx.SetTargetPort(8000)
	return ctx.TargetAddress(), nil
}

func TestSelectTargetPodHonorsSingleCandidatePolicy(t *testing.T) {
	algorithm := types.RoutingAlgorithm("single-candidate-policy-test")
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-a",
			Labels:    map[string]string{"model.aibrix.ai/port": "8000"},
		},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}

	for _, tt := range []struct {
		name       string
		bypass     bool
		wantCalled bool
	}{
		{name: "advisory bypass", bypass: true, wantCalled: false},
		{name: "authoritative evaluation", bypass: false, wantCalled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policyRouter := &singleCandidatePolicyRouter{bypass: tt.bypass}
			manager := routing.NewRouterManager()
			manager.RegisterProvider(algorithm, func(*types.RoutingContext) (types.Router, error) {
				return policyRouter, nil
			})
			fakeCache := &MockCache{}
			server := &Server{cache: fakeCache, routerManager: manager}
			routeCtx := types.NewRoutingContext(context.Background(), algorithm, "model", "", "req", "")
			address, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")
			require.NoError(t, err)
			require.Equal(t, "10.0.0.1:8000", address)
			require.Equal(t, tt.wantCalled, policyRouter.called)
		})
	}
}

func TestExternalSelectionAdmissionRejectsWithoutSecondDecision(t *testing.T) {
	var decisionCalls atomic.Int32
	decisionService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		decisionCalls.Add(1)
		w.Header().Set("Content-Type", "application/vnd.aibrix.external-routing+json;version=v1alpha1")
		_, _ = fmt.Fprintf(w, `{"apiVersion":"routing.aibrix.ai/v1alpha1","kind":"ReplicaSelectionResponse","metadata":{"requestId":%q},"status":{"decision":"Selected","target":{"id":"ns/a","port":8000}}}`, req.Header.Get("X-Request-Id"))
	}))
	t.Cleanup(decisionService.Close)

	t.Setenv(routing.EnvExternalRouterEndpoint, decisionService.URL)
	t.Setenv(routing.EnvExternalRouterPolicyMode, "Authoritative")
	t.Setenv(routing.EnvExternalRouterFailureMode, "FailClosed")
	t.Setenv(routing.EnvExternalRouterFallback, "")
	t.Setenv(routing.EnvExternalRouterTimeout, "1s")
	t.Setenv(routing.EnvExternalRouterCandidateAttributes, "")
	t.Setenv(routing.EnvExternalRouterCandidateMetrics, "")
	t.Setenv(routing.EnvExternalRouterPolicyAttributes, "")
	t.Setenv(routing.EnvExternalRouterAuthTokenFile, "")

	mockCache := &MockCache{}
	pod := podWithReplicaInflight("a", 1)
	pod.Labels = map[string]string{constants.ModelLabelPort: "8000"}
	podList := &utils.PodArray{Pods: []*v1.Pod{pod}}
	mockCache.On("HasModel", "test-model").Return(true).Once()
	mockCache.On("ListPodsByModel", "test-model").Return(podList, nil).Once()
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0}, nil)
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(false, nil).Once()

	externalRouter, err := routing.NewExternalRouterWithCache(mockCache)
	require.NoError(t, err)
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterExternal, func(*types.RoutingContext) (types.Router, error) {
		return externalRouter, nil
	})
	server := &Server{
		cache:            mockCache,
		routerManager:    manager,
		modelRateLimiter: ratelimiter.NewNoopRateLimiter(),
	}

	request := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"test"}]}`),
			},
		},
	}
	routingCtx := types.NewRoutingContext(context.Background(), routing.RouterExternal, "", "", "req-admission-reject", "user")
	routingCtx.ReqPath = PathChatCompletions
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(routing.RouterExternal)

	response, _, _, term := server.HandleRequestBody(context.Background(), routingCtx, routingCtx.RequestID, request, utils.User{Name: "user"})
	immediate := response.GetImmediateResponse()
	require.NotNil(t, immediate)
	require.Equal(t, envoyTypePb.StatusCode_TooManyRequests, immediate.GetStatus().GetCode())
	require.Contains(t, immediate.GetBody(), ErrorCodeReplicaInflightExceeded)
	require.Equal(t, int64(0), term)
	require.Same(t, pod, routingCtx.TargetPod())
	require.False(t, routingCtx.ReplicaInflightAdmitted)
	require.Equal(t, int32(1), decisionCalls.Load(), "admission rejection must not trigger another external decision")
	mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
	mockCache.AssertExpectations(t)
}
