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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	discoverytest "github.com/vllm-project/aibrix/test/utils/discovery"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestKubernetesNamespaceRequired(t *testing.T) {
	for _, namespace := range []string{"", " ", "*", "team/a", "Team-A", "team-a ", strings.Repeat("a", 64)} {
		t.Run(namespace, func(t *testing.T) {
			provider := NewKubernetesProvider(&rest.Config{Host: "http://unused.invalid"}, namespace)
			require.ErrorContains(t, provider.Watch(func(WatchEvent) { t.Error("unexpected event") }, nil), "installation namespace")
		})
	}
	require.NoError(t, ValidateNamespace("team-a"))
	require.ErrorContains(t, NewKubernetesProvider(nil, "team-a").Watch(nil, nil), "configuration is required")
}

func TestKubernetesDiscoveryFiltersForeignObjects(t *testing.T) {
	const namespace = "team-a"
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: namespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a-other"}},
	}
	adapters := []modelv1alpha1.ModelAdapter{
		{ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: namespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "foreign-adapter", Namespace: "team-a-other"}},
	}
	config := discoverytest.NewServer(t, namespace, pods, adapters, nil)
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var events []WatchEvent
	provider := NewKubernetesProvider(config, namespace)
	require.NoError(t, provider.Watch(func(event WatchEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}, stop))
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
	handler := func(WatchEvent) { delivered++ }
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
