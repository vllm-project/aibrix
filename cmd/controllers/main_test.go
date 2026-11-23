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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	cfg "github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/controller/modeladapter"
	"github.com/vllm-project/aibrix/pkg/controller/modelrouter"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler"
	"github.com/vllm-project/aibrix/pkg/features"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	// Every path in config/webhook/manifests.yaml, so a webhook that registers only one of its
	// mutating and validating paths is caught.
	model := []string{
		"/mutate-model-aibrix-ai-v1alpha1-modeladapter",
		"/validate-model-aibrix-ai-v1alpha1-modeladapter",
		"/validate-model-aibrix-ai-v1alpha1-modelclaim",
		"/validate-model-aibrix-ai-v1alpha1-modelwarmup",
	}
	orchestration := []string{
		"/mutate-orchestration-aibrix-ai-v1alpha1-kvcache",
		"/validate-orchestration-aibrix-ai-v1alpha1-kvcache",
		"/mutate-orchestration-aibrix-ai-v1alpha1-stormservice",
		"/validate-orchestration-aibrix-ai-v1alpha1-stormservice",
		"/validate-orchestration-aibrix-ai-v1alpha1-stormservice-scale",
		"/validate-orchestration-aibrix-ai-v1alpha1-rayclusterfleet",
	}
	podAutoscaler := []string{
		"/mutate-autoscaling-aibrix-ai-v1alpha1-podautoscaler",
		"/validate-autoscaling-aibrix-ai-v1alpha1-podautoscaler",
	}
	deployment := []string{
		"/mutate-apps-v1-deployment",
		"/validate-apps-v1-deployment",
	}
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
			served:      join(model, orchestration, podAutoscaler, deployment),
		},
		{
			// The flags of config/standalone/model-adapter-controller.
			controllers: "model-adapter-controller",
			served:      join(model, deployment),
			notServed:   join(orchestration, podAutoscaler),
		},
		{
			controllers: "model-warmup-controller",
			served:      join(model, deployment),
			notServed:   join(orchestration, podAutoscaler),
		},
		{
			controllers: "*,-pod-autoscaler-controller",
			served:      join(model, orchestration, deployment),
			notServed:   podAutoscaler,
		},
		{
			controllers: "kv-cache-controller",
			served:      join(orchestration, deployment),
			notServed:   join(model, podAutoscaler),
		},
		{
			// ModelRoute watches ModelAdapter and RayClusterFleet, so it needs both groups.
			controllers: "model-route-controller",
			served:      join(model, orchestration, deployment),
			notServed:   podAutoscaler,
		},
		{
			// PodAutoscaler reads and patches StormService, so it needs the orchestration group too.
			controllers: "pod-autoscaler-controller",
			served:      join(orchestration, podAutoscaler, deployment),
			notServed:   model,
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
			mgr := newTestManager(t, testScheme)

			require.NoError(t, setupWebhooks(mgr, webhookRegistrations))

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
	setupCalled := false
	registrations := []webhookRegistration{{
		name: "MissingKind",
		// An unstructured object without a kind fails ObjectKinds with a missing-kind error,
		// not a not-registered one, so it must not be skipped.
		obj:   &unstructured.Unstructured{},
		setup: func(ctrl.Manager) error { setupCalled = true; return nil },
	}}

	err := setupWebhooks(newTestManager(t, runtime.NewScheme()), registrations)

	require.True(t, runtime.IsMissingKind(err), "expected a missing-kind error, got %v", err)
	require.False(t, setupCalled)
}

// TestStartupWithOnlyModelRouteController runs the manager's startup steps for
// --controllers=model-route-controller: scheme registration, webhook setup and the model route
// controller's setup. It calls modelrouter.Add directly rather than controller.Initialize, which
// appends to a package-level list.
func TestStartupWithOnlyModelRouteController(t *testing.T) {
	previous := features.EnabledControllers
	features.EnabledControllers = map[string]bool{}
	t.Cleanup(func() { features.EnabledControllers = previous })
	features.InitControllers(features.ModelRouteController)

	testScheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(testScheme))
	require.NoError(t, RegisterSchemas(testScheme))
	mgr := newTestManager(t, testScheme)

	require.NoError(t, setupWebhooks(mgr, webhookRegistrations))
	require.NoError(t, modelrouter.Add(mgr,
		cfg.NewRuntimeConfig(false, false, modeladapter.DefaultModelAdapterSchedulerPolicy)))
}

// TestPodAutoscalerControllerStormServiceScale verifies that RegisterSchemas for
// --controllers=pod-autoscaler-controller includes the orchestration group needed by
// WorkloadScale's typed StormService reads and patches.
func TestPodAutoscalerControllerStormServiceScale(t *testing.T) {
	previous := features.EnabledControllers
	features.EnabledControllers = map[string]bool{}
	t.Cleanup(func() { features.EnabledControllers = previous })
	features.InitControllers(features.PodAutoscalerController)

	testScheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(testScheme))
	require.NoError(t, RegisterSchemas(testScheme))

	// Seed as unstructured so the fake client builds even when StormService is missing from testScheme.
	ssObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": orchestrationv1alpha1.GroupVersion.String(),
			"kind":       orchestrationv1alpha1.StormServiceKind,
			"metadata": map[string]any{
				"name":      "ss-pool",
				"namespace": "default",
			},
			"spec": map[string]any{
				"mode": string(orchestrationv1alpha1.StormServicePooledMode),
				"template": map[string]any{
					"spec": map[string]any{
						"roles": []any{
							map[string]any{
								"name":     "prefill",
								"replicas": int64(2),
							},
						},
					},
				},
			},
			"status": map[string]any{
				"roleStatuses": []any{
					map[string]any{
						"name":     "prefill",
						"replicas": int64(2),
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(ssObj).Build()
	scaler := podautoscaler.NewWorkloadScale(fakeClient, nil)

	scaleObj := &unstructured.Unstructured{}
	scaleObj.SetAPIVersion(orchestrationv1alpha1.GroupVersion.String())
	scaleObj.SetKind(orchestrationv1alpha1.StormServiceKind)
	scaleObj.SetNamespace("default")
	scaleObj.SetName("ss-pool")

	basePA := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pa-storm",
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: orchestrationv1alpha1.GroupVersion.String(),
				Kind:       orchestrationv1alpha1.StormServiceKind,
				Name:       "ss-pool",
			},
		},
	}

	ctx := context.Background()

	// Whole-StormService scaling still calls getPodSelectorForRole with a typed StormService Get.
	wholeSel, err := scaler.GetPodSelectorFromScale(ctx, basePA, scaleObj)
	require.NoError(t, err)
	expectedWhole, err := labels.Parse(constants.StormServiceNameLabelKey + "=ss-pool")
	require.NoError(t, err)
	require.Equal(t, expectedWhole.String(), wholeSel.String())

	// Role-level scaling calls Validate, GetPodSelectorFromScale, GetCurrentReplicasFromScale, and SetDesiredReplicas.
	rolePA := basePA.DeepCopy()
	rolePA.Spec.SubTargetSelector = &autoscalingv1alpha1.SubTargetSelector{RoleName: "prefill"}

	require.NoError(t, scaler.Validate(ctx, rolePA))

	roleSel, err := scaler.GetPodSelectorFromScale(ctx, rolePA, scaleObj)
	require.NoError(t, err)
	expectedRole, err := labels.Parse(constants.RoleNameLabelKey + "=prefill," + constants.StormServiceNameLabelKey + "=ss-pool")
	require.NoError(t, err)
	require.Equal(t, expectedRole.String(), roleSel.String())

	counts, err := scaler.GetCurrentReplicasFromScale(ctx, rolePA, scaleObj)
	require.NoError(t, err)
	require.Equal(t, podautoscaler.ReplicaCounts{Configured: 2, Observed: 2}, counts)

	require.NoError(t, scaler.SetDesiredReplicas(ctx, rolePA, 4))

	updatedCounts, err := scaler.GetCurrentReplicasFromScale(ctx, rolePA, scaleObj)
	require.NoError(t, err)
	require.Equal(t, int32(4), updatedCounts.Configured)
}

// newTestManager returns a manager that is never started. Its REST mapper is built from the scheme
// instead of discovery, so controllers can create informers, and a type missing from the scheme
// still fails as it does in the real manager. Anything that calls the API server directly fails
// fast against the unreachable host.
func newTestManager(t *testing.T, scheme *runtime.Scheme) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
			mapper := meta.NewDefaultRESTMapper(nil)
			for gvk := range scheme.AllKnownTypes() {
				mapper.Add(gvk, meta.RESTScopeNamespace)
			}
			return mapper, nil
		},
	})
	require.NoError(t, err)
	return mgr
}
