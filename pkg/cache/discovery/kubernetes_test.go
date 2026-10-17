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

package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
	discoverytest "github.com/vllm-project/aibrix/test/utils/discovery"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesNamespaceRequired(t *testing.T) {
	for _, namespace := range []string{"", " ", "*", "team/a", "Team-A", "team-a ", strings.Repeat("a", 64)} {
		t.Run(namespace, func(t *testing.T) {
			provider := NewKubernetesProvider(&rest.Config{Host: "http://unused.invalid"}, namespace)
			require.ErrorContains(t, provider.Watch(func(WatchEvent) { t.Error("unexpected event") }, nil), "installation namespace")
		})
	}
	require.NoError(t, ValidateNamespace("team-a"))
	require.ErrorContains(t, NewKubernetesProvider(nil, "team-a").Watch(func(WatchEvent) {}, nil), "configuration is required")
}

func TestKubernetesDiscoveryFiltersForeignObjects(t *testing.T) {
	const namespace = "team-a"
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: namespace, UID: "pod", ResourceVersion: "1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a-other"}},
	}
	adapters := []modelv1alpha1.ModelAdapter{
		{ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: namespace, UID: "adapter", ResourceVersion: "1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "foreign-adapter", Namespace: "team-a-other"}},
	}
	config := discoverytest.NewServer(t, namespace, pods, adapters, nil)
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var events []WatchEvent
	provider := NewKubernetesProvider(config, namespace).WithModelClaims().WithModelListHealth(time.Minute)
	require.NoError(t, provider.WatchApplied(func(event WatchEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		return true
	}, stop))
	require.NoError(t, provider.ModelListHealth().EnsureVerified(context.Background()))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 3) // Pod + adapter initial events, then adapter reconciliation.
	for _, event := range events {
		object, err := meta.Accessor(event.Object)
		require.NoError(t, err)
		require.Equal(t, namespace, object.GetNamespace())
	}
}

func TestKubernetesStartupAccessFailures(t *testing.T) {
	for _, tc := range []struct {
		name, resource string
		watch          bool
		status         int
	}{
		{"namespace missing", "/team-a", false, http.StatusNotFound},
		{"namespace denied", "/team-a", false, http.StatusForbidden},
		{"pod list denied", "/pods", false, http.StatusForbidden},
		{"pod watch denied", "/pods", true, http.StatusForbidden},
		{"adapter list denied", "/modeladapters", false, http.StatusForbidden},
		{"adapter watch denied", "/modeladapters", true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := discoverytest.NewServer(t, "team-a", nil, nil, func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, tc.resource) && (r.URL.Query().Get("watch") == "true") == tc.watch {
					http.Error(w, "access failed", tc.status)
					return true
				}
				return false
			})
			provider := NewKubernetesProvider(config, "team-a")
			provider.startupTimeout = 5 * time.Second
			require.Error(t, provider.Watch(func(WatchEvent) {}, nil))
		})
	}
}

func TestKubernetesStartupCancelsBlockedRequests(t *testing.T) {
	for _, resource := range []string{"/team-a", "/pods", "/modeladapters"} {
		t.Run(resource, func(t *testing.T) {
			canceled := make(chan struct{}, 4)
			config := discoverytest.NewServer(t, "team-a", nil, nil, func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, resource) || r.URL.Query().Get("watch") == "true" {
					return false
				}
				<-r.Context().Done()
				canceled <- struct{}{}
				return true
			})
			provider := NewKubernetesProvider(config, "team-a")
			provider.startupTimeout = 150 * time.Millisecond
			err := provider.Watch(func(WatchEvent) {}, nil)
			require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("startup left an API request running")
			}
		})
	}
}

func TestKubernetesEventsRejectForeignUpdatesAndDeletes(t *testing.T) {
	local := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a"}}
	foreign := local.DeepCopy()
	foreign.Namespace = "team-a-other"
	provider := NewKubernetesProvider(nil, "team-a")
	var delivered int
	handler := func(WatchEvent) bool { delivered++; return true }
	for _, event := range []WatchEvent{
		{Type: EventAdd, Object: foreign},
		{Type: EventUpdate, Object: foreign, OldObject: foreign},
		{Type: EventUpdate, Object: local, OldObject: foreign},
		{Type: EventDelete, Object: foreign},
	} {
		provider.deliver(handler, event)
	}
	require.Zero(t, delivered)
	provider.deliver(handler, WatchEvent{Type: EventUpdate, Object: local, OldObject: local})
	provider.deliver(handler, WatchEvent{Type: EventDelete, Object: local})
	require.Equal(t, 2, delivered)
}

func TestKubernetesProviderVerificationDetectsSilentWatchLoss(t *testing.T) {
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/default" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
			})
			return
		}

		if r.URL.Path != "/api/v1/namespaces/default/pods" {
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
		list := &corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			ListMeta: metav1.ListMeta{ResourceVersion: "10"},
		}
		if !deleted.Load() {
			list.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
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

	provider := NewKubernetesProvider(&rest.Config{Host: server.URL}, "default").
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
			assert.Equal(t, tc.want, canListModelClaims(context.Background(), client, "workloads"))
			require.Equal(t, "workloads", client.Actions()[0].GetNamespace())
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
				if r.URL.Path == "/api/v1/namespaces/default" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(&corev1.Namespace{
						TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
						ObjectMeta: metav1.ObjectMeta{Name: "default"},
					})
					return
				}

				adapterRequest := r.URL.Path == "/apis/model.aibrix.ai/v1alpha1/namespaces/default/modeladapters"
				claimRequest := r.URL.Path == "/apis/model.aibrix.ai/v1alpha1/namespaces/default/modelclaims"
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
				switch r.URL.Path {
				case "/api/v1/namespaces/default/pods":
					_ = json.NewEncoder(w).Encode(&corev1.PodList{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
				case "/apis/model.aibrix.ai/v1alpha1/namespaces/default/modeladapters":
					_ = json.NewEncoder(w).Encode(&modelv1alpha1.ModelAdapterList{
						TypeMeta: metav1.TypeMeta{APIVersion: "model.aibrix.ai/v1alpha1", Kind: "ModelAdapterList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
				case "/apis/model.aibrix.ai/v1alpha1/namespaces/default/modelclaims":
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

			provider := NewKubernetesProvider(&rest.Config{Host: server.URL}, "default")
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

func TestKubernetesClaimEventsRejectForeignNamespaces(t *testing.T) {
	local := &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a"}}
	foreign := local.DeepCopy()
	foreign.Namespace = "other"
	provider := NewKubernetesProvider(nil, "team-a").WithModelClaims().WithModelListHealth(time.Minute)
	var delivered int
	handler := func(WatchEvent) bool { delivered++; return true }
	for _, event := range []WatchEvent{
		{Type: EventAdd, Object: foreign},
		{Type: EventUpdate, Object: foreign, OldObject: local},
		{Type: EventUpdate, Object: local, OldObject: foreign},
		{Type: EventDelete, Object: foreign},
	} {
		provider.deliver(handler, event)
	}
	require.Zero(t, delivered)
	provider.deliver(handler, WatchEvent{Type: EventAdd, Object: local})
	provider.deliver(handler, WatchEvent{Type: EventUpdate, Object: local, OldObject: local})
	provider.deliver(handler, WatchEvent{Type: EventDelete, Object: local})
	require.Equal(t, 3, delivered)
}
