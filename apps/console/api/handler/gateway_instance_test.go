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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/vllm-project/aibrix/apps/console/api/config"
	deploymentprovider "github.com/vllm-project/aibrix/apps/console/api/deployment/provider"
	gatewayprovider "github.com/vllm-project/aibrix/apps/console/api/gateway/provider"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

type gatewayClients struct {
	client    gatewayclient.Interface
	namespace string
}

func (c gatewayClients) GatewayClient() (gatewayclient.Interface, string, error) {
	return c.client, c.namespace, nil
}

func gatewayMux(t *testing.T, client gatewayclient.Interface, namespace string) *runtime.ServeMux {
	t.Helper()
	mux := runtime.NewServeMux()
	h := NewGatewayInstanceHandler(gatewayprovider.NewKubernetes(gatewayClients{client, namespace}))
	if err := h.RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestGatewayInstanceAPI(t *testing.T) {
	client := gatewayfake.NewSimpleClientset()
	mux := gatewayMux(t, client, "inference")
	create := []byte(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway",
		"metadata":{"name":"public","annotations":{"example.org/owner":"team"}},
		"spec":{"gatewayClassName":"eg","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`)
	created := serveModelAdapterRequest(t, mux, http.MethodPost, gatewayInstancePath, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	gateways := client.GatewayV1().Gateways("inference")
	current, err := gateways.Get(context.Background(), "public", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Fake clients do not implement API-server resource versions or generations.
	// Seed controller-observed state explicitly to test what the BFF preserves.
	current.ResourceVersion = "10"
	current.Generation = 2
	current.Finalizers = []string{"example.org/protect"}
	current.Status.Conditions = []metav1.Condition{{
		Type: "Programmed", Status: metav1.ConditionTrue,
		ObservedGeneration: 1, Reason: "Programmed", LastTransitionTime: metav1.Now(),
	}}
	if _, err := gateways.Update(context.Background(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	get := serveModelAdapterRequest(t, mux, http.MethodGet, gatewayInstancePath+"/public", nil)
	var result gatewayv1.Gateway
	if err := json.Unmarshal(get.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if get.Code != http.StatusOK || result.Namespace != "inference" ||
		result.Status.Conditions[0].ObservedGeneration != 1 || result.Generation != 2 {
		t.Fatalf("controller state was not preserved: %s", get.Body.String())
	}
	result.Spec.Listeners[0].Port = 8080
	result.Status.Conditions = nil
	result.Finalizers = nil
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	updated := serveModelAdapterRequest(t, mux, http.MethodPut, gatewayInstancePath+"/public", body)
	if updated.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updated.Code, updated.Body.String())
	}
	saved, err := gateways.Get(context.Background(), "public", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Spec.Listeners[0].Port != 8080 || len(saved.Finalizers) != 1 ||
		len(saved.Status.Conditions) != 1 || saved.Annotations["example.org/owner"] != "team" {
		t.Fatalf("unexpected persisted Gateway: %#v", saved)
	}
	// Resources from other namespaces must never appear in the collection.
	other := current.DeepCopy()
	other.Namespace = "other"
	if _, err := client.GatewayV1().Gateways("other").Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	list := serveModelAdapterRequest(t, mux, http.MethodGet, gatewayInstancePath, nil)
	var collection gatewayv1.GatewayList
	if err := json.Unmarshal(list.Body.Bytes(), &collection); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(collection.Items) != 1 || collection.Items[0].Namespace != "inference" {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
}

func TestGatewayInstanceAPIRejectsUnsafeWrites(t *testing.T) {
	base := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "inference", ResourceVersion: "10"},
		Spec: gatewayv1.GatewaySpec{GatewayClassName: "eg", Listeners: []gatewayv1.Listener{{
			Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
		}}},
	}
	for _, tc := range []struct {
		name   string
		role   string
		edit   func(*gatewayv1.Gateway)
		status int
	}{
		{"anonymous", "", func(*gatewayv1.Gateway) {}, http.StatusUnauthorized},
		{"viewer", "viewer", func(*gatewayv1.Gateway) {}, http.StatusForbidden},
		{"stale version", "admin", func(g *gatewayv1.Gateway) { g.ResourceVersion = "9" }, http.StatusConflict},
		{"missing version", "admin", func(g *gatewayv1.Gateway) { g.ResourceVersion = "" }, http.StatusBadRequest},
		{"another namespace", "admin", func(g *gatewayv1.Gateway) { g.Namespace = "other" }, http.StatusBadRequest},
		{"different name", "admin", func(g *gatewayv1.Gateway) { g.Name = "another" }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := gatewayfake.NewSimpleClientset()
			// v1.0 aliases Gateway to v1beta1; seed through the v1 client so
			// the fake tracker stores it under the version used by the BFF.
			if _, err := client.GatewayV1().Gateways("inference").Create(context.Background(), base.DeepCopy(), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			mux := gatewayMux(t, client, "inference")
			request := base.DeepCopy()
			tc.edit(request)
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			response := serveModelAdapterRequestAsRole(t, mux, http.MethodPut, gatewayInstancePath+"/public", body, tc.role)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "update" || action.GetVerb() == "create" {
					t.Fatalf("rejected request wrote to Kubernetes: %#v", action)
				}
			}
		})
	}
}

func TestGatewayInstanceAPIPropagatesServerConflict(t *testing.T) {
	client := gatewayfake.NewSimpleClientset()
	if _, err := client.GatewayV1().Gateways("inference").Create(context.Background(), &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "inference", ResourceVersion: "10"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "eg"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "gateways", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		request := action.(k8stesting.UpdateAction).GetObject().(*gatewayv1.Gateway)
		if request.ResourceVersion != "10" {
			t.Fatalf("lost optimistic concurrency: %q", request.ResourceVersion)
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"}, "public", fmt.Errorf("concurrent controller write"),
		)
	})
	mux := gatewayMux(t, client, "inference")
	body := []byte(`{"metadata":{"name":"public","resourceVersion":"10"},"spec":{"gatewayClassName":"eg","listeners":[]}}`)
	response := serveModelAdapterRequest(t, mux, http.MethodPut, gatewayInstancePath+"/public", body)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestGatewayInstanceAPIMalformedRequests(t *testing.T) {
	client := gatewayfake.NewSimpleClientset()
	mux := gatewayMux(t, client, "inference")
	for _, body := range []string{"null", `{}`, `{"unexpected":true}`, `{} {}`} {
		response := serveModelAdapterRequest(t, mux, http.MethodPost, gatewayInstancePath, []byte(body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d %s", body, response.Code, response.Body.String())
		}
	}
	missing := serveModelAdapterRequest(t, mux, http.MethodGet, gatewayInstancePath+"/missing", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", missing.Code)
	}
	unscoped := gatewayMux(t, client, "")
	response := serveModelAdapterRequest(t, unscoped, http.MethodGet, gatewayInstancePath, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty namespace: %d", response.Code)
	}
}

func TestGatewayInstanceAPIWithTypedClient(t *testing.T) {
	const wireGateway = `{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway",
		"metadata":{"name":"public","namespace":"inference","resourceVersion":"10"},
		"spec":{"gatewayClassName":"eg","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			if r.URL.Query().Get("fieldValidation") != "Strict" {
				t.Error("write did not request strict API-server validation")
			}
			var object gatewayv1.Gateway
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				t.Error(err)
			}
			if object.Spec.Infrastructure != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"BadRequest","code":400,"message":"unknown field spec.infrastructure"}`)
				return
			}
			if object.Spec.GatewayClassName != "corrected" {
				t.Error("class edit was not forwarded to admission")
			}
		}
		if r.Method == http.MethodGet && r.URL.Path == "/apis/gateway.networking.k8s.io/v1/namespaces/inference/gateways" {
			_, _ = fmt.Fprintf(w, `{"apiVersion":"gateway.networking.k8s.io/v1","kind":"GatewayList","items":[%s]}`, wireGateway)
			return
		}
		_, _ = fmt.Fprint(w, wireGateway)
	}))
	defer server.Close()
	client, err := gatewayclient.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	mux := gatewayMux(t, client, "inference")
	for _, tc := range []struct{ method, suffix, body, kind string }{
		{http.MethodGet, "", "", "GatewayList"},
		{http.MethodGet, "/public", "", "Gateway"},
		{http.MethodPost, "", `{"metadata":{"name":"public"},"spec":{"gatewayClassName":"corrected"}}`, "Gateway"},
		{http.MethodPut, "/public", `{"metadata":{"name":"public","resourceVersion":"10"},"spec":{"gatewayClassName":"corrected"}}`, "Gateway"},
	} {
		response := serveModelAdapterRequest(t, mux, tc.method, gatewayInstancePath+tc.suffix, []byte(tc.body))
		var object map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
			t.Fatal(err)
		}
		if response.Code >= 400 || object["apiVersion"] != gatewayv1.GroupVersion.String() || object["kind"] != tc.kind {
			t.Fatalf("%s: %d %s", tc.method, response.Code, response.Body.String())
		}
	}
	body := []byte(`{"metadata":{"name":"public"},"spec":{"gatewayClassName":"eg","infrastructure":{"labels":{"team":"test"}}}}`)
	response := serveModelAdapterRequest(t, mux, http.MethodPost, gatewayInstancePath, body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported CRD field: %d %s", response.Code, response.Body.String())
	}
}

func TestGatewayInstanceAPIInvalidKubeconfig(t *testing.T) {
	clients := deploymentprovider.NewGatewayClientProvider(config.KubernetesProviderConfig{
		Kubeconfig: filepath.Join(t.TempDir(), "missing"), Namespace: "inference",
	})
	mux := runtime.NewServeMux()
	if err := NewGatewayInstanceHandler(gatewayprovider.NewKubernetes(clients)).RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	response := serveModelAdapterRequest(t, mux, http.MethodGet, gatewayInstancePath, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid server configuration: %d %s", response.Code, response.Body.String())
	}
}
