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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapi "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

func TestHTTPRoutePodNamespace(t *testing.T) {
	cache.InitForTest()
	const model = "vendor/Exact-Model.v1"
	routeName := utils.ModelRouterName(model)
	tests := []struct {
		name               string
		podNamespace       string
		inferenceNamespace string
		unset              bool
		wantNamespace      string
		statusCode         int
	}{
		{name: "unset", unset: true, wantNamespace: "aibrix-system"},
		{name: "empty", wantNamespace: "aibrix-system"},
		{name: "explicit default", podNamespace: "aibrix-system", wantNamespace: "aibrix-system"},
		{name: "gateway namespace", podNamespace: "gateway-test", wantNamespace: "gateway-test"},
		{name: "different workload namespace", podNamespace: "gateway-test", inferenceNamespace: "workload-test", wantNamespace: "gateway-test"},
		{name: "missing route does not fall back", podNamespace: "gateway-test", wantNamespace: "gateway-test", statusCode: http.StatusNotFound},
		{name: "forbidden route does not fall back", podNamespace: "gateway-test", wantNamespace: "gateway-test", statusCode: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_NAMESPACE", tt.podNamespace)
			if tt.unset {
				require.NoError(t, os.Unsetenv("POD_NAMESPACE"))
			}
			// Workload placement must not select the namespace of routing objects.
			t.Setenv("AIBRIX_INFERENCE_NAMESPACE", tt.inferenceNamespace)
			if tt.inferenceNamespace == "" {
				require.NoError(t, os.Unsetenv("AIBRIX_INFERENCE_NAMESPACE"))
			}
			t.Setenv(envHTTPRouteCacheTTL, "1m")
			wantPath := "/apis/gateway.networking.k8s.io/v1/namespaces/" + tt.wantNamespace + "/httproutes/" + routeName
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, wantPath, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == wantPath && tt.statusCode != 0 {
					resource := schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "httproutes"}
					apiErr := apierrors.NewNotFound(resource, routeName)
					if tt.statusCode == http.StatusForbidden {
						apiErr = apierrors.NewForbidden(resource, routeName, nil)
					}
					apiErr.ErrStatus.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}
					w.WriteHeader(tt.statusCode)
					assert.NoError(t, json.NewEncoder(w).Encode(apiErr.Status()))
					return
				}
				// An otherwise valid route must not mask a wrong-namespace lookup.
				route := gatewayv1.HTTPRoute{
					TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
					Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{
						Parents: []gatewayv1.RouteParentStatus{{Conditions: []metav1.Condition{
							{Type: string(gatewayv1.RouteConditionAccepted), Reason: string(gatewayv1.RouteReasonAccepted), Status: metav1.ConditionTrue},
							{Type: string(gatewayv1.RouteConditionResolvedRefs), Reason: string(gatewayv1.RouteReasonResolvedRefs), Status: metav1.ConditionTrue},
						}}},
					}},
				}
				assert.NoError(t, json.NewEncoder(w).Encode(route))
			}))
			t.Cleanup(api.Close)
			client, err := gatewayapi.NewForConfig(&rest.Config{Host: api.URL})
			require.NoError(t, err)
			server := NewServerWithOptions(nil, nil, client, ServerOptions{Cache: cache.NewForTest()})
			t.Cleanup(server.Shutdown)
			// The server keeps the namespace resolved at construction, as it does
			// the route cache. Changing the environment must not mix scopes.
			t.Setenv("POD_NAMESPACE", "another-gateway")
			for range 2 {
				err = server.validateHTTPRouteStatus(context.Background(), model)
				switch tt.statusCode {
				case http.StatusNotFound:
					require.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
				case http.StatusForbidden:
					require.True(t, apierrors.IsForbidden(err), "expected Forbidden, got %v", err)
				default:
					require.NoError(t, err)
				}
			}
			assert.Equal(t, int32(1), requests.Load(), "cached lookups must not issue another request")
		})
	}
}
