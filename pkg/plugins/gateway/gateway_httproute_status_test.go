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

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

func TestValidateHTTPRouteStatus(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		setupMock   func(*MockGatewayClient, *MockGatewayV1Client, *MockHTTPRouteClient)
		wantErr     bool
		errContains string
	}{
		{
			name:  "successful validation for path model name",
			model: "/models/mock",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "gateway-test").Return(http)

				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: string(gatewayv1.RouteReasonAccepted),
									Status: metav1.ConditionTrue,
								}, {
									Type:   string(gatewayv1.RouteConditionResolvedRefs),
									Reason: string(gatewayv1.RouteReasonResolvedRefs),
									Status: metav1.ConditionTrue,
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, utils.ModelRouterName("/models/mock"), mock.Anything).Return(route, nil)
			},
			wantErr: false,
		},
		{
			name:  "httproute get returns error",
			model: "get-failed",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "gateway-test").Return(http)
				http.On("Get", mock.Anything, "get-failed-router", mock.Anything).Return((*gatewayv1.HTTPRoute)(nil), errors.New("boom"))
			},
			wantErr:     true,
			errContains: "boom",
		},
		{
			name:  "no valid status conditions",
			model: "no-conditions",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "gateway-test").Return(http)
				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "no-conditions-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "httproute: gateway-test/no-conditions-router, does not have valid status",
		},
		{
			name:  "resolved refs not resolved",
			model: "refs-not-resolved",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "gateway-test").Return(http)
				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: string(gatewayv1.RouteReasonAccepted),
									Status: metav1.ConditionTrue,
								}, {
									Type:   string(gatewayv1.RouteConditionResolvedRefs),
									Reason: "InvalidRef",
									Status: metav1.ConditionFalse,
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "refs-not-resolved-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "httproute: gateway-test/refs-not-resolved-router, route's object references are not resolved",
		},
		{
			name:  "invalid route status",
			model: "invalid-model",
			setupMock: func(gw *MockGatewayClient, gwv1 *MockGatewayV1Client, http *MockHTTPRouteClient) {
				gw.On("GatewayV1").Return(gwv1)
				gwv1.On("HTTPRoutes", "gateway-test").Return(http)

				route := &gatewayv1.HTTPRoute{
					Status: gatewayv1.HTTPRouteStatus{
						RouteStatus: gatewayv1.RouteStatus{
							Parents: []gatewayv1.RouteParentStatus{{
								Conditions: []metav1.Condition{{
									Type:   string(gatewayv1.RouteConditionAccepted),
									Reason: "InvalidReason",
								}},
							}},
						},
					},
				}
				http.On("Get", mock.Anything, "invalid-model-router", mock.Anything).Return(route, nil)
			},
			wantErr:     true,
			errContains: "httproute: gateway-test/invalid-model-router, route is not accepted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mocks
			mockGW := &MockGatewayClient{}
			mockGWV1 := &MockGatewayV1Client{}
			mockHTTP := &MockHTTPRouteClient{}
			tt.setupMock(mockGW, mockGWV1, mockHTTP)

			// Create test server with mock client
			s := &Server{
				gatewayClient:    mockGW,
				gatewayNamespace: "gateway-test",
			}

			// Run test
			err := s.validateHTTPRouteStatus(context.Background(), tt.model)

			// Verify results
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				assert.NoError(t, err)
			}

			// Verify mock expectations
			mockGW.AssertExpectations(t)
			mockGWV1.AssertExpectations(t)
			mockHTTP.AssertExpectations(t)
		})
	}
}

func TestValidateHTTPRouteStatus_ParentReadiness(t *testing.T) {
	const model = "model"
	parent := func(conditions ...metav1.Condition) gatewayv1.RouteParentStatus {
		return gatewayv1.RouteParentStatus{Conditions: conditions}
	}
	accepted := metav1.Condition{
		Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "ControllerAccepted",
	}
	resolved := metav1.Condition{
		Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, Reason: "ReferencesVerified",
	}
	pending := metav1.Condition{Type: resolved.Type, Status: metav1.ConditionUnknown, Reason: "Pending"}
	notFound := metav1.Condition{Type: resolved.Type, Status: metav1.ConditionFalse, Reason: "BackendNotFound"}
	programmed := metav1.Condition{Type: "Programmed", Status: metav1.ConditionTrue, Reason: "Programmed"}

	for _, tc := range []struct {
		name    string
		parents []gatewayv1.RouteParentStatus
		wantErr string
	}{
		{name: "no parents", wantErr: "no parent reports"},
		{
			name:    "unrelated condition",
			parents: []gatewayv1.RouteParentStatus{parent(programmed)},
			wantErr: "no parent reports",
		}, {
			name:    "conditions on different parents",
			parents: []gatewayv1.RouteParentStatus{parent(accepted), parent(resolved)},
			wantErr: "no parent reports",
		}, {
			name:    "unknown condition",
			parents: []gatewayv1.RouteParentStatus{parent(accepted, pending)},
			wantErr: "Pending",
		}, {
			name: "one ready parent",
			parents: []gatewayv1.RouteParentStatus{
				parent(accepted, notFound), parent(), parent(accepted, resolved), parent(),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "aibrix-system", Name: utils.ModelRouterName(model)},
			}
			route.Status.Parents = tc.parents
			s := &Server{gatewayClient: gatewayfake.NewSimpleClientset(route)}
			err := s.validateHTTPRouteStatus(context.Background(), model)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateHTTPRouteStatus_StandaloneModeSkipsValidation(t *testing.T) {
	s := &Server{gatewayClient: nil}
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "any-model"))
}

func TestValidateHTTPRouteStatus_CachesResult(t *testing.T) {
	mockGW := &MockGatewayClient{}
	mockGWV1 := &MockGatewayV1Client{}
	mockHTTP := &MockHTTPRouteClient{}

	route := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{{
					Conditions: []metav1.Condition{{
						Type:   string(gatewayv1.RouteConditionAccepted),
						Reason: string(gatewayv1.RouteReasonAccepted),
						Status: metav1.ConditionTrue,
					}, {
						Type:   string(gatewayv1.RouteConditionResolvedRefs),
						Reason: string(gatewayv1.RouteReasonResolvedRefs),
						Status: metav1.ConditionTrue,
					}},
				}},
			},
		},
	}
	// Expect only one API call despite two invocations
	mockGW.On("GatewayV1").Return(mockGWV1).Once()
	mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP).Once()
	mockHTTP.On("Get", mock.Anything, "cached-model-router", mock.Anything).Return(route, nil).Once()

	s := &Server{
		gatewayClient:     mockGW,
		gatewayNamespace:  "aibrix-system",
		httprouteCacheTTL: 30 * time.Second,
	}

	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "cached-model"))
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "cached-model"))

	mockGW.AssertExpectations(t)
	mockGWV1.AssertExpectations(t)
	mockHTTP.AssertExpectations(t)
}

func TestValidateHTTPRouteStatus_CacheExpiry(t *testing.T) {
	mockGW := &MockGatewayClient{}
	mockGWV1 := &MockGatewayV1Client{}
	mockHTTP := &MockHTTPRouteClient{}

	route := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{{
					Conditions: []metav1.Condition{{
						Type:   string(gatewayv1.RouteConditionAccepted),
						Reason: string(gatewayv1.RouteReasonAccepted),
						Status: metav1.ConditionTrue,
					}, {
						Type:   string(gatewayv1.RouteConditionResolvedRefs),
						Reason: string(gatewayv1.RouteReasonResolvedRefs),
						Status: metav1.ConditionTrue,
					}},
				}},
			},
		},
	}
	// Expect two API calls because the TTL is already expired
	mockGW.On("GatewayV1").Return(mockGWV1).Twice()
	mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP).Twice()
	mockHTTP.On("Get", mock.Anything, "expire-model-router", mock.Anything).Return(route, nil).Twice()

	s := &Server{
		gatewayClient:     mockGW,
		gatewayNamespace:  "aibrix-system",
		httprouteCacheTTL: 1 * time.Millisecond,
	}

	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "expire-model"))
	time.Sleep(5 * time.Millisecond)
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "expire-model"))

	mockGW.AssertExpectations(t)
	mockGWV1.AssertExpectations(t)
	mockHTTP.AssertExpectations(t)
}

func TestValidateHTTPRouteStatus_ContextErrorNotCached(t *testing.T) {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(ctxErr.Error(), func(t *testing.T) {
			mockGW := &MockGatewayClient{}
			mockGWV1 := &MockGatewayV1Client{}
			mockHTTP := &MockHTTPRouteClient{}

			// First call returns a context error; second call succeeds.
			// Both must hit the API — the context error must not be cached.
			mockGW.On("GatewayV1").Return(mockGWV1).Twice()
			mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP).Twice()
			mockHTTP.On("Get", mock.Anything, "ctx-err-model-router", mock.Anything).
				Return((*gatewayv1.HTTPRoute)(nil), ctxErr).Once()

			route := &gatewayv1.HTTPRoute{
				Status: gatewayv1.HTTPRouteStatus{
					RouteStatus: gatewayv1.RouteStatus{
						Parents: []gatewayv1.RouteParentStatus{{
							Conditions: []metav1.Condition{{
								Type:   string(gatewayv1.RouteConditionAccepted),
								Reason: string(gatewayv1.RouteReasonAccepted),
								Status: metav1.ConditionTrue,
							}, {
								Type:   string(gatewayv1.RouteConditionResolvedRefs),
								Reason: string(gatewayv1.RouteReasonResolvedRefs),
								Status: metav1.ConditionTrue,
							}},
						}},
					},
				},
			}
			mockHTTP.On("Get", mock.Anything, "ctx-err-model-router", mock.Anything).
				Return(route, nil).Once()

			s := &Server{
				gatewayClient:     mockGW,
				gatewayNamespace:  "aibrix-system",
				httprouteCacheTTL: 30 * time.Second,
			}

			assert.ErrorIs(t, s.validateHTTPRouteStatus(context.Background(), "ctx-err-model"), ctxErr)
			assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "ctx-err-model"))

			mockGW.AssertExpectations(t)
			mockGWV1.AssertExpectations(t)
			mockHTTP.AssertExpectations(t)
		})
	}
}

func TestValidateHTTPRouteStatus_TransientRouteErrorExpires(t *testing.T) {
	mockGW := &MockGatewayClient{}
	mockGWV1 := &MockGatewayV1Client{}
	mockHTTP := &MockHTTPRouteClient{}

	mockGW.On("GatewayV1").Return(mockGWV1).Twice()
	mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP).Twice()
	mockHTTP.On("Get", mock.Anything, "transient-route-router", mock.Anything).
		Return((*gatewayv1.HTTPRoute)(nil), errors.New("route is not ready")).Once()

	route := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{{
					Conditions: []metav1.Condition{{
						Type:   string(gatewayv1.RouteConditionAccepted),
						Reason: string(gatewayv1.RouteReasonAccepted),
						Status: metav1.ConditionTrue,
					}, {
						Type:   string(gatewayv1.RouteConditionResolvedRefs),
						Reason: string(gatewayv1.RouteReasonResolvedRefs),
						Status: metav1.ConditionTrue,
					}},
				}},
			},
		},
	}
	mockHTTP.On("Get", mock.Anything, "transient-route-router", mock.Anything).
		Return(route, nil).Once()

	s := &Server{
		gatewayClient:     mockGW,
		gatewayNamespace:  "aibrix-system",
		httprouteCacheTTL: 30 * time.Second,
		httprouteErrorTTL: time.Millisecond,
	}

	assert.Error(t, s.validateHTTPRouteStatus(context.Background(), "transient-route"))
	time.Sleep(5 * time.Millisecond)
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "transient-route"))

	mockGW.AssertExpectations(t)
	mockGWV1.AssertExpectations(t)
	mockHTTP.AssertExpectations(t)
}

func TestValidateHTTPRouteStatus_EmptyParentsRecover(t *testing.T) {
	checkHTTPRouteErrorExpiry(t, &gatewayv1.HTTPRoute{})
}

func TestValidateHTTPRouteStatus_UnresolvedRouteErrorExpires(t *testing.T) {
	invalidRoute := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{
			Parents: []gatewayv1.RouteParentStatus{{Conditions: []metav1.Condition{{
				Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionFalse, Reason: "BackendNotFound",
			}}}},
		}},
	}
	checkHTTPRouteErrorExpiry(t, invalidRoute)
}

func makeTestHTTPRoute(model string, generation int64, parents ...gatewayv1.RouteParentStatus) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "aibrix-system", Name: utils.ModelRouterName(model), Generation: generation},
		Status:     gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: parents}},
	}
}

func makeRouteCond(condType gatewayv1.RouteConditionType, status metav1.ConditionStatus, reason string, gen int64) metav1.Condition {
	return metav1.Condition{Type: string(condType), Status: status, Reason: reason, ObservedGeneration: gen}
}

func TestValidateHTTPRouteStatus_ObservedGeneration(t *testing.T) {
	const (
		model    = "probe"
		cacheTTL = 30 * time.Second
		errorTTL = 2 * time.Second
	)
	parent := func(conds ...metav1.Condition) gatewayv1.RouteParentStatus {
		return gatewayv1.RouteParentStatus{Conditions: conds}
	}
	acc := func(gen int64) metav1.Condition {
		return makeRouteCond(gatewayv1.RouteConditionAccepted, metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), gen)
	}
	res := func(gen int64) metav1.Condition {
		return makeRouteCond(gatewayv1.RouteConditionResolvedRefs, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), gen)
	}
	resErr := func(gen int64) metav1.Condition {
		return makeRouteCond(gatewayv1.RouteConditionResolvedRefs, metav1.ConditionFalse, "BackendNotFound", gen)
	}

	for _, tc := range []struct {
		name         string
		gen          int64
		parents      []gatewayv1.RouteParentStatus
		wantErr      string
		wantShortTTL bool
	}{
		{"stale observedGeneration fails open under error TTL", 2, []gatewayv1.RouteParentStatus{parent(acc(1), res(1))}, "", true},
		{"zero observedGeneration with generation zero", 0, []gatewayv1.RouteParentStatus{parent(acc(0), res(0))}, "", false},
		{"zero observedGeneration with positive generation", 2, []gatewayv1.RouteParentStatus{parent(acc(0), res(0))}, "", false},
		{"current observedGeneration", 2, []gatewayv1.RouteParentStatus{parent(acc(2), res(2))}, "", false},
		{"higher observedGeneration than generation", 2, []gatewayv1.RouteParentStatus{parent(acc(3), res(3))}, "", false},
		{"mixed current and stale conditions on same parent", 2, []gatewayv1.RouteParentStatus{parent(acc(2), res(1))}, "", true},
		{"current conditions split across different parents", 2, []gatewayv1.RouteParentStatus{parent(acc(2), res(1)), parent(acc(1), res(2))}, "", true},
		{"one stale parent and one current parent", 2, []gatewayv1.RouteParentStatus{parent(acc(1), res(1)), parent(acc(2), res(2))}, "", false},
		{"stale False condition does not leak reason", 2, []gatewayv1.RouteParentStatus{parent(acc(2), resErr(1))}, "", true},
		{"current failing with stale passing reports current reason", 2, []gatewayv1.RouteParentStatus{parent(acc(1), resErr(2))}, "BackendNotFound", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				gatewayClient:     gatewayfake.NewSimpleClientset(makeTestHTTPRoute(model, tc.gen, tc.parents...)),
				gatewayNamespace:  "aibrix-system",
				httprouteCacheTTL: cacheTTL,
				httprouteErrorTTL: errorTTL,
			}
			err := s.validateHTTPRouteStatus(context.Background(), model)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			raw, ok := s.httprouteCache.Load(model)
			require.True(t, ok)
			remaining := time.Until(raw.(httpRouteCacheEntry).expiresAt)
			if tc.wantShortTTL {
				assert.LessOrEqual(t, remaining, errorTTL+time.Second)
			} else {
				assert.Greater(t, remaining, cacheTTL/2)
			}
		})
	}
}

func TestValidateHTTPRouteStatus_StaleGenerationTransitions(t *testing.T) {
	const (
		model    = "probe"
		cacheTTL = 30 * time.Second
		errorTTL = 2 * time.Second
	)
	acc := func(gen int64) metav1.Condition {
		return makeRouteCond(gatewayv1.RouteConditionAccepted, metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), gen)
	}
	res := func(status metav1.ConditionStatus, reason string, gen int64) metav1.Condition {
		return makeRouteCond(gatewayv1.RouteConditionResolvedRefs, status, reason, gen)
	}

	for _, tc := range []struct {
		name         string
		nextConds    []metav1.Condition
		wantErr      string
		wantShortTTL bool
	}{
		{"stale to BackendNotFound", []metav1.Condition{acc(2), res(metav1.ConditionFalse, "BackendNotFound", 2)}, "BackendNotFound", true},
		{"stale to healthy recovers full cache TTL", []metav1.Condition{acc(2), res(metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), 2)}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := makeTestHTTPRoute(model, 2, gatewayv1.RouteParentStatus{
				Conditions: []metav1.Condition{acc(1), res(metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), 1)},
			})
			gwClient := gatewayfake.NewSimpleClientset(route)
			s := &Server{
				gatewayClient:     gwClient,
				gatewayNamespace:  "aibrix-system",
				httprouteCacheTTL: cacheTTL,
				httprouteErrorTTL: errorTTL,
			}

			// Cache miss on stale status fails open under short error TTL.
			require.NoError(t, s.validateHTTPRouteStatus(context.Background(), model))
			raw, ok := s.httprouteCache.Load(model)
			require.True(t, ok)
			entry := raw.(httpRouteCacheEntry)
			assert.LessOrEqual(t, time.Until(entry.expiresAt), errorTTL+time.Second)

			// Controller writes generation 2 status; expire cache without wall-clock sleep.
			updated := route.DeepCopy()
			updated.Status.Parents[0].Conditions = tc.nextConds
			_, err := gwClient.GatewayV1().HTTPRoutes("aibrix-system").UpdateStatus(context.Background(), updated, metav1.UpdateOptions{})
			require.NoError(t, err)
			entry.expiresAt = time.Now().Add(-time.Second)
			s.httprouteCache.Store(model, entry)

			err = s.validateHTTPRouteStatus(context.Background(), model)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			raw, ok = s.httprouteCache.Load(model)
			require.True(t, ok)
			remaining := time.Until(raw.(httpRouteCacheEntry).expiresAt)
			if tc.wantShortTTL {
				assert.LessOrEqual(t, remaining, errorTTL+time.Second)
			} else {
				assert.Greater(t, remaining, cacheTTL/2)
			}
		})
	}
}

func checkHTTPRouteErrorExpiry(t *testing.T, invalidRoute *gatewayv1.HTTPRoute) {
	t.Helper()
	mockGW := &MockGatewayClient{}
	mockGWV1 := &MockGatewayV1Client{}
	mockHTTP := &MockHTTPRouteClient{}
	mockGW.On("GatewayV1").Return(mockGWV1).Twice()
	mockGWV1.On("HTTPRoutes", "aibrix-system").Return(mockHTTP).Twice()

	validRoute := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{
			Parents: []gatewayv1.RouteParentStatus{{Conditions: []metav1.Condition{{
				Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted),
			}, {
				Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonResolvedRefs),
			}}}},
		}},
	}
	mockHTTP.On("Get", mock.Anything, "unresolved-route-router", mock.Anything).Return(invalidRoute, nil).Once()
	mockHTTP.On("Get", mock.Anything, "unresolved-route-router", mock.Anything).Return(validRoute, nil).Once()

	s := &Server{gatewayClient: mockGW, gatewayNamespace: "aibrix-system", httprouteCacheTTL: time.Second, httprouteErrorTTL: time.Millisecond}
	assert.Error(t, s.validateHTTPRouteStatus(context.Background(), "unresolved-route"))
	entry, ok := s.httprouteCache.Load("unresolved-route")
	require.True(t, ok, "pending status should use the error cache")
	require.Error(t, entry.(httpRouteCacheEntry).err)
	time.Sleep(5 * time.Millisecond)
	assert.NoError(t, s.validateHTTPRouteStatus(context.Background(), "unresolved-route"))

	mockGW.AssertExpectations(t)
	mockGWV1.AssertExpectations(t)
	mockHTTP.AssertExpectations(t)
}
