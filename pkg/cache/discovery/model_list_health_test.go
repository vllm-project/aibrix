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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	metadatafake "k8s.io/client-go/metadata/fake"
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
	var accepts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		accepts = append(accepts, r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		list := &metav1.PartialObjectMetadataList{
			TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadataList"},
		}
		item := func(namespace, uid, rv string) metav1.PartialObjectMetadata {
			return metav1.PartialObjectMetadata{
				TypeMeta:   metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"},
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "pod", UID: types.UID(uid), ResourceVersion: rv},
			}
		}
		switch r.URL.Query().Get("continue") {
		case "":
			list.Continue = "next"
			list.Items = []metav1.PartialObjectMetadata{item("one", "first", "1")}
		case "next":
			list.Items = []metav1.PartialObjectMetadata{item("two", "second", "2")}
		default:
			http.Error(w, "unexpected continuation", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer server.Close()
	client, err := metadata.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	versions, err := listPodVersions(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, 2, pages)
	assert.Equal(t, map[string]objectVersion{
		"one/pod": {uid: "first", resourceVersion: "1"},
		"two/pod": {uid: "second", resourceVersion: "2"},
	}, versions)
	for _, accept := range accepts {
		// The API server returns full objects unless the request asks for
		// metadata; this is what keeps spec and status off the wire.
		assert.Contains(t, accept, "as=PartialObjectMetadataList")
	}
}

func metadataObject(apiVersion, kind, namespace, name, uid, rv string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(uid), ResourceVersion: rv,
		},
	}
}

func TestModelListHealthMetadataSnapshotMatchAndMismatch(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid", ResourceVersion: "7"}}
	adapter := &modelv1alpha1.ModelAdapter{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "adapter", UID: "adapter-uid", ResourceVersion: "3"}}
	applied := func(h *ModelListHealth) {
		h.Apply(WatchEvent{Type: EventAdd, Object: pod}, func(WatchEvent) bool { return true })
		h.Apply(WatchEvent{Type: EventAdd, Object: adapter}, func(WatchEvent) bool { return true })
	}
	scheme := metadatafake.NewTestScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))
	adapterAPIVersion := modelv1alpha1.GroupVersion.String()

	t.Run("match", func(t *testing.T) {
		client := metadatafake.NewSimpleMetadataClient(scheme,
			metadataObject("v1", "Pod", "default", "pod", "pod-uid", "7"),
			metadataObject(adapterAPIVersion, "ModelAdapter", "default", "adapter", "adapter-uid", "3"))
		provider := NewKubernetesProvider(&rest.Config{}).WithModelListHealth(time.Minute)
		provider.configureModelListHealth(client)
		applied(provider.ModelListHealth())
		require.NoError(t, provider.ModelListHealth().EnsureVerified(context.Background()))
	})

	t.Run("mismatch", func(t *testing.T) {
		// The cache missed an update: the API server has resourceVersion 8.
		client := metadatafake.NewSimpleMetadataClient(scheme,
			metadataObject("v1", "Pod", "default", "pod", "pod-uid", "8"),
			metadataObject(adapterAPIVersion, "ModelAdapter", "default", "adapter", "adapter-uid", "3"))
		provider := NewKubernetesProvider(&rest.Config{}).WithModelListHealth(time.Minute)
		provider.configureModelListHealth(client)
		applied(provider.ModelListHealth())
		require.ErrorIs(t, provider.ModelListHealth().EnsureVerified(context.Background()), ErrModelListDiscoveryUnavailable)
	})

	t.Run("adapters ignored when not watched", func(t *testing.T) {
		client := metadatafake.NewSimpleMetadataClient(scheme,
			metadataObject("v1", "Pod", "default", "pod", "pod-uid", "7"),
			metadataObject(adapterAPIVersion, "ModelAdapter", "default", "adapter", "adapter-uid", "3"))
		provider := NewKubernetesProvider(&rest.Config{}).WithModelAdapters(false).WithModelListHealth(time.Minute)
		provider.configureModelListHealth(client)
		provider.ModelListHealth().Apply(WatchEvent{Type: EventAdd, Object: pod}, func(WatchEvent) bool { return true })
		require.NoError(t, provider.ModelListHealth().EnsureVerified(context.Background()))
	})
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

// slowSnapshot blocks until its context ends, like a full LIST on a cluster
// too large for the verification budget, and counts how often it starts.
func slowSnapshot(calls *atomic.Int32) func(context.Context) (sourceVersions, error) {
	return func(ctx context.Context) (sourceVersions, error) {
		calls.Add(1)
		<-ctx.Done()
		return sourceVersions{}, ctx.Err()
	}
}

func TestModelListHealthSlowVerificationBacksOffWithoutCallerDeadline(t *testing.T) {
	// Review D1 on #3555: under a caller deadline shorter than the
	// verification budget, every call started a new full snapshot and
	// retryAfter was never set. The discovery checker now calls without a
	// deadline, so the verification's own budget expires instead, and the
	// backoff must hold.
	h := newModelListHealth(time.Minute)
	h.verifyTimeout = 20 * time.Millisecond
	var calls atomic.Int32
	h.snapshot = slowSnapshot(&calls)

	for i := 0; i < 5; i++ {
		require.Error(t, h.EnsureVerified(context.Background()))
	}
	assert.Equal(t, int32(1), calls.Load(), "calls within the retry delay must not start new snapshots")
	_, retryLater, _ := h.verificationState(time.Now())
	assert.True(t, retryLater, "a verification that ran out of its own budget sets retryAfter")
}

func TestModelListHealthCallerDeadlineSkipsBackoff(t *testing.T) {
	// The contrast case, and the reason the checker must not pass a deadline:
	// a caller that gives up is treated as gone, so no backoff applies.
	h := newModelListHealth(time.Minute)
	var calls atomic.Int32
	h.snapshot = slowSnapshot(&calls)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		require.Error(t, h.EnsureVerified(ctx))
		cancel()
	}
	assert.Equal(t, int32(3), calls.Load())
}

func TestModelListHealthWaiterReusesInFlightVerification(t *testing.T) {
	// Review contention case: the discovery checker calling while a
	// /v1/models verification runs waits for it and reuses its proof, with
	// no second snapshot and no error.
	h := newModelListHealth(time.Minute)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	h.snapshot = func(context.Context) (sourceVersions, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
	}
	first := make(chan error, 1)
	go func() { first <- h.EnsureVerified(context.Background()) }()
	<-started

	second := make(chan error, 1)
	go func() { second <- h.EnsureVerified(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.Equal(t, int32(1), calls.Load())
}

func emptyVersions() sourceVersions {
	return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}
}

func TestModelListHealthDeferredDuringRetryDelay(t *testing.T) {
	// The discovery checker must tell "this call ran a LIST and it failed"
	// from "a recent failure's retry delay is running" (review of #3555).
	h := newModelListHealth(time.Minute)
	var calls atomic.Int32
	h.snapshot = func(context.Context) (sourceVersions, error) {
		calls.Add(1)
		return sourceVersions{}, errors.New("connection refused")
	}
	err := h.EnsureVerified(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrModelListVerificationDeferred)

	err = h.EnsureVerified(context.Background())
	require.ErrorIs(t, err, ErrModelListVerificationDeferred)
	require.ErrorIs(t, err, ErrModelListDiscoveryUnavailable, "callers matching unavailable still match")
	assert.Equal(t, int32(1), calls.Load(), "a deferred call makes no request")
}

func TestModelListHealthLastMatchedSurvivesFailures(t *testing.T) {
	h := newModelListHealth(time.Millisecond)
	assert.True(t, h.LastMatched().IsZero())

	h.snapshot = func(context.Context) (sourceVersions, error) { return emptyVersions(), nil }
	before := time.Now()
	require.NoError(t, h.EnsureVerified(context.Background()))
	matched := h.LastMatched()
	assert.False(t, matched.Before(before), "counted from the start of the verification")

	time.Sleep(2 * time.Millisecond) // let the 1 ms proof expire
	h.snapshot = func(context.Context) (sourceVersions, error) {
		return sourceVersions{}, errors.New("connection refused")
	}
	require.Error(t, h.EnsureVerified(context.Background()))
	h.Invalidate()
	assert.Equal(t, matched, h.LastMatched(), "a failure or watch error does not move the last match")

	// A fresh proof answered without a LIST does not move it either.
	h2 := newModelListHealth(time.Minute)
	var calls atomic.Int32
	h2.snapshot = func(context.Context) (sourceVersions, error) { calls.Add(1); return emptyVersions(), nil }
	require.NoError(t, h2.EnsureVerified(context.Background()))
	first := h2.LastMatched()
	require.NoError(t, h2.EnsureVerified(context.Background()))
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, first, h2.LastMatched())
}
