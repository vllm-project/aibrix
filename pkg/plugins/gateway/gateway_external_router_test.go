/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package gateway

import (
	"context"
	"fmt"
	"testing"

	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/require"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
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
