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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// assertAdapterIndexConsistent recomputes the inverse of the recorded
// status.instances and checks that podAdapters equals it, with no empty sets.
// It reads internal fields, so it lives apart from the behavioural tests.
func assertAdapterIndexConsistent(t *testing.T, c *Store) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	want := make(map[string]map[string]struct{})
	for adapterKey, model := range c.modelAdapters {
		for _, instance := range model.Status.Instances {
			podKey := utils.GeneratePodKey(model.Namespace, instance)
			if want[podKey] == nil {
				want[podKey] = make(map[string]struct{})
			}
			want[podKey][adapterKey] = struct{}{}
		}
	}
	got := make(map[string]map[string]struct{}, len(c.podAdapters))
	for podKey, adapters := range c.podAdapters {
		assert.NotEmpty(t, adapters, "podAdapters must not hold an empty set for %s", podKey)
		copied := make(map[string]struct{}, len(adapters))
		for adapterKey := range adapters {
			copied[adapterKey] = struct{}{}
		}
		got[podKey] = copied
	}
	assert.Equal(t, want, got, "podAdapters must be the inverse of the recorded status.instances")
}

// Each step of a ModelAdapter lifecycle, driven through the handlers, keeps
// podAdapters the inverse of the recorded adapters.
func TestAdapterIndexStaysConsistent(t *testing.T) {
	c := NewForTest()
	assertAdapterIndexConsistent(t, c)

	newAdapter := func(name, uid string, instances ...string) *modelv1alpha1.ModelAdapter {
		adapter := newAdapterLinkTestAdapter(name, adapterLinkBase, instances...)
		adapter.UID = types.UID(uid)
		return adapter
	}

	// add with instances [p0, p1]
	lora := newAdapter(adapterLinkAdapter, "uid-1", "p0", "p1")
	c.addModelAdapter(lora)
	assertAdapterIndexConsistent(t, c)

	// update to [p1, p2]
	updated := lora.DeepCopy()
	updated.Status.Instances = []string{"p1", "p2"}
	c.updateModelAdapter(lora, updated)
	assertAdapterIndexConsistent(t, c)
	lora = updated

	// update with a duplicate instance [p2, p2]
	duplicated := lora.DeepCopy()
	duplicated.Status.Instances = []string{"p2", "p2"}
	c.updateModelAdapter(lora, duplicated)
	assertAdapterIndexConsistent(t, c)
	lora = duplicated

	// stale same-UID re-add of an older object that lists [p0, p1]
	stale := newAdapter(adapterLinkAdapter, "uid-1", "p0", "p1")
	c.addModelAdapter(stale)
	assertAdapterIndexConsistent(t, c)

	// recreate under a new UID replaces the record
	recreated := newAdapter(adapterLinkAdapter, "uid-2", "p1")
	c.addModelAdapter(recreated)
	assertAdapterIndexConsistent(t, c)

	// delete with the full object
	c.deleteModelAdapter(recreated)
	assertAdapterIndexConsistent(t, c)

	// add another adapter, then delete it by a key-only tombstone
	other := newAdapter("lora-b", "uid-3", "p0", "p1")
	c.addModelAdapter(other)
	assertAdapterIndexConsistent(t, c)
	c.deleteModelAdapter(cache.DeletedFinalStateUnknown{Key: "default/lora-b"})
	assertAdapterIndexConsistent(t, c)

	assert.Empty(t, c.modelAdapters)
	assert.Empty(t, c.podAdapters)
}

// The record is a deep copy: mutating the object after the add changes neither the
// stored record nor the index.
func TestAdapterRecordIsDeepCopy(t *testing.T) {
	c := NewForTest()
	adapter := newAdapterLinkTestAdapter(adapterLinkAdapter, adapterLinkBase, "p0")
	c.addModelAdapter(adapter)

	adapter.Status.Instances[0] = "p9"
	adapter.Status.Instances = append(adapter.Status.Instances, "p8")
	adapter.Name = "renamed"
	adapter.Labels = map[string]string{"mutated": "true"}

	c.mu.Lock()
	record, ok := c.modelAdapters["default/"+adapterLinkAdapter]
	c.mu.Unlock()
	require.True(t, ok)
	assert.NotSame(t, adapter, record)
	assert.Equal(t, []string{"p0"}, record.Status.Instances)
	assert.Equal(t, adapterLinkAdapter, record.Name)
	assert.Empty(t, record.Labels)
	assertAdapterIndexConsistent(t, c)

	c.mu.Lock()
	_, indexedP0 := c.podAdapters[utils.GeneratePodKey("default", "p0")]
	_, indexedP9 := c.podAdapters[utils.GeneratePodKey("default", "p9")]
	c.mu.Unlock()
	assert.True(t, indexedP0)
	assert.False(t, indexedP9)
}
