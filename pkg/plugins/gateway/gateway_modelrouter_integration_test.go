//go:build integration

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

package gateway

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/modelrouter"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayapi "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// TestModelRouterGatewayNamespace checks the producer and consumer against the
// same API server. Envoy is not part of envtest, so only its status is supplied
// by the test; the HTTPRoute itself must be created by the real ModelRouter.
func TestModelRouterGatewayNamespace(t *testing.T) {
	cache.InitForTest()
	// Exercise ordinary HTTPRoute routing, without a header or environment algorithm.
	previousStrategy, previousEnabled := defaultRoutingStrategy, defaultRoutingStrategyEnabled
	defaultRoutingStrategy, defaultRoutingStrategyEnabled = "", false
	t.Cleanup(func() { defaultRoutingStrategy, defaultRoutingStrategyEnabled = previousStrategy, previousEnabled })
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd/bases",
			"../../../test/integration/controller/testdata/crd",
		},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, modelv1alpha1.AddToScheme,
		orchestrationv1alpha1.AddToScheme, gatewayv1.Install, gatewayv1beta1.Install,
	} {
		require.NoError(t, add(scheme))
	}
	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)
	gatewayClient, err := gatewayapi.NewForConfig(restConfig)
	require.NoError(t, err)
	ctx := context.Background()
	for _, namespace := range []string{"aibrix-system", "custom-system", "models"} {
		require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	}
	for _, tt := range []struct {
		name, configured, podNamespace, want string
		unset                                bool
	}{
		{name: "default", podNamespace: "aibrix-system", want: "aibrix-system", unset: true},
		{name: "relocated", podNamespace: "relocated-plugin", want: "aibrix-system", unset: true},
		{name: "empty", podNamespace: "relocated-plugin", want: "aibrix-system"},
		{name: "custom", configured: "custom-system", podNamespace: "relocated-plugin", want: "custom-system"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AIBRIX_GATEWAY_NAMESPACE", tt.configured)
			if tt.unset {
				require.NoError(t, os.Unsetenv("AIBRIX_GATEWAY_NAMESPACE"))
			}
			t.Setenv("POD_NAMESPACE", tt.podNamespace)
			mgr, err := manager.New(restConfig, manager.Options{
				Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
			})
			require.NoError(t, err)
			require.NoError(t, modelrouter.Add(mgr, config.NewRuntimeConfig(false, false, "")))
			managerCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- mgr.Start(managerCtx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Error("manager did not stop")
				}
			})
			require.True(t, mgr.GetCache().WaitForCacheSync(managerCtx))
			model := "namespace-" + tt.name
			labels := map[string]string{constants.ModelLabelName: model, constants.ModelLabelPort: "8000"}
			workload := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: model, Namespace: "models", Labels: labels},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "unused"}}},
					},
				},
			}
			require.NoError(t, k8sClient.Create(ctx, workload))
			t.Cleanup(func() { require.NoError(t, k8sClient.Delete(ctx, workload)) })
			var route gatewayv1.HTTPRoute
			key := client.ObjectKey{Namespace: tt.want, Name: utils.ModelRouterName(model)}
			require.Eventually(t, func() bool { return k8sClient.Get(ctx, key, &route) == nil },
				10*time.Second, 50*time.Millisecond)
			require.Equal(t, gatewayv1.Namespace(tt.want), *route.Spec.ParentRefs[0].Namespace)
			require.Equal(t, gatewayv1.Namespace("models"), *route.Spec.Rules[0].BackendRefs[0].Namespace)
			route.Status.Parents = []gatewayv1.RouteParentStatus{{
				ParentRef: route.Spec.ParentRefs[0], ControllerName: "gateway.envoyproxy.io/gatewayclass-controller",
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.Now()},
					{Type: "ResolvedRefs", Status: metav1.ConditionTrue, Reason: "ResolvedRefs", LastTransitionTime: metav1.Now()},
				},
			}}
			require.NoError(t, k8sClient.Status().Update(ctx, &route))
			// Supply a ready backend in the discovery cache; envtest has no kubelet.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: model, Namespace: "models"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning, PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				},
			}
			server := NewServerWithOptions(nil, nil, gatewayClient, ServerOptions{
				Cache: cache.NewWithPodsForTest([]*corev1.Pod{pod}, model), DisableRateLimiting: true,
			})
			t.Cleanup(server.Shutdown)
			request := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{Body: []byte(fmt.Sprintf(
					`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model))},
			}}
			routingCtx := types.NewRoutingContext(ctx, "", "", "", model, "")
			routingCtx.ReqPath = PathChatCompletions
			response, gotModel, _, _ := server.HandleRequestBody(ctx, routingCtx, model, request, utils.User{})
			require.Nil(t, response.GetImmediateResponse(), "unexpected rejection: %v", response)
			require.NotNil(t, response.GetRequestBody())
			require.Equal(t, model, gotModel)
			require.Empty(t, routingCtx.Algorithm)
			_, validated := server.httprouteCache.Load(model)
			require.True(t, validated, "the default request path must validate the controller-created route")
		})
	}
}
