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

package modelrouter

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func TestModelRouterGatewayNamespaceLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name, configured, want, workload string
	}{
		{"default", "", "aibrix-system", "models"},
		{"custom cross namespace", "custom-system", "custom-system", "models"},
		{"custom same namespace", "custom-system", "custom-system", "custom-system"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AIBRIX_GATEWAY_NAMESPACE", tt.configured)
			t.Setenv("POD_NAMESPACE", "controller-only")
			ctx := context.Background()
			const model = "test-model"
			workload := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: "model", Namespace: tt.workload, Labels: modelWorkloadLabels(model, "8000"),
			}}
			m := newEventTestRouter(t, workload)
			m.RuntimeConfig = config.NewRuntimeConfig(false, false, "")
			// Configuration is captured at startup, including during later resyncs.
			t.Setenv("AIBRIX_GATEWAY_NAMESPACE", "changed-after-startup")
			m.addRouteFromDeployment(workload)
			key := client.ObjectKey{Namespace: tt.want, Name: utils.ModelRouterName(model)}
			var route gatewayv1.HTTPRoute
			require.NoError(t, m.Get(ctx, key, &route))
			require.Equal(t, gatewayv1.Namespace(tt.want), *route.Spec.ParentRefs[0].Namespace)
			require.Equal(t, gatewayv1.Namespace(tt.workload), *route.Spec.Rules[0].BackendRefs[0].Namespace)

			grantKey := client.ObjectKey{Namespace: tt.workload, Name: fmt.Sprintf("%s-reserved-referencegrant-in-%s", tt.want, tt.workload)}
			var grant gatewayv1beta1.ReferenceGrant
			err := m.Get(ctx, grantKey, &grant)
			if tt.workload == tt.want {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				require.Equal(t, gatewayv1beta1.Namespace(tt.want), grant.Spec.From[0].Namespace)
			}

			// Resync must update the configured route rather than creating one in the default namespace.
			route.Spec.Rules = nil
			require.NoError(t, m.Update(ctx, &route))
			require.NoError(t, m.ensureHTTPRoutes(ctx))
			require.NoError(t, m.Get(ctx, key, &route))
			require.NotEmpty(t, route.Spec.Rules)
			// It must also recreate a missing route in that same namespace.
			require.NoError(t, m.Delete(ctx, &route))
			require.NoError(t, m.ensureHTTPRoutes(ctx))
			require.NoError(t, m.Get(ctx, key, &route))
			var routes gatewayv1.HTTPRouteList
			require.NoError(t, m.List(ctx, &routes))
			require.Len(t, routes.Items, 1)

			require.NoError(t, m.Delete(ctx, workload))
			m.deleteRouteFromDeployment(workload)
			require.True(t, apierrors.IsNotFound(m.Get(ctx, key, &route)))
			require.True(t, apierrors.IsNotFound(m.Get(ctx, grantKey, &grant)))
		})
	}
}
