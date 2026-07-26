/*
Copyright 2024 The Aibrix Team.

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
	"testing"

	"github.com/vllm-project/aibrix/pkg/constants"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func TestCreateHTTPRouteSupportsAnnotatedModelAndServiceNames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	m := &ModelRouter{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	m.createHTTPRoute(
		"default",
		map[string]string{constants.ModelLabelPort: "8000"},
		map[string]string{
			constants.ModelLabelName:       "/models/mock",
			constants.ModelAnnoServiceName: "console-mock-svc",
		},
	)

	routes := &gatewayv1.HTTPRouteList{}
	if err := m.Client.List(context.Background(), routes, client.InNamespace(aibrixEnvoyGatewayNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(routes.Items) != 1 {
		t.Fatalf("created %d HTTPRoutes, want 1", len(routes.Items))
	}
	route := routes.Items[0]
	if errs := validation.IsDNS1123Subdomain(route.Name); len(errs) > 0 {
		t.Fatalf("HTTPRoute name %q is invalid: %v", route.Name, errs)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("unexpected HTTPRoute rules: %#v", route.Spec.Rules)
	}
	backend := route.Spec.Rules[0].BackendRefs[0].Name
	if backend != "console-mock-svc" {
		t.Errorf("backend service = %q, want console-mock-svc", backend)
	}
	gotHeader := route.Spec.Rules[0].Matches[0].Headers[0]
	if gotHeader.Type == nil || *gotHeader.Type != gatewayv1.HeaderMatchExact ||
		gotHeader.Name != modelHeaderIdentifier || gotHeader.Value != "/models/mock" {
		t.Errorf("model header match = %#v", gotHeader)
	}

	grant := &gatewayv1beta1.ReferenceGrant{}
	if err := m.Client.Get(context.Background(), client.ObjectKey{
		Namespace: "default",
		Name:      "aibrix-system-reserved-referencegrant-in-default",
	}, grant); err != nil {
		t.Errorf("ReferenceGrant was not created: %v", err)
	}
}

func TestAppendCustomModelRouterPaths(t *testing.T) {

	modelHeaderMatch := gatewayv1.HTTPHeaderMatch{
		Name:  modelHeaderIdentifier,
		Type:  ptr.To(gatewayv1.HeaderMatchExact),
		Value: "demo",
	}

	tests := []struct {
		name         string
		httpRoute    *gatewayv1.HTTPRoute
		annotations  map[string]string
		wantPaths    []string
		checkHeaders bool
		skipCheck    bool
	}{
		{
			name: "basic append with multiple paths",
			httpRoute: &gatewayv1.HTTPRoute{
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{
									Path: &gatewayv1.HTTPPathMatch{
										Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
										Value: ptr.To("/origin"),
									},
								},
							},
						},
					},
				},
			},
			annotations: map[string]string{
				modelRouterCustomPath: "/foo,/bar,/baz/",
			},
			wantPaths:    []string{"/origin", "/foo", "/bar", "/baz/"},
			checkHeaders: true,
		},
		{
			name: "multiple paths include empty and space",
			httpRoute: &gatewayv1.HTTPRoute{
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{
									Path: &gatewayv1.HTTPPathMatch{
										Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
										Value: ptr.To("/origin"),
									},
								},
							},
						},
					},
				},
			},
			annotations: map[string]string{
				modelRouterCustomPath: "/f oo, /bar , ,/ba z /",
			},
			wantPaths:    []string{"/origin", "/foo", "/bar", "/baz/"},
			checkHeaders: true,
		},
		{
			name: "no related annotation key",
			httpRoute: &gatewayv1.HTTPRoute{
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{Matches: nil},
					},
				},
			},
			annotations: map[string]string{
				"other": "/foo",
			},
			wantPaths:    []string{},
			checkHeaders: false,
		},
		{
			name: "no rules in httpRoute",
			httpRoute: &gatewayv1.HTTPRoute{
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: nil,
				},
			},
			annotations: map[string]string{
				modelRouterCustomPath: "/foo",
			},
			wantPaths:    nil, // empty rule
			checkHeaders: false,
		},
		{
			name:        "nil inputs should not panic",
			httpRoute:   nil,
			annotations: nil,
			skipCheck:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appendCustomModelRouterPaths(tt.httpRoute, modelHeaderMatch, tt.annotations)

			if tt.skipCheck {
				return
			}

			// if no rule
			if tt.httpRoute == nil {
				if tt.wantPaths != nil {
					t.Fatalf("httpRoute is nil, but wantPaths is not nil")
				}
				return
			}

			if len(tt.httpRoute.Spec.Rules) == 0 {
				if tt.wantPaths != nil {
					t.Fatalf("expected some rules, got 0")
				}
				return
			}

			matches := tt.httpRoute.Spec.Rules[0].Matches
			if len(matches) != len(tt.wantPaths) {
				t.Fatalf("expected %d matches, got %d", len(tt.wantPaths), len(matches))
			}

			for i, want := range tt.wantPaths {
				if matches[i].Path == nil || matches[i].Path.Value == nil {
					t.Fatalf("match[%d] path is nil", i)
				}
				got := *matches[i].Path.Value
				if got != want {
					t.Errorf("match[%d] path = %q, want %q", i, got, want)
				}

				if tt.checkHeaders && i > 0 {
					if len(matches[i].Headers) != 1 {
						t.Errorf("match[%d] expected 1 header, got %d", i, len(matches[i].Headers))
					} else if matches[i].Headers[0] != modelHeaderMatch {
						t.Errorf("match[%d] header = %#v, want %#v", i, matches[i].Headers[0], modelHeaderMatch)
					}
				}
			}
		})
	}
}
