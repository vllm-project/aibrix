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

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/features"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestRegisterSchemasIncludesModelClaimAPI(t *testing.T) {
	previous := features.EnabledControllers
	features.EnabledControllers = map[string]bool{
		features.ModelClaimController: true,
	}
	t.Cleanup(func() { features.EnabledControllers = previous })

	testScheme := runtime.NewScheme()
	require.NoError(t, RegisterSchemas(testScheme))
	_, err := testScheme.New(schema.GroupVersionKind{
		Group: modelv1alpha1.GroupVersion.Group, Version: modelv1alpha1.GroupVersion.Version, Kind: "ModelClaim",
	})
	require.NoError(t, err)
}

func TestRegisterSchemasIncludesModelWarmupAPI(t *testing.T) {
	previous := features.EnabledControllers
	features.EnabledControllers = map[string]bool{
		features.ModelWarmupController: true,
	}
	t.Cleanup(func() { features.EnabledControllers = previous })

	testScheme := runtime.NewScheme()
	require.NoError(t, RegisterSchemas(testScheme))
	_, err := testScheme.New(schema.GroupVersionKind{
		Group: modelv1alpha1.GroupVersion.Group, Version: modelv1alpha1.GroupVersion.Version, Kind: "ModelWarmup",
	})
	require.NoError(t, err)
}

func TestSetupWebhooksServesOnlyRegisteredAPIGroups(t *testing.T) {
	const (
		modelAdapter    = "/validate-model-aibrix-ai-v1alpha1-modeladapter"
		modelClaim      = "/validate-model-aibrix-ai-v1alpha1-modelclaim"
		modelWarmup     = "/validate-model-aibrix-ai-v1alpha1-modelwarmup"
		kvCache         = "/validate-orchestration-aibrix-ai-v1alpha1-kvcache"
		stormService    = "/validate-orchestration-aibrix-ai-v1alpha1-stormservice"
		rayClusterFleet = "/validate-orchestration-aibrix-ai-v1alpha1-rayclusterfleet"
		podAutoscaler   = "/validate-autoscaling-aibrix-ai-v1alpha1-podautoscaler"
		deployment      = "/validate-apps-v1-deployment"
	)
	model := []string{modelAdapter, modelClaim, modelWarmup}
	orchestration := []string{kvCache, stormService, rayClusterFleet}
	join := func(groups ...[]string) []string {
		var paths []string
		for _, group := range groups {
			paths = append(paths, group...)
		}
		return paths
	}

	tests := []struct {
		controllers string
		served      []string
		notServed   []string
	}{
		{
			controllers: "*",
			served:      join(model, orchestration, []string{podAutoscaler, deployment}),
		},
		{
			// The flags of config/standalone/model-adapter-controller.
			controllers: "model-adapter-controller",
			served:      join(model, []string{deployment}),
			notServed:   join(orchestration, []string{podAutoscaler}),
		},
		{
			controllers: "model-warmup-controller",
			served:      join(model, []string{deployment}),
			notServed:   join(orchestration, []string{podAutoscaler}),
		},
		{
			controllers: "*,-pod-autoscaler-controller",
			served:      join(model, orchestration, []string{deployment}),
			notServed:   []string{podAutoscaler},
		},
		{
			controllers: "kv-cache-controller",
			served:      join(orchestration, []string{deployment}),
			notServed:   join(model, []string{podAutoscaler}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.controllers, func(t *testing.T) {
			previous := features.EnabledControllers
			features.EnabledControllers = map[string]bool{}
			t.Cleanup(func() { features.EnabledControllers = previous })
			features.InitControllers(tt.controllers)

			testScheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(testScheme))
			require.NoError(t, RegisterSchemas(testScheme))
			mgr := newWebhookTestManager(t, testScheme)

			require.NoError(t, setupWebhooks(mgr))

			mux := mgr.GetWebhookServer().WebhookMux()
			isServed := func(path string) bool {
				_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, path, nil))
				return pattern == path
			}
			for _, path := range tt.served {
				require.Truef(t, isServed(path), "expected webhook %s to be served", path)
			}
			for _, path := range tt.notServed {
				require.Falsef(t, isServed(path), "expected webhook %s not to be served", path)
			}
		})
	}
}

func TestSetupWebhooksReturnsOtherSchemeErrors(t *testing.T) {
	previous := webhookRegistrations
	t.Cleanup(func() { webhookRegistrations = previous })
	setupCalled := false
	webhookRegistrations = []webhookRegistration{{
		name: "MissingKind",
		// An unstructured object without a kind fails ObjectKinds with a missing-kind error,
		// not a not-registered one, so it must not be skipped.
		obj:   &unstructured.Unstructured{},
		setup: func(ctrl.Manager) error { setupCalled = true; return nil },
	}}

	err := setupWebhooks(newWebhookTestManager(t, runtime.NewScheme()))

	require.True(t, runtime.IsMissingKind(err), "expected a missing-kind error, got %v", err)
	require.False(t, setupCalled)
}

// newWebhookTestManager returns a manager that is never started. Registering webhooks makes no
// API calls, so it never contacts the configured host.
func newWebhookTestManager(t *testing.T, scheme *runtime.Scheme) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)
	return mgr
}
