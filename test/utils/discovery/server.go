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

// Package discovery supplies a strict Kubernetes HTTP fixture for discovery tests.
package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// NewServer serves only GET requests for one namespace and its discovery resources.
// Lists deliberately return all supplied objects, even foreign ones, to exercise
// defensive event filtering. Intercept can handle a request to simulate failure.
// Watches remain open until cancellation. Cleanup closes the server.
func NewServer(t testing.TB, namespace string, pods []corev1.Pod, adapters []modelv1alpha1.ModelAdapter,
	intercept func(http.ResponseWriter, *http.Request) bool) *rest.Config {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		var object interface{}
		switch r.URL.Path {
		case "/api/v1/namespaces/" + namespace:
			object = &corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			}
		case "/api/v1/namespaces/" + namespace + "/pods":
			object = &corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: pods,
			}
		case "/apis/model.aibrix.ai/v1alpha1/namespaces/" + namespace + "/modeladapters":
			object = &modelv1alpha1.ModelAdapterList{
				TypeMeta: metav1.TypeMeta{APIVersion: "model.aibrix.ai/v1alpha1", Kind: "ModelAdapterList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: adapters,
			}
		case "/apis/model.aibrix.ai/v1alpha1/namespaces/" + namespace + "/modelclaims":
			object = &modelv1alpha1.ModelClaimList{
				TypeMeta: metav1.TypeMeta{APIVersion: "model.aibrix.ai/v1alpha1", Kind: "ModelClaimList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			}
		default:
			t.Errorf("unexpected discovery path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if err := json.NewEncoder(w).Encode(object); err != nil {
			t.Errorf("encode discovery response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return &rest.Config{Host: server.URL}
}
