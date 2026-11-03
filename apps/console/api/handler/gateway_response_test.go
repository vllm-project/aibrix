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

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

func TestGatewayResponseSerializationFailure(t *testing.T) {
	response := httptest.NewRecorder()
	writeGatewayInstanceJSON(response, http.StatusOK, make(chan int))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] == "" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("expected JSON error response: %s", response.Body.String())
	}
}

func TestGatewayInstanceAPIUpstreamAuthorizationErrors(t *testing.T) {
	resource := schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "gateways"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unauthorized", apierrors.NewUnauthorized("expired token for system:serviceaccount:inference:console")},
		{"forbidden", apierrors.NewForbidden(resource, "public", fmt.Errorf("system:serviceaccount:inference:console cannot list gateways"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := gatewayfake.NewSimpleClientset()
			client.PrependReactor("list", "gateways", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, tc.err
			})
			mux := gatewayMux(t, client, "inference")
			response := serveModelAdapterRequest(t, mux, http.MethodGet, gatewayInstancePath, nil)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "serviceaccount") {
				t.Fatalf("upstream credentials leaked or mistaken for user authentication: %d %s", response.Code, response.Body.String())
			}
			anonymous := serveModelAdapterRequestAsRole(t, mux, http.MethodGet, gatewayInstancePath, nil, "")
			if anonymous.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous status = %d, want 401", anonymous.Code)
			}
		})
	}
}
