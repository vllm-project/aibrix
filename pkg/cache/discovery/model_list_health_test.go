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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestModelListHealthDetectsMissedDeleteAndRecovers(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "served", UID: "one", ResourceVersion: "10",
	}}
	h := newModelListHealth(time.Minute)
	h.Apply(WatchEvent{Type: EventAdd, Object: pod}, func(WatchEvent) bool { return true })
	current := sourceVersions{
		pods:     map[string]objectVersion{"default/served": {uid: "one", resourceVersion: "10"}},
		adapters: map[string]objectVersion{},
	}
	checks := 0
	h.snapshot = func(context.Context) (sourceVersions, error) {
		checks++
		return current, nil
	}
	require.NoError(t, h.EnsureVerified(context.Background()))
	models, ok := h.ModelsIfHealthy(func() []string { return []string{"served"} })
	require.True(t, ok)
	assert.Equal(t, []string{"served"}, models)
	require.NoError(t, h.EnsureVerified(context.Background()))
	assert.Equal(t, 1, checks, "fresh proof avoids another Kubernetes LIST")

	// Kubernetes deleted the Pod but the watch did not deliver its event.
	current.pods = map[string]objectVersion{}
	h.mu.Lock()
	h.lastVerified = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	require.ErrorIs(t, h.EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	_, ok = h.ModelsIfHealthy(func() []string { return []string{"stale"} })
	assert.False(t, ok, "a stale model list must not be returned as success")
	assert.Equal(t, 2, checks)
	require.ErrorIs(t, h.EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	assert.Equal(t, 2, checks, "failed verification has a bounded retry delay")

	h.Apply(WatchEvent{Type: EventDelete, Object: pod}, func(WatchEvent) bool { return true })
	h.mu.Lock()
	h.retryAfter = time.Time{}
	h.mu.Unlock()
	require.NoError(t, h.EnsureVerified(context.Background()))
	_, ok = h.ModelsIfHealthy(func() []string { return nil })
	assert.True(t, ok, "an empty but verified list is healthy")
}

func TestModelListHealthInvalidatesOnError(t *testing.T) {
	h := newModelListHealth(time.Hour)
	checks := 0
	h.snapshot = func(context.Context) (sourceVersions, error) {
		checks++
		return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
	}
	require.NoError(t, h.EnsureVerified(context.Background()))
	h.Invalidate()
	require.NoError(t, h.EnsureVerified(context.Background()))
	assert.Equal(t, 2, checks)

	h.Apply(WatchEvent{Type: EventAdd, Object: &v1.Pod{}}, func(WatchEvent) bool { return false })
	_, ok := h.ModelsIfHealthy(func() []string { return nil })
	assert.False(t, ok, "a failed cache handler invalidates the proof")
}

func TestModelListHealthRejectsNilRequiredObjects(t *testing.T) {
	for _, test := range []struct {
		name   string
		object any
	}{
		{name: "Pod", object: (*v1.Pod)(nil)},
		{name: "ModelAdapter", object: (*modelv1alpha1.ModelAdapter)(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newModelListHealth(time.Hour)
			checks := 0
			h.snapshot = func(context.Context) (sourceVersions, error) {
				checks++
				return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
			}
			require.NoError(t, h.EnsureVerified(context.Background()))

			handlerCalled := false
			h.Apply(WatchEvent{Type: EventAdd, Object: test.object}, func(WatchEvent) bool {
				handlerCalled = true
				return true
			})
			assert.False(t, handlerCalled, "invalid objects must not reach the cache handler")
			_, healthy := h.ModelsIfHealthy(func() []string { return nil })
			assert.False(t, healthy, "an unapplied event invalidates the previous proof")
			require.NoError(t, h.EnsureVerified(context.Background()))
			assert.Equal(t, 2, checks, "recovery requires a new snapshot")
		})
	}
}

func TestModelListHealthChecksEnabledAdapterVersions(t *testing.T) {
	h := newModelListHealth(time.Minute)
	adapter := &modelv1alpha1.ModelAdapter{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "adapter", UID: "one", ResourceVersion: "4",
	}}
	h.Apply(WatchEvent{Type: EventAdd, Object: adapter}, func(WatchEvent) bool { return true })
	h.snapshot = func(context.Context) (sourceVersions, error) {
		return sourceVersions{
			pods: map[string]objectVersion{},
			adapters: map[string]objectVersion{
				"default/adapter": {uid: "one", resourceVersion: "5"},
			},
		}, nil
	}
	require.ErrorIs(t, h.EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	_, ok := h.ModelsIfHealthy(func() []string { return []string{"adapter"} })
	assert.False(t, ok)
}

func TestModelListHealthVerificationFailureAndCancellation(t *testing.T) {
	h := newModelListHealth(time.Minute)
	h.snapshot = func(ctx context.Context) (sourceVersions, error) {
		<-ctx.Done()
		return sourceVersions{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.EnsureVerified(ctx), context.DeadlineExceeded)
	_, ok := h.ModelsIfHealthy(func() []string { return []string{"stale"} })
	assert.False(t, ok)

	h.retryAfter = time.Time{}
	h.snapshot = func(context.Context) (sourceVersions, error) {
		return sourceVersions{}, errors.New("forbidden")
	}
	require.ErrorContains(t, h.EnsureVerified(context.Background()), "forbidden")
	_, ok = h.ModelsIfHealthy(func() []string { return nil })
	assert.False(t, ok)
}

func TestModelListHealthErrorDuringVerificationCannotClearFailure(t *testing.T) {
	h := newModelListHealth(time.Minute)
	h.snapshot = func(context.Context) (sourceVersions, error) {
		h.Invalidate()
		return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
	}
	require.ErrorIs(t, h.EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	_, ok := h.ModelsIfHealthy(func() []string { return nil })
	assert.False(t, ok)
}

func TestListPodVersionsUsesCompletePaginatedSnapshot(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		list := &v1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
		switch r.URL.Query().Get("continue") {
		case "":
			list.Continue = "next"
			list.Items = []v1.Pod{{ObjectMeta: metav1.ObjectMeta{
				Namespace: "one", Name: "pod", UID: "first", ResourceVersion: "1",
			}}}
		case "next":
			list.Items = []v1.Pod{{ObjectMeta: metav1.ObjectMeta{
				Namespace: "two", Name: "pod", UID: "second", ResourceVersion: "2",
			}}}
		default:
			http.Error(w, "unexpected continuation", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	versions, err := listPodVersions(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, 2, pages)
	assert.Equal(t, map[string]objectVersion{
		"one/pod": {uid: "first", resourceVersion: "1"},
		"two/pod": {uid: "second", resourceVersion: "2"},
	}, versions)
}

func TestModelListHealthSnapshotWaitsForAppliedEvent(t *testing.T) {
	h := newModelListHealth(time.Minute)
	h.snapshot = func(context.Context) (sourceVersions, error) {
		return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
	}
	require.NoError(t, h.EnsureVerified(context.Background()))
	reading := make(chan struct{})
	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		_, _ = h.ModelsIfHealthy(func() []string {
			close(reading)
			<-release
			return nil
		})
	}()
	<-reading
	applied := make(chan struct{})
	go func() {
		h.Apply(WatchEvent{Type: EventAdd, Object: &v1.Pod{}}, func(WatchEvent) bool {
			close(applied)
			return true
		})
	}()
	select {
	case <-applied:
		t.Fatal("event handler ran in the middle of a verified model-list read")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	done.Wait()
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("event handler did not resume after model-list read")
	}
}
