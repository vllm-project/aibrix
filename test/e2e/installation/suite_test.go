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

package installation

import (
	"testing"

	"github.com/stretchr/testify/require"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStormServiceBuilders(t *testing.T) {
	t.Run("single instance", func(t *testing.T) {
		stormService := newSingleStormService("default", "single-smoke", "smoke-single")

		require.Equal(t, "default", stormService.Namespace)
		require.Equal(t, "single-smoke", stormService.Name)
		require.NotNil(t, stormService.Spec.Replicas)
		require.Equal(t, int32(1), *stormService.Spec.Replicas)
		require.Len(t, stormService.Spec.Template.Spec.Roles, 1)

		role := stormService.Spec.Template.Spec.Roles[0]
		require.Equal(t, "worker", role.Name)
		require.NotNil(t, role.Replicas)
		require.Equal(t, int32(1), *role.Replicas)
		require.Equal(t, "smoke-single", role.Template.Labels[modelNameLabel])
		require.Equal(t, "8000", role.Template.Labels[modelPortLabel])
		require.Len(t, role.Template.Spec.Containers, 1)
		require.Equal(t, mockImage, role.Template.Spec.Containers[0].Image)
	})

	t.Run("pd disaggregated", func(t *testing.T) {
		stormService := newPDStormService("default", "pd-smoke", "smoke-pd")

		require.Equal(t, "default", stormService.Namespace)
		require.Equal(t, "pd-smoke", stormService.Name)
		require.JSONEq(t, `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd"}}}`,
			stormService.Annotations[modelConfigAnnotation])
		require.NotNil(t, stormService.Spec.Replicas)
		require.Equal(t, int32(1), *stormService.Spec.Replicas)
		require.Len(t, stormService.Spec.Template.Spec.Roles, 2)

		roles := make(map[string]string, 2)
		for _, role := range stormService.Spec.Template.Spec.Roles {
			require.NotNil(t, role.Replicas)
			require.Equal(t, int32(1), *role.Replicas)
			require.Equal(t, "smoke-pd", role.Template.Labels[modelNameLabel])
			require.Equal(t, "8000", role.Template.Labels[modelPortLabel])
			require.Len(t, role.Template.Spec.Containers, 1)
			require.Equal(t, mockImage, role.Template.Spec.Containers[0].Image)
			require.JSONEq(t, `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd"}}}`,
				role.Template.Annotations[modelConfigAnnotation])

			environment := map[string]string{}
			for _, variable := range role.Template.Spec.Containers[0].Env {
				environment[variable.Name] = variable.Value
			}
			require.Equal(t, pdContract, environment["MOCK_PD_CONTRACT"])
			roles[role.Name] = environment["MOCK_PD_ROLE"]
		}

		require.Equal(t, map[string]string{
			"prefill": "prefill",
			"decode":  "decode",
		}, roles)
	})
}

func TestStormServiceReady(t *testing.T) {
	readyStormService := func() *orchestrationv1alpha1.StormService {
		return &orchestrationv1alpha1.StormService{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Status: orchestrationv1alpha1.StormServiceStatus{
				ObservedGeneration: 2,
				Replicas:           1,
				ReadyReplicas:      1,
				Conditions: orchestrationv1alpha1.Conditions{{
					Type:   orchestrationv1alpha1.StormServiceReady,
					Status: corev1.ConditionTrue,
				}},
				RoleStatuses: []orchestrationv1alpha1.RoleStatus{
					{Name: "prefill", Replicas: 1, ReadyReplicas: 1},
					{Name: "decode", Replicas: 1, ReadyReplicas: 1},
				},
			},
		}
	}

	tests := []struct {
		name          string
		mutate        func(*orchestrationv1alpha1.StormService)
		expectedRoles map[string]int32
		want          bool
	}{
		{
			name:          "ready",
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1},
			want:          true,
		},
		{
			name: "stale generation",
			mutate: func(stormService *orchestrationv1alpha1.StormService) {
				stormService.Status.ObservedGeneration = 1
			},
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1},
		},
		{
			name: "ready condition false",
			mutate: func(stormService *orchestrationv1alpha1.StormService) {
				stormService.Status.Conditions[0].Status = corev1.ConditionFalse
			},
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1},
		},
		{
			name: "roleset not ready",
			mutate: func(stormService *orchestrationv1alpha1.StormService) {
				stormService.Status.ReadyReplicas = 0
			},
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1},
		},
		{
			name:          "missing role",
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1, "router": 1},
		},
		{
			name: "role replica not ready",
			mutate: func(stormService *orchestrationv1alpha1.StormService) {
				stormService.Status.RoleStatuses[1].ReadyReplicas = 0
			},
			expectedRoles: map[string]int32{"prefill": 1, "decode": 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stormService := readyStormService()
			if test.mutate != nil {
				test.mutate(stormService)
			}
			require.Equal(t, test.want, stormServiceReady(stormService, test.expectedRoles))
		})
	}
}

func TestRoutingResourceBuilders(t *testing.T) {
	service, route, grant := newRoutingResources("default", "smoke-model")

	require.Equal(t, "default", service.Namespace)
	require.Equal(t, "smoke-model", service.Name)
	require.Equal(t, "smoke-model", service.Spec.Selector[modelNameLabel])
	require.Len(t, service.Spec.Ports, 1)
	require.Equal(t, int32(8000), service.Spec.Ports[0].Port)

	require.Equal(t, gatewayNamespace, route.Namespace)
	require.Equal(t, "smoke-model-router", route.Name)
	require.Len(t, route.Spec.ParentRefs, 1)
	require.Equal(t, "aibrix-eg", string(route.Spec.ParentRefs[0].Name))
	require.Len(t, route.Spec.Rules, 1)
	require.Len(t, route.Spec.Rules[0].BackendRefs, 1)
	require.Equal(t, "smoke-model", string(route.Spec.Rules[0].BackendRefs[0].Name))
	require.Len(t, route.Spec.Rules[0].Matches, 1)
	require.Equal(t, "smoke-model", route.Spec.Rules[0].Matches[0].Headers[0].Value)

	require.Equal(t, "default", grant.Namespace)
	require.Equal(t, "smoke-model-route-grant", grant.Name)
	require.Len(t, grant.Spec.From, 1)
	require.Equal(t, gatewayNamespace, string(grant.Spec.From[0].Namespace))
	require.Len(t, grant.Spec.To, 1)
	require.Equal(t, "Service", string(grant.Spec.To[0].Kind))
}
