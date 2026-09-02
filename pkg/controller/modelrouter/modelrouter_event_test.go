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

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func newEventTestRouter(t *testing.T, objs ...client.Object) *ModelRouter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := modelv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &ModelRouter{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
}

func modelWorkloadLabels(modelName, port string) map[string]string {
	return map[string]string{
		constants.ModelLabelName: modelName,
		constants.ModelLabelPort: port,
	}
}

func getHTTPRoute(t *testing.T, c client.Client, modelName string) *gatewayv1.HTTPRoute {
	t.Helper()
	route := &gatewayv1.HTTPRoute{}
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: aibrixEnvoyGatewayNamespace,
		Name:      utils.ModelRouterName(modelName),
	}, route)
	if err != nil {
		t.Fatalf("HTTPRoute for model %q: %v", modelName, err)
	}
	return route
}

func getReferenceGrant(t *testing.T, c client.Client, namespace string) *gatewayv1beta1.ReferenceGrant {
	t.Helper()
	grant := &gatewayv1beta1.ReferenceGrant{}
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: namespace,
		Name:      fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, namespace),
	}, grant)
	if err != nil {
		t.Fatalf("ReferenceGrant in namespace %q: %v", namespace, err)
	}
	return grant
}

func routeMatchPaths(route *gatewayv1.HTTPRoute) []string {
	if len(route.Spec.Rules) == 0 {
		return nil
	}
	paths := make([]string, 0, len(route.Spec.Rules[0].Matches))
	for _, match := range route.Spec.Rules[0].Matches {
		if match.Path != nil && match.Path.Value != nil {
			paths = append(paths, *match.Path.Value)
		}
	}
	return paths
}

func TestInformerEventsCreateHTTPRoute(t *testing.T) {
	const modelName = "llama-7b"
	labels := modelWorkloadLabels(modelName, "8000")

	tests := []struct {
		name      string
		namespace string
		trigger   func(*ModelRouter)
	}{
		{
			name:      "deployment add",
			namespace: "workload-ns",
			trigger: func(m *ModelRouter) {
				m.addRouteFromDeployment(&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "llama-deploy",
						Namespace: "workload-ns",
						Labels:    labels,
					},
				})
			},
		},
		{
			name:      "model adapter add",
			namespace: "adapter-ns",
			trigger: func(m *ModelRouter) {
				m.addRouteFromModelAdapter(&modelv1alpha1.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "llama-adapter",
						Namespace: "adapter-ns",
						Labels:    labels,
					},
				})
			},
		},
		{
			name:      "ray cluster fleet add",
			namespace: "fleet-ns",
			trigger: func(m *ModelRouter) {
				m.addRouteFromRayClusterFleet(&orchestrationv1alpha1.RayClusterFleet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "llama-fleet",
						Namespace: "fleet-ns",
						Labels:    labels,
					},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newEventTestRouter(t)
			tt.trigger(m)

			route := getHTTPRoute(t, m.Client, modelName)
			if route.Namespace != aibrixEnvoyGatewayNamespace {
				t.Errorf("HTTPRoute namespace = %q, want %q", route.Namespace, aibrixEnvoyGatewayNamespace)
			}
			if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
				t.Fatalf("unexpected HTTPRoute rules: %#v", route.Spec.Rules)
			}
			backend := route.Spec.Rules[0].BackendRefs[0]
			if backend.Name != gatewayv1.ObjectName(modelName) {
				t.Errorf("backend name = %q, want %q", backend.Name, modelName)
			}
			if backend.Namespace == nil || string(*backend.Namespace) != tt.namespace {
				t.Errorf("backend namespace = %v, want %q", backend.Namespace, tt.namespace)
			}
			if backend.Port == nil || int32(*backend.Port) != 8000 {
				t.Errorf("backend port = %v, want 8000", backend.Port)
			}

			paths := routeMatchPaths(route)
			if len(paths) != len(modelPaths) {
				t.Fatalf("got %d match paths, want %d default paths: %v", len(paths), len(modelPaths), paths)
			}
			for i, want := range modelPaths {
				if paths[i] != want {
					t.Errorf("match[%d] = %q, want %q", i, paths[i], want)
				}
			}
			if len(route.Spec.Rules[0].Matches[0].Headers) != 1 || route.Spec.Rules[0].Matches[0].Headers[0].Value != modelName {
				t.Errorf("model header match = %#v", route.Spec.Rules[0].Matches[0].Headers)
			}
		})
	}
}

func TestInformerEventsSkipWorkloadsWithoutModelName(t *testing.T) {
	m := newEventTestRouter(t)
	m.addRouteFromDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plain-deploy",
			Namespace: "default",
			Labels:    map[string]string{"app": "plain"},
		},
	})

	routes := &gatewayv1.HTTPRouteList{}
	if err := m.Client.List(context.Background(), routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Items) != 0 {
		t.Fatalf("created %d HTTPRoutes for unlabeled workload, want 0", len(routes.Items))
	}
}

func TestCrossNamespaceReferenceGrantCreation(t *testing.T) {
	t.Run("creates grant when workload is outside aibrix-system", func(t *testing.T) {
		m := newEventTestRouter(t)
		m.addRouteFromDeployment(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		})

		grant := getReferenceGrant(t, m.Client, "models")
		if len(grant.Spec.From) != 1 {
			t.Fatalf("ReferenceGrant.From = %#v, want 1 entry", grant.Spec.From)
		}
		from := grant.Spec.From[0]
		if from.Group != gatewayv1.GroupName || from.Kind != "HTTPRoute" || string(from.Namespace) != aibrixEnvoyGatewayNamespace {
			t.Errorf("ReferenceGrant.From = %#v", from)
		}
		if len(grant.Spec.To) != 1 || grant.Spec.To[0].Group != "" || grant.Spec.To[0].Kind != "Service" {
			t.Errorf("ReferenceGrant.To = %#v", grant.Spec.To)
		}
	})

	t.Run("does not create grant when workload is in aibrix-system", func(t *testing.T) {
		m := newEventTestRouter(t)
		m.addRouteFromDeployment(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: aibrixEnvoyGatewayNamespace,
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		})

		_ = getHTTPRoute(t, m.Client, "llama-7b")
		grant := &gatewayv1beta1.ReferenceGrant{}
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, aibrixEnvoyGatewayNamespace),
		}, grant)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("ReferenceGrant Get error = %v, want NotFound", err)
		}
	})
}

func TestRouteAndReferenceGrantCleanupAfterWorkloadDeletion(t *testing.T) {
	t.Run("deletes route and grant when last model workload is removed", func(t *testing.T) {
		m := newEventTestRouter(t)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		}
		m.addRouteFromDeployment(deploy)
		_ = getHTTPRoute(t, m.Client, "llama-7b")
		_ = getReferenceGrant(t, m.Client, "models")

		m.deleteRouteFromDeployment(deploy)

		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("llama-7b"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after delete: %v, want NotFound", err)
		}
		err = m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: "models",
			Name:      fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, "models"),
		}, &gatewayv1beta1.ReferenceGrant{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("ReferenceGrant after delete: %v, want NotFound", err)
		}
	})

	t.Run("keeps grant when another model deployment remains", func(t *testing.T) {
		remaining := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("mistral-7b", "8000"),
			},
		}
		m := newEventTestRouter(t, remaining)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		}
		m.addRouteFromDeployment(deploy)
		m.deleteRouteFromDeployment(deploy)

		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("llama-7b"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after delete: %v, want NotFound", err)
		}
		_ = getReferenceGrant(t, m.Client, "models")
	})

	t.Run("keeps grant when another model adapter remains", func(t *testing.T) {
		remaining := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-adapter",
				Namespace: "models",
				Labels:    modelWorkloadLabels("mistral-adapter", "8000"),
			},
		}
		m := newEventTestRouter(t, remaining)
		adapter := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-adapter",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-adapter", "8000"),
			},
		}
		m.addRouteFromModelAdapter(adapter)
		m.deleteRouteFromModelAdapter(adapter)
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("llama-adapter"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after adapter delete: %v, want NotFound", err)
		}
		_ = getReferenceGrant(t, m.Client, "models")
	})

	t.Run("keeps grant when another ray cluster fleet remains", func(t *testing.T) {
		remaining := &orchestrationv1alpha1.RayClusterFleet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-fleet",
				Namespace: "models",
				Labels:    modelWorkloadLabels("mistral-fleet", "8000"),
			},
		}
		m := newEventTestRouter(t, remaining)
		fleet := &orchestrationv1alpha1.RayClusterFleet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-fleet",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-fleet", "8000"),
			},
		}
		m.addRouteFromRayClusterFleet(fleet)
		m.deleteRouteFromRayClusterFleet(fleet)
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("llama-fleet"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after fleet delete: %v, want NotFound", err)
		}
		_ = getReferenceGrant(t, m.Client, "models")
	})

	t.Run("keeps grant when a deployment is deleted but a model adapter remains", func(t *testing.T) {
		remaining := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "remaining-adapter",
				Namespace: "models",
				Labels:    modelWorkloadLabels("adapter-model", "8000"),
			},
		}
		m := newEventTestRouter(t, remaining)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		}
		m.addRouteFromDeployment(deploy)
		m.deleteRouteFromDeployment(deploy)
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("llama-7b"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after deployment delete: %v, want NotFound", err)
		}
		_ = getReferenceGrant(t, m.Client, "models")
	})

	t.Run("model adapter delete removes route", func(t *testing.T) {
		m := newEventTestRouter(t)
		adapter := &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-adapter",
				Namespace: "models",
				Labels:    modelWorkloadLabels("adapter-model", "8000"),
			},
		}
		m.addRouteFromModelAdapter(adapter)
		m.deleteRouteFromModelAdapter(adapter)
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("adapter-model"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after adapter delete: %v, want NotFound", err)
		}
	})

	t.Run("ray cluster fleet delete removes route", func(t *testing.T) {
		m := newEventTestRouter(t)
		fleet := &orchestrationv1alpha1.RayClusterFleet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-fleet",
				Namespace: "models",
				Labels:    modelWorkloadLabels("fleet-model", "8000"),
			},
		}
		m.addRouteFromRayClusterFleet(fleet)
		m.deleteRouteFromRayClusterFleet(fleet)
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("fleet-model"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after fleet delete: %v, want NotFound", err)
		}
	})

	t.Run("tombstone delete still removes route", func(t *testing.T) {
		m := newEventTestRouter(t)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels("tombstone-model", "8000"),
			},
		}
		m.addRouteFromDeployment(deploy)
		m.deleteRouteFromDeployment(cache.DeletedFinalStateUnknown{Obj: deploy})
		err := m.Client.Get(context.Background(), client.ObjectKey{
			Namespace: aibrixEnvoyGatewayNamespace,
			Name:      utils.ModelRouterName("tombstone-model"),
		}, &gatewayv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("HTTPRoute after tombstone delete: %v, want NotFound", err)
		}
	})
}

func TestCustomModelRouterPathsAreAppended(t *testing.T) {
	m := newEventTestRouter(t)
	m.addRouteFromDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-path-deploy",
			Namespace: "models",
			Labels:    modelWorkloadLabels("custom-model", "8080"),
			Annotations: map[string]string{
				constants.ModelAnnoRouterCustomPath: "/score, /version",
			},
		},
	})

	route := getHTTPRoute(t, m.Client, "custom-model")
	paths := routeMatchPaths(route)
	wantSuffix := []string{"/score", "/version"}
	if len(paths) < len(modelPaths)+len(wantSuffix) {
		t.Fatalf("got paths %v, want default paths plus %v", paths, wantSuffix)
	}
	for i, want := range modelPaths {
		if paths[i] != want {
			t.Errorf("default path[%d] = %q, want %q", i, paths[i], want)
		}
	}
	gotSuffix := paths[len(modelPaths):]
	if len(gotSuffix) != len(wantSuffix) {
		t.Fatalf("custom paths = %v, want %v", gotSuffix, wantSuffix)
	}
	for i, want := range wantSuffix {
		if gotSuffix[i] != want {
			t.Errorf("custom path[%d] = %q, want %q", i, gotSuffix[i], want)
		}
	}

	backend := route.Spec.Rules[0].BackendRefs[0]
	if backend.Port == nil || int32(*backend.Port) != 8080 {
		t.Errorf("backend port = %v, want 8080 from model port label", backend.Port)
	}
	for i := len(modelPaths); i < len(route.Spec.Rules[0].Matches); i++ {
		headers := route.Spec.Rules[0].Matches[i].Headers
		if len(headers) != 1 || headers[0].Value != "custom-model" {
			t.Errorf("custom match[%d] headers = %#v, want model header custom-model", i, headers)
		}
	}
}
