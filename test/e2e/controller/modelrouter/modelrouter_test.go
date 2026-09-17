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

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// TestModelRouterLifecycle validates the live resource and request path without
// extending the controller's current Add/Delete event contract.
//
//nolint:gocyclo // One serial lifecycle keeps shared ReferenceGrant and restart assertions deterministic.
func TestModelRouterLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	h := newModelRouterHarness(t, ctx)
	primaryModel := "modelrouter-primary-" + h.namespace[len("modelrouter-e2e-"):]
	adapterModel := "modelrouter-adapter-" + h.namespace[len("modelrouter-e2e-"):]
	probeModel := "modelrouter-probe-" + h.namespace[len("modelrouter-e2e-"):]
	backendName := "modelrouter-backend"
	adapterName := "modelrouter-adapter"
	probeName := "modelrouter-probe"

	backendPod := h.createBackend(
		t,
		ctx,
		backendName,
		primaryModel,
		[]string{"/score", "/version"},
	)
	primaryRoute := h.waitForRouteReady(t, ctx, primaryModel)
	assertModelRoute(t, primaryRoute, h.namespace, primaryModel, backendName, []string{"/score", "/version"})
	assertReferenceGrant(t, h.waitForReferenceGrant(t, ctx))

	record := h.waitForGatewayRequest(t, ctx, primaryModel, backendPod)
	assert.Equal(t, backendPod, record.Pod)
	assert.Equal(t, "/v1/chat/completions", record.Path)

	h.createModelAdapter(t, ctx, adapterName, adapterModel, backendName)
	adapterRoute := h.waitForRouteReady(t, ctx, adapterModel)
	assertModelRoute(t, adapterRoute, h.namespace, adapterModel, backendName, nil)
	assertReferenceGrant(t, h.waitForReferenceGrant(t, ctx))

	require.NoError(t, h.modelClient.ModelV1alpha1().ModelAdapters(h.namespace).
		Delete(ctx, adapterName, metav1.DeleteOptions{}))
	h.waitForRouteDeleted(t, ctx, adapterModel)
	_ = h.waitForRouteReady(t, ctx, primaryModel)
	_ = h.waitForReferenceGrant(t, ctx)

	h.restartController(t, ctx)
	h.createRouteProbeDeployment(t, ctx, probeName, probeModel, backendName)
	_ = h.waitForRouteReady(t, ctx, probeModel)
	_ = h.waitForRouteReady(t, ctx, primaryModel)
	assert.Equal(t, 1, h.routeCount(t, ctx, primaryModel))
	require.NoError(t, h.kubeClient.AppsV1().Deployments(h.namespace).
		Delete(ctx, probeName, metav1.DeleteOptions{}))
	h.waitForRouteDeleted(t, ctx, probeModel)
	_ = h.waitForReferenceGrant(t, ctx)

	require.NoError(t, h.kubeClient.AppsV1().Deployments(h.namespace).
		Delete(ctx, backendName, metav1.DeleteOptions{}))
	h.waitForRouteDeleted(t, ctx, primaryModel)
	h.waitForReferenceGrantDeleted(t, ctx)
}

func assertModelRoute(
	t *testing.T,
	route *gatewayv1.HTTPRoute,
	backendNamespace, model, serviceName string,
	customPaths []string,
) {
	t.Helper()
	require.NotNil(t, route)
	require.Len(t, route.Spec.ParentRefs, 1)
	assert.Equal(t, gatewayv1.ObjectName(modelRouterGatewayName), route.Spec.ParentRefs[0].Name)
	require.NotNil(t, route.Spec.ParentRefs[0].Namespace)
	assert.Equal(t, gatewayv1.Namespace(modelRouterGatewayNamespace), *route.Spec.ParentRefs[0].Namespace)
	require.Len(t, route.Spec.Rules, 1)
	require.Len(t, route.Spec.Rules[0].BackendRefs, 1)
	backend := route.Spec.Rules[0].BackendRefs[0]
	assert.Equal(t, gatewayv1.ObjectName(serviceName), backend.Name)
	require.NotNil(t, backend.Namespace)
	assert.Equal(t, gatewayv1.Namespace(backendNamespace), *backend.Namespace)
	require.NotNil(t, backend.Port)
	assert.Equal(t, gatewayv1.PortNumber(modelRouterBackendPort), *backend.Port)

	paths := routePaths(route)
	assert.Contains(t, paths, "/v1/chat/completions")
	for _, path := range customPaths {
		assert.Contains(t, paths, path)
	}
	for _, match := range route.Spec.Rules[0].Matches {
		require.Len(t, match.Headers, 1)
		assert.Equal(t, gatewayv1.HTTPHeaderName("model"), match.Headers[0].Name)
		assert.Equal(t, model, match.Headers[0].Value)
		assert.Equal(t, ptr.To(gatewayv1.HeaderMatchExact), match.Headers[0].Type)
	}
}

func assertReferenceGrant(t *testing.T, grant *gatewayv1beta1.ReferenceGrant) {
	t.Helper()
	require.NotNil(t, grant)
	require.Len(t, grant.Spec.From, 1)
	assert.Equal(t, gatewayv1beta1.Group(gatewayv1.GroupName), grant.Spec.From[0].Group)
	assert.Equal(t, gatewayv1beta1.Kind("HTTPRoute"), grant.Spec.From[0].Kind)
	assert.Equal(t, gatewayv1beta1.Namespace(modelRouterGatewayNamespace), grant.Spec.From[0].Namespace)
	require.Len(t, grant.Spec.To, 1)
	assert.Equal(t, gatewayv1beta1.Group(""), grant.Spec.To[0].Group)
	assert.Equal(t, gatewayv1beta1.Kind("Service"), grant.Spec.To[0].Kind)
}

func TestHTTPRouteReady(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{name: "no conditions"},
		{
			name: "accepted only",
			conditions: []metav1.Condition{{
				Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
			}},
		},
		{
			name: "accepted and resolved",
			conditions: []metav1.Condition{
				{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue},
				{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue},
			},
			want: true,
		},
		{
			name: "resolved false",
			conditions: []metav1.Condition{
				{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue},
				{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionFalse},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{Status: gatewayv1.HTTPRouteStatus{
				RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
					Conditions: test.conditions,
				}}},
			}}
			assert.Equal(t, test.want, httpRouteReady(route))
		})
	}
}
