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

package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
)

func TestKubernetesProviderVerificationDetectsSilentWatchLoss(t *testing.T) {
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pods") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		list := &v1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			ListMeta: metav1.ListMeta{ResourceVersion: "10"},
		}
		if !deleted.Load() {
			list.Items = []v1.Pod{{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "served", UID: "one", ResourceVersion: "10",
			}}}
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
		server.CloseClientConnections()
		server.Close()
	})

	provider := NewKubernetesProvider(&rest.Config{Host: server.URL}).
		WithModelAdapters(false).WithModelListHealth(time.Minute)
	result := make(chan error, 1)
	go func() {
		result <- provider.WatchApplied(func(WatchEvent) bool { return true }, stopCh)
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not finish initial handler sync")
	}
	h := provider.ModelListHealth()
	require.NoError(t, h.EnsureVerified(context.Background()))
	deleted.Store(true) // The API server changed, but the watch sent no delete.
	h.mu.Lock()
	h.lastVerified = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	require.ErrorIs(t, h.EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	_, ok := h.ModelsIfHealthy(func() []string { return []string{"served"} })
	assert.False(t, ok)
}

func TestCanListModelClaims(t *testing.T) {
	claims := schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"listable", nil, true},
		{"the role may not list claims", apierrors.NewForbidden(claims, "", errors.New("no rule")), false},
		{"the CRD is not installed", apierrors.NewNotFound(claims, ""), false},
		// Anything else may pass, so the informer is left to retry it.
		{"a transient error", apierrors.NewServiceUnavailable("try again"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			if tc.err != nil {
				client.PrependReactor("list", "modelclaims",
					func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			}
			assert.Equal(t, tc.want, canListModelClaims(client))
		})
	}
}

func TestKubernetesProviderQueriesOnlyEnabledResources(t *testing.T) {
	for _, tc := range []struct {
		name          string
		watchAdapters bool
		watchClaims   bool
		useDefaults   bool
	}{
		{name: "default adapters", watchAdapters: true, useDefaults: true},
		{name: "pods only"},
		{name: "claims only", watchClaims: true},
		{name: "both resources", watchAdapters: true, watchClaims: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var adapterRequests, claimRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				adapterRequest := strings.HasSuffix(r.URL.Path, "/modeladapters")
				claimRequest := strings.HasSuffix(r.URL.Path, "/modelclaims")
				if adapterRequest {
					adapterRequests.Add(1)
				}
				if claimRequest {
					claimRequests.Add(1)
				}
				if (adapterRequest && !tc.watchAdapters) || (claimRequest && !tc.watchClaims) {
					http.Error(w, "resource is unavailable", http.StatusForbidden)
					return
				}
				if r.URL.Query().Get("watch") == "true" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/pods"):
					_ = json.NewEncoder(w).Encode(&v1.PodList{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
				case strings.HasSuffix(r.URL.Path, "/modeladapters"):
					_ = json.NewEncoder(w).Encode(&modelv1alpha1.ModelAdapterList{
						TypeMeta: metav1.TypeMeta{APIVersion: "model.aibrix.ai/v1alpha1", Kind: "ModelAdapterList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
				case strings.HasSuffix(r.URL.Path, "/modelclaims"):
					_ = json.NewEncoder(w).Encode(&modelv1alpha1.ModelClaimList{
						TypeMeta: metav1.TypeMeta{APIVersion: "model.aibrix.ai/v1alpha1", Kind: "ModelClaimList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
				default:
					http.NotFound(w, r)
				}
			}))
			stopCh := make(chan struct{})
			t.Cleanup(func() {
				close(stopCh)
				server.CloseClientConnections()
				server.Close()
			})

			provider := NewKubernetesProvider(&rest.Config{Host: server.URL})
			if !tc.useDefaults {
				provider.WithModelAdapters(tc.watchAdapters)
			}
			if tc.watchClaims {
				provider.WithModelClaims()
			}
			result := make(chan error, 1)
			go func() { result <- provider.Watch(func(WatchEvent) {}, stopCh) }()
			select {
			case err := <-result:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Kubernetes provider did not finish initial sync")
			}
			if tc.watchAdapters {
				assert.Positive(t, adapterRequests.Load())
			} else {
				assert.Zero(t, adapterRequests.Load(), "disabled adapters must not be queried")
			}
			if tc.watchClaims {
				assert.Positive(t, claimRequests.Load())
			} else {
				assert.Zero(t, claimRequests.Load(), "disabled claims must not be queried")
			}
		})
	}
}
