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
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	adapterLinkBase    = "base-model"
	adapterLinkAdapter = "lora-a"
)

// newAdapterLinkTestAdapter returns a ModelAdapter in namespace default. An empty
// baseModel leaves spec.baseModel unset. getNewModelAdapterWithPods is not used
// because cache_test.go is excluded from -race builds.
func newAdapterLinkTestAdapter(name, baseModel string, instances ...string) *modelv1alpha1.ModelAdapter {
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
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

// A pod is linked to an adapter that lists it only when the pod serves the
// adapter's base model, either by its model label or by a routable ModelClaim.
func TestLinkModelAdaptersMatchesBaseModel(t *testing.T) {
	claimed := func(port string) func(*v1.Pod) {
		return func(p *v1.Pod) {
			delete(p.Labels, constants.ModelLabelName)
			p.Annotations = map[string]string{
				constants.ModelClaimPodAnnotationPrefix + "claim": `{"model":"` + adapterLinkBase + `","port":` + port + `}`,
			}
		}
	}
	for _, tc := range []struct {
		name     string
		specBase string
		mutate   func(*v1.Pod)
		want     bool
	}{
		{name: "same label", specBase: adapterLinkBase, want: true},
		{name: "other label", specBase: "other-model", want: false},
		{name: "padded spec", specBase: " " + adapterLinkBase + " ", want: true},
		{name: "unset spec matches any listed pod", specBase: "", want: true},
		{name: "routable claim", specBase: adapterLinkBase, mutate: claimed("8000"), want: true},
		{name: "claim without a port", specBase: adapterLinkBase, mutate: claimed("0"), want: false},
		{name: "unset spec on a claim pod", specBase: "", mutate: claimed("8000"), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewForTest()
			pod := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, tc.specBase, "p0")
			c.addModelAdapter(adapter)
			c.addPod(pod)
			assert.Equal(t, tc.want, c.HasModel(adapterLinkAdapter))
		})
	}
}

// A ModelClaim binding for the base model that is not routable (port 0) does not
// link the pod; the wake patch that gives it a port does.
func TestUpdatePodLinksAdapterWhenClaimWakes(t *testing.T) {
	c := NewForTest()
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)
	asleep := warmModelClaimPod("p0", "default", map[string]int{adapterLinkBase: 0})
	c.addPod(asleep)
	assert.False(t, c.HasModel(adapterLinkAdapter))

	awake := warmModelClaimPod("p0", "default", map[string]int{adapterLinkBase: 9001})
	c.updatePod(asleep, awake)
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))
}

// A pod that is relabelled to another base model loses the adapter edge it no
// longer serves.
func TestUpdatePodDropsAdapterEdgeOfRelabelledPod(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	c.addPod(p0)
	c.addModelAdapter(newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0"))
	require.True(t, c.HasModel(adapterLinkAdapter))

	c.updatePod(p0, requestTrackerTestPod("p0", "default", "other-model", "10.0.0.1"))
	assert.False(t, c.HasModel(adapterLinkAdapter))
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

// A pod that is deleted and re-added while an adapter still lists it is linked
// again.
func TestAddPodRelinksAdapterAfterDelete(t *testing.T) {
	c := NewForTest()
	p0 := requestTrackerTestPod("p0", "default", adapterLinkBase, "10.0.0.1")
	c.addPod(p0)
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)

	c.deletePod(p0)
	require.False(t, c.HasModel(adapterLinkAdapter))

	c.addPod(p0)
	assert.Equal(t, []string{"p0"}, podNamesOfModel(t, c, adapterLinkAdapter))
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
