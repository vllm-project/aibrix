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

package cache

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

const (
	adapterLinkBase    = "base-model"
	adapterLinkAdapter = "lora-a"
)

// newAdapterLinkTestAdapter returns a ModelAdapter in namespace default with a UID
// derived from its name, so an Add with the same UID is a re-emit, not a recreate.
// An empty baseModel leaves spec.baseModel unset. getNewModelAdapterWithPods is not
// used because cache_test.go is excluded from -race builds.
func newAdapterLinkTestAdapter(name, baseModel string, instances ...string) *modelv1alpha1.ModelAdapter {
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-default-" + name)},
		Status:     modelv1alpha1.ModelAdapterStatus{Instances: instances},
	}
	if baseModel != "" {
		adapter.Spec.BaseModel = ptr.To(baseModel)
	}
	return adapter
}

// touchedAdapterLinkTestPod returns a copy of pod that an update event can carry.
func touchedAdapterLinkTestPod(pod *v1.Pod) *v1.Pod {
	touched := pod.DeepCopy()
	if touched.Annotations == nil {
		touched.Annotations = map[string]string{}
	}
	touched.Annotations["example.com/touched"] = "1"
	return touched
}

// podNamesOfModel returns the sorted names of the pods serving model, or nil when
// the model is not cached.
func podNamesOfModel(t *testing.T, c *Store, model string) []string {
	t.Helper()
	meta, ok := c.metaModels.Load(model)
	if !ok {
		return nil
	}
	var names []string
	for _, pod := range meta.Pods.Array().Pods {
		names = append(names, pod.Name)
	}
	sort.Strings(names)
	return names
}

// An update of a pod that a ModelAdapter lists must keep the adapter edge,
// including for an adapter that only this pod serves.
func TestUpdatePodKeepsAdapterEdge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []string
	}{
		{name: "single host", instances: []string{"p0"}},
		{name: "two hosts", instances: []string{"p0", "p1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewForTest()
			p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
			c.addPod(p0)
			c.addPod(requestTrackerTestPod("p1", "default", adapterLinkBase, "10.0.0.2"))
			adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, tc.instances...)
			c.addModelAdapter(adapter)
			before, ok := c.metaModels.Load(adapterLinkAdapter)
			require.True(t, ok)
			atomic.StoreInt32(&before.pendingRequests, 3)

			c.updatePod(p0, touchedAdapterLinkTestPod(p0))

			assert.Equal(t, tc.instances, podNamesOfModel(t, c, adapterLinkAdapter))
			after, ok := c.metaModels.Load(adapterLinkAdapter)
			require.True(t, ok)
			assert.Same(t, before, after, "the adapter model must not be rebuilt")
			assert.Equal(t, int32(3), atomic.LoadInt32(&after.pendingRequests))
			models, err := c.ListModelsByPod("p0", "default")
			require.NoError(t, err)
			assert.Contains(t, models, adapterLinkAdapter)
		})
	}
}

// Two adapters that list the same pod both survive an update of that pod.
func TestUpdatePodKeepsEveryAdapterOfThePod(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	c.addPod(p0)
	for _, name := range []string{"lora-a", "lora-b"} {
		c.addModelAdapter(newAdapterLinkTestAdapter(name, adapterLinkBase, "p0"))
	}

	c.updatePod(p0, touchedAdapterLinkTestPod(p0))

	for _, name := range []string{"lora-a", "lora-b"} {
		assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, name), name)
	}
}

// A record whose pod is not cached must not create a model entry.
func TestAddModelAdapterBeforePodCreatesNoModel(t *testing.T) {
	c := NewForTest()
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)
	assert.False(t, c.HasModel(adapterLinkAdapter))

	c.addPod(requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1"))
	assert.True(t, c.HasModel(adapterLinkAdapter))
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))
}

// The base model of an adapter must not depend on whether its pod or the adapter
// event arrives first.
func TestAdapterBaseModelIsIndependentOfEventOrder(t *testing.T) {
	for _, specBase := range []string{adapterLinkBase, ""} {
		for _, podFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("spec=%q/podFirst=%v", specBase, podFirst), func(t *testing.T) {
				c := NewForTest()
				pod := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
				adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, specBase, "p0")
				if podFirst {
					c.addPod(pod)
					c.addModelAdapter(adapter)
				} else {
					c.addModelAdapter(adapter)
					c.addPod(pod)
				}
				base, ok := c.ModelBaseModel(adapterLinkAdapter)
				assert.True(t, ok)
				assert.Equal(t, adapterLinkBase, base)
			})
		}
	}
}

// A pod relabelled to another base model keeps the adapter edge while the
// ModelAdapter's status.instances lists it, and loses it once the ModelAdapter
// stops listing it. The final state must not depend on which event comes first.
func TestRelabelledPodAdapterEdgeFollowsStatusInstances(t *testing.T) {
	relabelled := func(c *Store, p0 *v1.Pod) {
		c.updatePod(p0, requestTrackerTestPod("p0", "default", "other-model", "10.0.0.1"))
	}
	removed := func(c *Store, adapter *modelv1alpha1.ModelAdapter) {
		updated := adapter.DeepCopy()
		updated.Status.Instances = nil
		c.updateModelAdapter(adapter, updated)
	}
	run := func(t *testing.T, podFirst bool) (bool, []string) {
		c := NewForTest()
		p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
		c.addPod(p0)
		adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
		c.addModelAdapter(adapter)
		if podFirst {
			relabelled(c, p0)
			assert.True(t, c.HasModel(adapterLinkAdapter), "status.instances still lists the pod")
			removed(c, adapter)
		} else {
			removed(c, adapter)
			relabelled(c, p0)
		}
		return c.HasModel(adapterLinkAdapter), podNamesOfModel(t, c, adapterLinkAdapter)
	}

	podFirstHas, podFirstPods := run(t, true)
	adapterFirstHas, adapterFirstPods := run(t, false)
	assert.False(t, podFirstHas)
	assert.Nil(t, podFirstPods)
	assert.Equal(t, podFirstHas, adapterFirstHas)
	assert.Equal(t, podFirstPods, adapterFirstPods)
}

// A pod whose model label differs from spec.baseModel is linked when a ModelAdapter
// lists it in status.instances, both by the adapter event and by a later pod update.
func TestListedPodWithOtherModelLabelIsLinked(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", "other-model", "10.0.0.1")
	c.addPod(p0)
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))

	c.updatePod(p0, touchedAdapterLinkTestPod(p0))
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))
}

// A pod that becomes a Ray worker loses the adapter edge and is not linked.
func TestUpdatePodDropsAdapterEdgeWhenPodBecomesWorker(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	c.addPod(p0)
	c.addModelAdapter(newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0"))
	require.True(t, c.HasModel(adapterLinkAdapter))

	worker := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	worker.Labels[nodeType] = nodeWorker
	c.updatePod(p0, worker)
	assert.False(t, c.HasModel(adapterLinkAdapter))
}

// A pod that an adapter stops listing, or whose adapter is deleted, must not get
// the edge back from a later pod event.
func TestAdapterEdgeIsNotResurrected(t *testing.T) {
	remove := func(c *Store, adapter *modelv1alpha1.ModelAdapter) {
		updated := adapter.DeepCopy()
		updated.Status.Instances = []string{"p1"}
		c.updateModelAdapter(adapter, updated)
	}
	for _, tc := range []struct {
		name   string
		remove func(c *Store, adapter *modelv1alpha1.ModelAdapter)
		want   []string
	}{
		{name: "instance removed", remove: remove, want: []string{"p1"}},
		{name: "adapter deleted", remove: func(c *Store, a *modelv1alpha1.ModelAdapter) { c.deleteModelAdapter(a) }},
		{name: "adapter deleted by tombstone", remove: func(c *Store, a *modelv1alpha1.ModelAdapter) {
			c.deleteModelAdapter(cache.DeletedFinalStateUnknown{Key: "default/" + a.Name, Obj: a})
		}},
		{name: "adapter deleted by key-only tombstone", remove: func(c *Store, a *modelv1alpha1.ModelAdapter) {
			c.deleteModelAdapter(cache.DeletedFinalStateUnknown{Key: "default/" + a.Name})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewForTest()
			p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
			c.addPod(p0)
			c.addPod(requestTrackerTestPod("p1", "default", adapterLinkBase, "10.0.0.2"))
			adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0", "p1")
			c.addModelAdapter(adapter)

			tc.remove(c, adapter)
			updated := touchedAdapterLinkTestPod(p0)
			c.updatePod(p0, updated)
			c.deletePod(updated)
			c.addPod(p0)

			assert.Equal(t, tc.want, podNamesOfModel(t, c, adapterLinkAdapter))
		})
	}
}

// The post-sync re-emit replays an Add that may be older than a recorded Update.
// The stale Add must not restore the instance that the Update removed.
func TestStaleAdapterAddKeepsNewerRecord(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	c.addPod(p0)
	c.addPod(requestTrackerTestPod("p1", "default", adapterLinkBase, "10.0.0.2"))
	first := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0", "p1")
	c.addModelAdapter(first)
	latest := first.DeepCopy()
	latest.Status.Instances = []string{"p1"}
	c.updateModelAdapter(first, latest)

	c.addModelAdapter(first) // stale post-sync re-emit
	assert.Equal(t, []string{"p1"}, podNamesOfModel(t, c, adapterLinkAdapter))
	c.updatePod(p0, touchedAdapterLinkTestPod(p0))

	assert.Equal(t, []string{"p1"}, podNamesOfModel(t, c, adapterLinkAdapter))
}

// A pod that is deleted and re-added under the same name, with a new UID, while an
// adapter still lists it is linked again.
func TestAddPodRelinksAdapterAfterDelete(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	p0.UID = types.UID("uid-p0-1")
	c.addPod(p0)
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)

	c.deletePod(p0)
	require.False(t, c.HasModel(adapterLinkAdapter))

	recreated := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	recreated.UID = types.UID("uid-p0-2")
	c.addPod(recreated)
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))
}

// A recreated ModelAdapter (same name, new UID) replaces its record: the old
// instances lose the edge and the new ones gain it.
func TestRecreatedAdapterReplacesEdges(t *testing.T) {
	c := NewForTest()
	for _, name := range []string{"p0", "p1"} {
		c.addPod(requestTrackerTestPod(name, "default", adapterLinkBase, "10.0.0.1"))
	}
	old := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	old.UID = types.UID("uid-old")
	c.addModelAdapter(old)
	require.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))

	recreated := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p1")
	recreated.UID = types.UID("uid-new")
	c.addModelAdapter(recreated)

	assert.Equal(t, []string{"p1"}, podNamesOfModel(t, c, adapterLinkAdapter))
	models, err := c.ListModelsByPod("p0", "default")
	require.NoError(t, err)
	assert.NotContains(t, models, adapterLinkAdapter)
}

// An update unlinks the recorded instances, so an old object that disagrees with
// the record cannot leave a stale edge behind.
func TestUpdateUnlinksRecordedInstances(t *testing.T) {
	c := NewForTest()
	for _, name := range []string{"p0", "p1", "p2"} {
		c.addPod(requestTrackerTestPod(name, "default", adapterLinkBase, "10.0.0.1"))
	}
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0", "p1")
	c.addModelAdapter(adapter)

	outdated := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p1")
	updated := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p1", "p2")
	c.updateModelAdapter(outdated, updated)

	assert.Equal(t, []string{"p1", "p2"}, podNamesOfModel(t, c, adapterLinkAdapter))
	models, err := c.ListModelsByPod("p0", "default")
	require.NoError(t, err)
	assert.NotContains(t, models, adapterLinkAdapter)
}

// A delete unlinks the recorded instances even when the delete object is outdated.
func TestDeleteUnlinksRecordedInstances(t *testing.T) {
	c := NewForTest()
	for _, name := range []string{"p0", "p1"} {
		c.addPod(requestTrackerTestPod(name, "default", adapterLinkBase, "10.0.0.1"))
	}
	c.addModelAdapter(newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0", "p1"))

	c.deleteModelAdapter(newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p1"))

	assert.False(t, c.HasModel(adapterLinkAdapter))
	for _, name := range []string{"p0", "p1"} {
		models, err := c.ListModelsByPod(name, "default")
		require.NoError(t, err)
		assert.NotContains(t, models, adapterLinkAdapter, name)
	}
}

// Pod updates and ModelAdapter updates interleave under -race. Only one goroutine
// writes the adapter, so its last applied state is known, and the final edges must
// equal it whatever order the pod updates ran in.
func TestConcurrentPodAndAdapterUpdatesEndInLastAdapterState(t *testing.T) {
	const iterations = 200
	states := [][]string{{"p0", "p1"}, {"p1", "p2"}, {"p2"}, {"p0"}}
	pods := []string{"p0", "p1", "p2"}

	c := NewForTest()
	for i, name := range pods {
		c.addPod(requestTrackerTestPod(name, "default", adapterLinkBase, fmt.Sprintf("10.0.0.%d", i+1)))
	}
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0", "p1")
	c.addModelAdapter(adapter)

	var wg sync.WaitGroup
	wg.Add(1 + len(pods))
	go func() {
		defer wg.Done()
		current := adapter
		for i := 0; i < iterations; i++ {
			next := current.DeepCopy()
			next.Status.Instances = states[i%len(states)]
			c.updateModelAdapter(current, next)
			current = next
		}
	}()
	for i, name := range pods {
		go func() {
			defer wg.Done()
			prev := requestTrackerTestPod(name, "default", adapterLinkBase, fmt.Sprintf("10.0.0.%d", i+1))
			for j := 0; j < iterations; j++ {
				next := touchedAdapterLinkTestPod(prev)
				c.updatePod(prev, next)
				prev = next
			}
		}()
	}
	wg.Wait()

	want := states[(iterations-1)%len(states)]
	assert.Equal(t, want, podNamesOfModel(t, c, adapterLinkAdapter))
	for _, name := range pods {
		models, err := c.ListModelsByPod(name, "default")
		require.NoError(t, err)
		linked := slices.Contains(want, name)
		assert.Equal(t, linked, slices.Contains(models, adapterLinkAdapter), name)
	}
	assertAdapterIndexConsistent(t, c)
}

// Adapters of the same name in two namespaces link only the pods of their own
// namespace.
func TestLinkModelAdaptersIsNamespaced(t *testing.T) {
	c := NewForTest()
	for namespace, base := range map[string]string{"ns-a": "base-a", "ns-b": "base-b"} {
		c.addPod(requestTrackerTestPod("p0", namespace, base, "10.0.0.1"))
		adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, base, "p0")
		adapter.Namespace = namespace
		c.addModelAdapter(adapter)
	}
	c.addPod(requestTrackerTestPod("p0", "ns-c", "base-a", "10.0.0.3"))

	meta, ok := c.metaModels.Load(adapterLinkAdapter)
	require.True(t, ok)
	var keys []string
	for _, pod := range meta.Pods.Array().Pods {
		keys = append(keys, pod.Namespace+"/"+pod.Name)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"ns-a/p0", "ns-b/p0"}, keys)
}
