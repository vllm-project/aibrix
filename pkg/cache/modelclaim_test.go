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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func warmModelClaimPod(name, namespace string, models map[string]int) *v1.Pod {
	anns := map[string]string{}
	i := 0
	for served, port := range models {
		anns[fmt.Sprintf("%smc%d", constants.ModelClaimPodAnnotationPrefix, i)] =
			fmt.Sprintf(`{"model":%q,"port":%d}`, served, port)
		i++
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{constants.ModelPoolLabelName: "pool-a", constants.ModelPoolLabelEnabled: "true"},
			Annotations: anns,
		},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.9",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestAddPodRegistersModelClaim(t *testing.T) {
	c := NewForTest()
	c.addPod(warmModelClaimPod("p1", "default", map[string]int{"qwen-0.5b": 9001}))

	_, exist := c.metaPods.Load("default/p1")
	assert.True(t, exist, "warm pod should be admitted to the cache")

	pods, err := c.ListPodsByModel("qwen-0.5b")
	require.NoError(t, err)
	assert.Equal(t, 1, pods.Len(), "served model should resolve to the warm pod")
}

func TestAddPodMultipleModelClaimsOnePod(t *testing.T) {
	c := NewForTest()
	c.addPod(warmModelClaimPod("p1", "default", map[string]int{"m-a": 9001, "m-b": 9002}))

	a, err := c.ListPodsByModel("m-a")
	require.NoError(t, err)
	assert.Equal(t, 1, a.Len())

	b, err := c.ListPodsByModel("m-b")
	require.NoError(t, err)
	assert.Equal(t, 1, b.Len())
}

func TestUpdatePodAddsAndRemovesModelClaim(t *testing.T) {
	c := NewForTest()
	old := warmModelClaimPod("p1", "default", map[string]int{"m-a": 9001})
	c.addPod(old)

	both := warmModelClaimPod("p1", "default", map[string]int{"m-a": 9001, "m-b": 9002})
	c.updatePod(old, both)
	a, err := c.ListPodsByModel("m-a")
	require.NoError(t, err)
	assert.Equal(t, 1, a.Len())
	b, err := c.ListPodsByModel("m-b")
	require.NoError(t, err)
	assert.Equal(t, 1, b.Len())

	onlyB := warmModelClaimPod("p1", "default", map[string]int{"m-b": 9002})
	c.updatePod(both, onlyB)
	_, err = c.ListPodsByModel("m-a")
	assert.Error(t, err, "m-a should no longer be registered after the annotation is dropped")
	b, err = c.ListPodsByModel("m-b")
	require.NoError(t, err)
	assert.Equal(t, 1, b.Len())
}

func TestModelClaimPortZeroHasNoRoutableBackend(t *testing.T) {
	c := NewForTest()
	c.addPod(warmModelClaimPod("p1", "default", map[string]int{"m-live": 9001, "m-booting": 0}))

	assert.True(t, c.HasModel("m-live"))
	assert.False(t, c.HasModel("m-booting"), "port 0 ModelClaim must have no routable pods")
	_, err := c.ListPodsByModel("m-booting")
	assert.Error(t, err)
}

func TestModelClaimRoutabilityViaAnnotationUpdate(t *testing.T) {
	c := NewForTest()
	live := warmModelClaimPod("p1", "default", map[string]int{"m": 9001})
	c.addPod(live)
	require.True(t, c.HasModel("m"))

	notRoutable := warmModelClaimPod("p1", "default", map[string]int{"m": 0})
	c.updatePod(live, notRoutable)
	assert.False(t, c.HasModel("m"))

	c.updatePod(notRoutable, warmModelClaimPod("p1", "default", map[string]int{"m": 9002}))
	assert.True(t, c.HasModel("m"))
}

func TestModelClaimStateClearedOnPodDelete(t *testing.T) {
	c := NewForTest()
	pod := warmModelClaimPod("p1", "default", map[string]int{"m": 0})
	c.addPod(pod)
	require.False(t, c.HasModel("m"))

	c.deletePod(pod)
	assert.False(t, c.HasModel("m"))
}
