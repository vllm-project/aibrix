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
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
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
	pod := warmModelClaimPod("p1", "default", map[string]int{"m-live": 9001})
	pod.Annotations[constants.ModelClaimPodAnnotationPrefix+"sleeping"] =
		`{"model":"m-sleeping","port":0,"state":"sleeping"}`
	c.addPod(pod)

	assert.True(t, c.HasModel("m-live"))
	assert.False(t, c.HasModel("m-sleeping"), "port 0 ModelClaim must have no routable pods")
	_, err := c.ListPodsByModel("m-sleeping")
	assert.Error(t, err)

	provider, ok := any(c).(interface {
		ModelClaimBinding(string) (*v1.Pod, int, string, bool)
	})
	require.True(t, ok, "cache must expose non-routable ModelClaim bindings")
	boundPod, port, state, found := provider.ModelClaimBinding("m-sleeping")
	require.True(t, found)
	assert.Equal(t, pod.Name, boundPod.Name)
	assert.Zero(t, port)
	assert.Equal(t, constants.ModelClaimRoutingStateSleeping, state)
}

func TestModelClaimBindingTracksAnnotationUpdatesAndDeletion(t *testing.T) {
	c := NewForTest()
	active := warmModelClaimPod("p1", "default", map[string]int{"m": 9001})
	c.addPod(active)

	provider, ok := any(c).(interface {
		ModelClaimBinding(string) (*v1.Pod, int, string, bool)
	})
	require.True(t, ok, "cache must expose ModelClaim bindings")
	_, port, state, found := provider.ModelClaimBinding("m")
	require.True(t, found)
	assert.Equal(t, 9001, port)
	assert.Equal(t, constants.ModelClaimRoutingStateActive, state)

	sleeping := warmModelClaimPod("p1", "default", nil)
	sleeping.Annotations[constants.ModelClaimPodAnnotationPrefix+"sleeping"] =
		`{"model":"m","port":0,"state":"sleeping"}`
	c.updatePod(active, sleeping)
	_, port, state, found = provider.ModelClaimBinding("m")
	require.True(t, found)
	assert.Zero(t, port)
	assert.Equal(t, constants.ModelClaimRoutingStateSleeping, state)
	assert.False(t, c.HasModel("m"))

	c.deletePod(sleeping)
	_, _, _, found = provider.ModelClaimBinding("m")
	assert.False(t, found)
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

// pendingModelClaim is a claim the controller has not placed: its Scheduled
// condition is False, and no pod advertises it.
func pendingModelClaim(namespace, name, served string) *modelv1alpha1.ModelClaim {
	claim := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: modelv1alpha1.ModelClaimStatus{
			Phase: modelv1alpha1.ModelClaimPending,
			Conditions: []metav1.Condition{{
				Type:   string(modelv1alpha1.ModelClaimConditionTypeScheduled),
				Status: metav1.ConditionFalse,
				Reason: "NoMatchingPods",
			}},
		},
	}
	if served != "" {
		claim.Spec.ModelName = ptr.To(served)
	}
	return claim
}

func TestModelClaimStatusKnowsAClaimNoPodAdvertises(t *testing.T) {
	c := NewForTest()
	handleDiscoveryObject(c, discovery.EventAdd, pendingModelClaim("default", "qwen-claim", "qwen"), nil)

	// No pod serves the model, so it is not routable, but it is claimed.
	assert.False(t, c.HasModel("qwen"))
	phase, reason, found := c.ModelClaimStatus("qwen")
	require.True(t, found)
	assert.Equal(t, string(modelv1alpha1.ModelClaimPending), phase)
	assert.Equal(t, "NoMatchingPods", reason)
	_, _, found = c.ModelClaimStatus("qwen-claim")
	assert.False(t, found, "a claim is known by the name it serves")
}

func TestModelClaimStatusFallsBackToTheClaimName(t *testing.T) {
	c := NewForTest()
	handleDiscoveryObject(c, discovery.EventAdd, pendingModelClaim("default", "gate-a", ""), nil)

	_, _, found := c.ModelClaimStatus("gate-a")
	assert.True(t, found)
}

func TestModelClaimStatusFollowsUpdatesAndDeletion(t *testing.T) {
	c := NewForTest()
	pending := pendingModelClaim("default", "qwen-claim", "qwen")
	handleDiscoveryObject(c, discovery.EventAdd, pending, nil)

	// Activation failed. As the controller writes it, Scheduled stays False
	// from the earlier refusal, and Ready says why the claim failed.
	failed := pending.DeepCopy()
	failed.Status.Phase = modelv1alpha1.ModelClaimFailed
	failed.Status.Conditions = append(failed.Status.Conditions, metav1.Condition{
		Type: string(modelv1alpha1.ModelClaimConditionReady), Status: metav1.ConditionFalse, Reason: "ActivateFailed",
	})
	handleDiscoveryObject(c, discovery.EventUpdate, failed, pending)
	phase, reason, found := c.ModelClaimStatus("qwen")
	require.True(t, found)
	assert.Equal(t, string(modelv1alpha1.ModelClaimFailed), phase)
	assert.Equal(t, "ActivateFailed", reason)

	// The served name changes: only the new one is claimed.
	renamed := failed.DeepCopy()
	renamed.Spec.ModelName = ptr.To("qwen2")
	handleDiscoveryObject(c, discovery.EventUpdate, renamed, failed)
	_, _, found = c.ModelClaimStatus("qwen")
	assert.False(t, found)
	_, _, found = c.ModelClaimStatus("qwen2")
	assert.True(t, found)

	handleDiscoveryObject(c, discovery.EventDelete, renamed, nil)
	_, _, found = c.ModelClaimStatus("qwen2")
	assert.False(t, found)
}

func TestModelClaimStatusForgetsAClaimBeingDeleted(t *testing.T) {
	c := NewForTest()
	pending := pendingModelClaim("default", "qwen-claim", "qwen")
	handleDiscoveryObject(c, discovery.EventAdd, pending, nil)

	deleting := pending.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	handleDiscoveryObject(c, discovery.EventUpdate, deleting, pending)

	_, _, found := c.ModelClaimStatus("qwen")
	assert.False(t, found, "the model a claim being deleted served is going away")
}

func TestModelClaimStatusWithOneNameClaimedTwice(t *testing.T) {
	c := NewForTest()
	first := pendingModelClaim("team-a", "qwen-claim", "qwen")
	second := pendingModelClaim("team-b", "qwen-claim", "qwen")
	second.Status.Conditions[0].Reason = "InvalidEngineConfig"
	handleDiscoveryObject(c, discovery.EventAdd, second, nil)
	handleDiscoveryObject(c, discovery.EventAdd, first, nil)

	// The first by namespace and name answers, as for bindings.
	_, reason, found := c.ModelClaimStatus("qwen")
	require.True(t, found)
	assert.Equal(t, "NoMatchingPods", reason)

	handleDiscoveryObject(c, discovery.EventDelete, first, nil)
	_, reason, found = c.ModelClaimStatus("qwen")
	require.True(t, found)
	assert.Equal(t, "InvalidEngineConfig", reason)
}
