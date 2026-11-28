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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// newReemitAdapter returns a ModelAdapter whose Status.Instances are instances.
func newReemitAdapter(name, resourceVersion string, instances ...string) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			ResourceVersion: resourceVersion,
		},
		Status: modelv1alpha1.ModelAdapterStatus{Instances: instances},
	}
}

func collectReemitEvents(store cache.Store) ([]WatchEvent, int) {
	var events []WatchEvent
	n := reemitModelAdapters(store, func(ev WatchEvent) {
		events = append(events, ev)
	}, nil)
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

// replayReemitEvents runs reemitModelAdapters on store, calling onEmit after
// each event is recorded, so a test can change the live store between an
// emit and the next re-read, as a handler-applied change would.
func replayReemitEvents(store cache.Store, onEmit func(WatchEvent)) ([]WatchEvent, int) {
	var events []WatchEvent
	n := reemitModelAdapters(store, func(ev WatchEvent) {
		events = append(events, ev)
		if onEmit != nil {
			onEmit(ev)
		}
	}, nil)
	return events, n
}

// podLinks is the edge-level model of the cache: each adapter key
// (namespace/name) maps to the set of pods linked to it. An adapter with no
// links has no entry.
type podLinks map[string]map[string]struct{}

// applyReemitEvents applies events to a fresh podLinks model.
func applyReemitEvents(t *testing.T, events []WatchEvent) podLinks {
	t.Helper()
	links := podLinks{}
	for _, ev := range events {
		applyReemitEvent(t, links, ev)
	}
	return links
}

// applyReemitEvent applies one event to links with the cache handler's
// semantics. Add links the object's instances, additively. Update unlinks the
// old object's instances and then links the new object's. Delete unlinks the
// object's instances. Unlinking a pair that is not linked is a no-op.
func applyReemitEvent(t *testing.T, links podLinks, ev WatchEvent) {
	t.Helper()
	obj, ok := ev.Object.(*modelv1alpha1.ModelAdapter)
	require.True(t, ok)
	switch ev.Type {
	case EventAdd:
		linkInstances(links, obj)
	case EventUpdate:
		old, ok := ev.OldObject.(*modelv1alpha1.ModelAdapter)
		require.True(t, ok)
		unlinkInstances(links, old)
		linkInstances(links, obj)
	case EventDelete:
		unlinkInstances(links, obj)
	default:
		t.Fatalf("unexpected event type %v", ev.Type)
	}
}

// linkInstances links each pod in a.Status.Instances to a.
func linkInstances(links podLinks, a *modelv1alpha1.ModelAdapter) {
	key := reemitKey(a)
	for _, pod := range a.Status.Instances {
		if links[key] == nil {
			links[key] = map[string]struct{}{}
		}
		links[key][pod] = struct{}{}
	}
}

// unlinkInstances removes the link from each pod in a.Status.Instances to a
// and drops the adapter's entry once it has no links.
func unlinkInstances(links podLinks, a *modelv1alpha1.ModelAdapter) {
	key := reemitKey(a)
	for _, pod := range a.Status.Instances {
		delete(links[key], pod)
	}
	if len(links[key]) == 0 {
		delete(links, key)
	}
}

// reemitKey returns the namespace/name key the cache uses for a.
func reemitKey(a *modelv1alpha1.ModelAdapter) string {
	return a.Namespace + "/" + a.Name
}

// liveReemitPods returns the links the live store implies: each adapter's
// Status.Instances, keyed by namespace/name.
func liveReemitPods(t *testing.T, store cache.Store) podLinks {
	t.Helper()
	links := podLinks{}
	for _, key := range store.ListKeys() {
		obj, exists, err := store.GetByKey(key)
		require.NoError(t, err)
		require.True(t, exists)
		adapter, ok := obj.(*modelv1alpha1.ModelAdapter)
		require.True(t, ok)
		linkInstances(links, adapter)
	}
	return links
}

// assertLinksMatchStore checks that links has, for every adapter key, the pod
// set of the live store's Status.Instances, and no links for an adapter that
// is absent from the store.
func assertLinksMatchStore(t *testing.T, store cache.Store, links podLinks) {
	t.Helper()
	assert.Equal(t, liveReemitPods(t, store), links)
}

func TestReemitModelAdaptersCompensatesDeleteDuringEmit(t *testing.T) {
	adapterA := newReemitAdapter("adapter-a", "10", "pod-a")
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(adapterA))

	// A is deleted after GetByKey returns it and before its Add is applied.
	events, n := replayReemitEvents(live, func(ev WatchEvent) {
		if ev.Type == EventAdd {
			require.NoError(t, live.Delete(adapterA))
		}
	})

	// The links are checked first, so a missing compensation fails here.
	assertLinksMatchStore(t, live, applyReemitEvents(t, events))
	assert.Empty(t, applyReemitEvents(t, events))
	require.Len(t, events, 2)
	assert.Equal(t, EventAdd, events[0].Type)
	assert.Same(t, adapterA, events[0].Object)
	assert.Equal(t, EventDelete, events[1].Type)
	assert.Same(t, adapterA, events[1].Object)
	assert.Equal(t, 1, n)
}

func TestReemitModelAdaptersCatchesUpdateDuringEmit(t *testing.T) {
	adapterAOld := newReemitAdapter("adapter-a", "10", "pod-a")
	adapterANew := newReemitAdapter("adapter-a", "11", "pod-b")
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(adapterAOld))

	// A is updated from rv10 to rv11 after GetByKey returns rv10.
	events, n := replayReemitEvents(live, func(ev WatchEvent) {
		if ev.Type == EventAdd {
			require.NoError(t, live.Update(adapterANew))
		}
	})

	// The links are checked first, so a missing compensation fails here.
	assertLinksMatchStore(t, live, applyReemitEvents(t, events))
	require.Len(t, events, 2)
	assert.Equal(t, EventAdd, events[0].Type)
	assert.Same(t, adapterAOld, events[0].Object)
	assert.Equal(t, EventUpdate, events[1].Type)
	assert.Same(t, adapterANew, events[1].Object)
	assert.Same(t, adapterAOld, events[1].OldObject)
	assert.Equal(t, 1, n)
}

func TestReemitModelAdaptersHandlesRecreateDuringEmit(t *testing.T) {
	// The instances overlap, so a link left over from the old object (pod-a)
	// and the links the new object keeps (pod-b, pod-c) can be told apart.
	adapterOld := newReemitAdapter("adapter-a", "10", "pod-a", "pod-b")
	adapterOld.UID = "uid-1"
	adapterRecreated := newReemitAdapter("adapter-a", "10", "pod-b", "pod-c")
	adapterRecreated.UID = "uid-2"
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(adapterOld))

	// A is deleted and re-created with a new UID after GetByKey returns uid-1.
	events, n := replayReemitEvents(live, func(ev WatchEvent) {
		if ev.Type == EventAdd {
			require.NoError(t, live.Delete(adapterOld))
			require.NoError(t, live.Add(adapterRecreated))
		}
	})

	// The links are checked first, so a missing compensation fails here.
	assertLinksMatchStore(t, live, applyReemitEvents(t, events))
	require.Len(t, events, 2)
	assert.Equal(t, EventAdd, events[0].Type)
	assert.Same(t, adapterOld, events[0].Object)
	assert.Equal(t, EventUpdate, events[1].Type)
	assert.Same(t, adapterRecreated, events[1].Object)
	assert.Same(t, adapterOld, events[1].OldObject)
	assert.Equal(t, 1, n)
	assert.Equal(t, map[string]struct{}{"pod-b": {}, "pod-c": {}}, applyReemitEvents(t, events)["default/adapter-a"])
}

func TestReemitModelAdaptersStableStoreEmitsOnce(t *testing.T) {
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, name := range []string{"adapter-a", "adapter-b", "adapter-c"} {
		require.NoError(t, live.Add(newReemitAdapter(name, "1", "pod-"+name)))
	}

	events, n := replayReemitEvents(live, nil)

	// The links are checked first, so a missing emit fails here.
	assertLinksMatchStore(t, live, applyReemitEvents(t, events))
	require.Len(t, events, 3)
	for _, ev := range events {
		assert.Equal(t, EventAdd, ev.Type)
	}
	assert.Equal(t, 3, n)
}

// simAdapter returns the test ModelAdapter with the given UID, resource version
// and instances.
func simAdapter(uid, resourceVersion string, instances ...string) *modelv1alpha1.ModelAdapter {
	adapter := newReemitAdapter("adapter-a", resourceVersion, instances...)
	adapter.UID = types.UID(uid)
	return adapter
}

func TestReemitModelAdaptersLoopsUntilStoreMatches(t *testing.T) {
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(simAdapter("uid-1", "10", "pod-10")))
	rv := 10
	emits := 0

	// The store changes on each of the first six emits, so the loop must keep
	// re-reading until a re-read matches the object it last applied. Each
	// version has its own pod, so a link left by an older version stays visible.
	events, n := replayReemitEvents(live, func(WatchEvent) {
		emits++
		if emits <= 6 {
			rv++
			require.NoError(t, live.Update(simAdapter("uid-1", strconv.Itoa(rv), "pod-"+strconv.Itoa(rv))))
		}
	})

	// The links are checked first, so a loop that stops early fails here.
	assertLinksMatchStore(t, live, applyReemitEvents(t, events))
	assert.Equal(t, 1, n)
	require.Len(t, events, 7)
	last, ok := events[6].Object.(*modelv1alpha1.ModelAdapter)
	require.True(t, ok)
	assert.Equal(t, []string{"pod-" + strconv.Itoa(rv)}, last.Status.Instances)
}

func TestReemitModelAdaptersReturnsOnStop(t *testing.T) {
	live := cache.NewStore(cache.MetaNamespaceKeyFunc)
	require.NoError(t, live.Add(simAdapter("uid-1", "10")))
	stop := make(chan struct{})
	rv := 10
	var events []WatchEvent

	// The store changes on every emit, so the loop never settles on its own.
	// Closing stop must end it.
	n := reemitModelAdapters(live, func(ev WatchEvent) {
		events = append(events, ev)
		rv++
		require.NoError(t, live.Update(simAdapter("uid-1", strconv.Itoa(rv))))
		if len(events) == 5 {
			close(stop)
		}
	}, stop)

	assert.Equal(t, 1, n)
	assert.Len(t, events, 5)
}

// notifySim models one ModelAdapter during a replay. Store mutations queue the
// notifications client-go's listener would deliver, and replay emits are applied
// to model at once, as the handler does. A queued notification reaches model
// only when the test delivers it, so the test chooses where it lands.
type notifySim struct {
	t       *testing.T
	store   cache.Store
	model   podLinks
	pending []WatchEvent
	events  []WatchEvent
	emits   int
	// before maps a 1-based emit number to a step that runs before that emit
	// is applied, which is where a store change can land in the window.
	before map[int]func()
}

func newNotifySim(t *testing.T, store cache.Store) *notifySim {
	return &notifySim{t: t, store: store, model: podLinks{}}
}

func (s *notifySim) emit(ev WatchEvent) {
	s.emits++
	s.events = append(s.events, ev)
	if step := s.before[s.emits]; step != nil {
		step()
	}
	applyReemitEvent(s.t, s.model, ev)
}

func (s *notifySim) replay() int {
	return reemitModelAdapters(s.store, s.emit, nil)
}

// set stores obj and queues the Add or Update that client-go would send.
func (s *notifySim) set(obj *modelv1alpha1.ModelAdapter) {
	s.t.Helper()
	old, exists, err := s.store.GetByKey(reemitKey(obj))
	require.NoError(s.t, err)
	require.NoError(s.t, s.store.Update(obj))
	if exists {
		s.pending = append(s.pending, WatchEvent{Type: EventUpdate, Object: obj, OldObject: old})
	} else {
		s.pending = append(s.pending, WatchEvent{Type: EventAdd, Object: obj})
	}
}

// remove deletes the live adapter and queues the Delete that client-go would send.
func (s *notifySim) remove() {
	s.t.Helper()
	old, exists, err := s.store.GetByKey("default/adapter-a")
	require.NoError(s.t, err)
	require.True(s.t, exists)
	require.NoError(s.t, s.store.Delete(old))
	s.pending = append(s.pending, WatchEvent{Type: EventDelete, Object: old})
}

// deliver applies up to n queued notifications to model in order. A negative n delivers all.
func (s *notifySim) deliver(n int) {
	for ; n != 0 && len(s.pending) > 0; n-- {
		applyReemitEvent(s.t, s.model, s.pending[0])
		s.pending = s.pending[1:]
	}
}

// requireConverged delivers every queued notification and then checks that
// model's links match the live store.
func (s *notifySim) requireConverged() {
	s.t.Helper()
	s.deliver(-1)
	assert.Empty(s.t, s.pending)
	assertLinksMatchStore(s.t, s.store, s.model)
}

func TestReemitModelAdaptersConvergesWithBufferedNotifications(t *testing.T) {
	t.Run("delete after GetByKey, delivered before the Add", func(t *testing.T) {
		live := cache.NewStore(cache.MetaNamespaceKeyFunc)
		require.NoError(t, live.Add(simAdapter("uid-1", "10", "pod-a")))
		sim := newNotifySim(t, live)
		sim.before = map[int]func(){1: func() {
			sim.remove()
			sim.deliver(-1) // the Delete reaches model before the replayed Add
		}}

		assert.Equal(t, 1, sim.replay())
		// The links are checked first, so a stale Add fails here.
		sim.requireConverged()
		require.Len(t, sim.events, 2)
		assert.Equal(t, EventAdd, sim.events[0].Type)
		assert.Equal(t, EventDelete, sim.events[1].Type)
	})

	t.Run("two updates, notifications delivered late", func(t *testing.T) {
		live := cache.NewStore(cache.MetaNamespaceKeyFunc)
		require.NoError(t, live.Add(simAdapter("uid-1", "10", "pod-a")))
		sim := newNotifySim(t, live)
		sim.before = map[int]func(){1: func() {
			sim.set(simAdapter("uid-1", "11", "pod-b"))
			sim.set(simAdapter("uid-1", "12", "pod-c"))
		}}

		assert.Equal(t, 1, sim.replay())
		assert.Len(t, sim.pending, 2)
		sim.requireConverged()
		require.Len(t, sim.events, 2)
		assert.Equal(t, EventUpdate, sim.events[1].Type)
	})

	t.Run("delete then recreate with a new UID, notifications interleaved", func(t *testing.T) {
		// The old and new instances overlap on pod-b. Only a stale link to
		// pod-a or a missing link to pod-c can leave the links unequal to the store.
		live := cache.NewStore(cache.MetaNamespaceKeyFunc)
		require.NoError(t, live.Add(simAdapter("uid-1", "10", "pod-a", "pod-b")))
		sim := newNotifySim(t, live)
		sim.before = map[int]func(){
			1: func() {
				sim.remove()
				sim.set(simAdapter("uid-2", "10", "pod-b", "pod-c"))
				sim.deliver(1) // the Delete of uid-1 reaches model before our Add
			},
			2: func() {
				sim.set(simAdapter("uid-2", "11", "pod-b", "pod-c"))
			},
		}

		assert.Equal(t, 1, sim.replay())
		sim.requireConverged()
		require.Len(t, sim.events, 3)
		assert.Equal(t, EventUpdate, sim.events[1].Type)
		assert.Equal(t, EventUpdate, sim.events[2].Type)
	})

	t.Run("four changes in a row, the fourth a delete", func(t *testing.T) {
		live := cache.NewStore(cache.MetaNamespaceKeyFunc)
		require.NoError(t, live.Add(simAdapter("uid-1", "10", "pod-a")))
		sim := newNotifySim(t, live)
		sim.before = map[int]func(){
			1: func() { sim.set(simAdapter("uid-1", "11", "pod-b")) },
			2: func() { sim.set(simAdapter("uid-1", "12", "pod-c")) },
			3: func() { sim.set(simAdapter("uid-1", "13", "pod-d")) },
			// The Delete is delivered before the replayed Update(rv13) is
			// applied, which leaves pod-d linked in model. Only a further
			// re-check emits the Delete again.
			4: func() {
				sim.remove()
				sim.deliver(-1)
			},
		}

		assert.Equal(t, 1, sim.replay())
		sim.requireConverged()
		require.Len(t, sim.events, 5)
		assert.Equal(t, EventDelete, sim.events[4].Type)
	})
}
