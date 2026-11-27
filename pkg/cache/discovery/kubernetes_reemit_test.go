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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// staleListStore models the post-sync window. List and ListKeys return the
// snapshot taken before the delete and update, and GetByKey reflects the live
// store (the embedded cache.Store), which has changed since that snapshot.
type staleListStore struct {
	cache.Store
	snapshot []interface{}
	keys     []string
}

func newStaleListStore(live cache.Store, snapshot ...*modelv1alpha1.ModelAdapter) *staleListStore {
	s := &staleListStore{Store: live}
	for _, obj := range snapshot {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			panic(err)
		}
		s.snapshot = append(s.snapshot, obj)
		s.keys = append(s.keys, key)
	}
	return s
}

func (s *staleListStore) List() []interface{} {
	return s.snapshot
}

func (s *staleListStore) ListKeys() []string {
	return s.keys
}

func newReemitAdapter(name, resourceVersion string) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			ResourceVersion: resourceVersion,
		},
	}
}

func collectReemitEvents(store cache.Store) ([]WatchEvent, int) {
	var events []WatchEvent
	n := reemitModelAdapters(store, func(ev WatchEvent) {
		events = append(events, ev)
	})
	return events, n
}

func TestReemitModelAdaptersSkipsDeletedAfterSnapshot(t *testing.T) {
	adapterBOld := newReemitAdapter("adapter-b", "10")
	adapterANew := newReemitAdapter("adapter-a", "10")
	adapterBNew := newReemitAdapter("adapter-b", "11")

	// The snapshot was listed at rv10 for both adapters. Afterwards A was
	// deleted, so the live store no longer holds it, and B was updated to rv11.
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(adapterBNew))
	store := newStaleListStore(live, adapterANew, adapterBOld)

	events, n := collectReemitEvents(store)

	require.Len(t, events, 1, "the deleted adapter must not be replayed")
	assert.Equal(t, EventAdd, events[0].Type)
	assert.Same(t, adapterBNew, events[0].Object, "the replay must carry the current object, not the snapshot")
	assert.Equal(t, 1, n)
}

func TestReemitModelAdaptersReplaysUnchangedStore(t *testing.T) {
	adapterA := newReemitAdapter("adapter-a", "1")
	adapterB := newReemitAdapter("adapter-b", "2")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, store.Add(adapterA))
	require.NoError(t, store.Add(adapterB))

	events, n := collectReemitEvents(store)

	require.Len(t, events, 2)
	assert.Equal(t, 2, n)
	emitted := map[string]bool{}
	for _, ev := range events {
		assert.Equal(t, EventAdd, ev.Type)
		obj, ok := ev.Object.(*modelv1alpha1.ModelAdapter)
		require.True(t, ok)
		emitted[obj.Name] = true
	}
	assert.Equal(t, map[string]bool{"adapter-a": true, "adapter-b": true}, emitted)
}

func TestReemitModelAdaptersEmptyStore(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)

	events, n := collectReemitEvents(store)

	assert.Empty(t, events)
	assert.Equal(t, 0, n)
}
