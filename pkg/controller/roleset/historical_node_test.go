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

package roleset

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestRefreshHistoricalNodeBindingsFromPods(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	statefulRole := historicalNodeRole("decode", true, 2, intstr.FromInt(1), intstr.FromInt(1))
	statelessRole := historicalNodeRole("prefill", false, 3, intstr.FromInt(1), intstr.FromInt(1))
	pods := []*v1.Pod{
		historicalNodePod("decode-0", "test-ns", "decode", "test-roleset", "node-a", ptr.To(0)),
		historicalNodePod("decode-1", "test-ns", "decode", "test-roleset", "node-b", ptr.To(1)),
		historicalNodePod("prefill-0", "test-ns", "prefill", "test-roleset", "node-c", nil),
		historicalNodePod("prefill-1", "test-ns", "prefill", "test-roleset", "node-d", nil),
		historicalNodePod("prefill-2", "test-ns", "prefill", "test-roleset", "node-c", nil),
		historicalNodePod("unscheduled", "test-ns", "prefill", "test-roleset", "", nil),
	}

	bindings, changed := refreshHistoricalNodeBindingsFromPods(rs, []orchestrationv1alpha1.RoleSpec{*statefulRole, *statelessRole}, pods)

	assert.True(t, changed)
	assert.Equal(t, "node-a", bindings.ReplicaSlots["decode/0"])
	assert.Equal(t, "node-b", bindings.ReplicaSlots["decode/1"])
	assert.Equal(t, []string{"node-c", "node-d"}, bindings.Roles["prefill"])
}

func TestRefreshHistoricalNodeBindingsRebuildsInvalidAnnotation(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	rs.Annotations = map[string]string{
		constants.RoleSetHistoricalNodeBindingsAnnotationKey: "{invalid-json",
	}

	bindings, changed := refreshHistoricalNodeBindingsFromPods(rs, nil, nil)

	assert.True(t, changed)
	assert.Empty(t, bindings.ReplicaSlots)
	assert.Empty(t, bindings.Roles)
}

func TestRefreshHistoricalNodeBindingsSortsPodsDeterministically(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	statelessRole := historicalNodeRole("prefill", false, 3, intstr.FromInt(1), intstr.FromInt(1))
	oldest := metav1.NewTime(mustParseTime(t, "2026-01-01T00:00:00Z"))
	middle := metav1.NewTime(mustParseTime(t, "2026-01-02T00:00:00Z"))
	newest := metav1.NewTime(mustParseTime(t, "2026-01-03T00:00:00Z"))
	pods := []*v1.Pod{
		historicalNodePod("prefill-newest", "test-ns", "prefill", "test-roleset", "node-newest", nil),
		historicalNodePod("prefill-oldest", "test-ns", "prefill", "test-roleset", "node-oldest", nil),
		historicalNodePod("prefill-middle", "test-ns", "prefill", "test-roleset", "node-middle", nil),
	}
	pods[0].CreationTimestamp = newest
	pods[1].CreationTimestamp = oldest
	pods[2].CreationTimestamp = middle

	bindings, changed := refreshHistoricalNodeBindingsFromPods(
		rs,
		[]orchestrationv1alpha1.RoleSpec{*statelessRole},
		pods,
	)

	assert.True(t, changed)
	assert.Equal(t, []string{"node-newest", "node-middle", "node-oldest"}, bindings.Roles["prefill"])
}

func TestRefreshHistoricalNodeBindingsPrunesStaleEntries(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	setHistoricalNodeBindingsAnnotation(t, rs, historicalNodeBindings{
		ReplicaSlots: map[string]string{
			"decode/0":       "node-a",
			"decode/1":       "node-b",
			"decode/9":       "node-stale-slot",
			"removed-role/0": "node-stale-role",
		},
		Roles: map[string][]string{
			"prefill":      {"node-c"},
			"removed-role": {"node-stale-role"},
		},
	})
	statefulRole := historicalNodeRole("decode", true, 1, intstr.FromInt(1), intstr.FromInt(1))
	statelessRole := historicalNodeRole("prefill", false, 2, intstr.FromInt(1), intstr.FromInt(1))

	bindings, changed := refreshHistoricalNodeBindingsFromPods(
		rs,
		[]orchestrationv1alpha1.RoleSpec{*statefulRole, *statelessRole},
		nil,
	)

	assert.True(t, changed)
	assert.Equal(t, map[string]string{"decode/0": "node-a"}, bindings.ReplicaSlots)
	assert.Equal(t, map[string][]string{"prefill": {"node-c"}}, bindings.Roles)
}

func TestHistoricalNodeBindingsForPodCreationSkipsDisabledRoleBeforeParsing(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	rs.Annotations = map[string]string{
		constants.RoleSetHistoricalNodeBindingsAnnotationKey: "{invalid-json",
	}
	role := historicalNodeRole("worker", false, 1, intstr.FromInt(1), intstr.FromInt(1))
	role.UpdateStrategy.ReplacementScheduling = nil

	bindings := historicalNodeBindingsForPodCreation(rs, role)

	assert.Nil(t, bindings)
}

func TestMaybeInjectHistoricalNodeAffinity(t *testing.T) {
	rs := newTestRoleSet("test-roleset", "test-ns")
	bindings := historicalNodeBindings{
		ReplicaSlots: map[string]string{"decode/0": "node-a"},
		Roles:        map[string][]string{"prefill": {"node-c", "node-d"}},
	}

	t.Run("stateful slot", func(t *testing.T) {
		role := historicalNodeRole("decode", true, 1, intstr.FromInt(1), intstr.FromInt(1))
		pod := historicalNodePod("decode-0-new", "test-ns", "decode", "test-roleset", "", ptr.To(0))

		injected := maybeInjectHistoricalNodeAffinity(rs, role, pod, &bindings, ptr.To(0), false)

		assert.True(t, injected)
		assert.Equal(t, []string{"node-a"}, historicalNodeAffinityValues(t, pod))
	})

	t.Run("stateless role", func(t *testing.T) {
		role := historicalNodeRole("prefill", false, 2, intstr.FromInt(1), intstr.FromInt(1))
		pod := historicalNodePod("prefill-new", "test-ns", "prefill", "test-roleset", "", nil)

		injected := maybeInjectHistoricalNodeAffinity(rs, role, pod, &bindings, nil, false)

		assert.True(t, injected)
		assert.Equal(t, []string{"node-c", "node-d"}, historicalNodeAffinityValues(t, pod))
	})

	t.Run("required node affinity skip", func(t *testing.T) {
		role := historicalNodeRole("prefill", false, 2, intstr.FromInt(1), intstr.FromInt(1))
		role.Template.Spec.Affinity = &v1.Affinity{
			NodeAffinity: &v1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{},
			},
		}
		pod := historicalNodePod("prefill-new", "test-ns", "prefill", "test-roleset", "", nil)
		pod.Spec.Affinity = role.Template.Spec.Affinity

		injected := maybeInjectHistoricalNodeAffinity(rs, role, pod, &bindings, nil, false)

		assert.False(t, injected)
		assert.Empty(t, historicalNodeAffinityValues(t, pod))
	})

	t.Run("required hostname topology skip", func(t *testing.T) {
		role := historicalNodeRole("prefill", false, 2, intstr.FromInt(1), intstr.FromInt(1))
		rsWithTopology := rs.DeepCopy()
		rsWithTopology.Spec.TopologyPolicy = &orchestrationv1alpha1.TopologyPolicy{
			Scope: orchestrationv1alpha1.TopologyRoleSetScope,
			Mode:  orchestrationv1alpha1.TopologyPolicyRequired,
			Key:   v1.LabelHostname,
		}
		pod := historicalNodePod("prefill-new", "test-ns", "prefill", "test-roleset", "", nil)

		injected := maybeInjectHistoricalNodeAffinity(rsWithTopology, role, pod, &bindings, nil, false)

		assert.False(t, injected)
		assert.Empty(t, historicalNodeAffinityValues(t, pod))
	})
}

func TestHistoricalNodeAffinitySyncers(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	t.Run("stateful empty slot creation uses existing binding", func(t *testing.T) {
		rs := newTestRoleSet("test-roleset", "test-ns")
		setHistoricalNodeBindingsAnnotation(t, rs, historicalNodeBindings{
			ReplicaSlots: map[string]string{"worker/0": "node-a"},
		})
		role := historicalNodeRole("worker", true, 1, intstr.FromInt(0), intstr.FromInt(1))
		syncer := &StatefulRoleSyncer{
			cli:             fake.NewClientBuilder().WithScheme(scheme).Build(),
			computeHashFunc: fakeComputeHashFunc,
			recorder:        record.NewFakeRecorder(1),
		}

		changed, err := syncer.Scale(ctx, rs, role)

		require.NoError(t, err)
		assert.True(t, changed)
		pods := &v1.PodList{}
		require.NoError(t, syncer.cli.List(ctx, pods))
		require.Len(t, pods.Items, 1)
		assert.Equal(t, []string{"node-a"}, historicalNodeAffinityValues(t, &pods.Items[0]))
	})

	t.Run("stateless rollout replacement uses recent nodes", func(t *testing.T) {
		rs := newTestRoleSet("test-roleset", "test-ns")
		setHistoricalNodeBindingsAnnotation(t, rs, historicalNodeBindings{
			Roles: map[string][]string{"worker": {"node-c", "node-d"}},
		})
		role := historicalNodeRole("worker", false, 2, intstr.FromInt(1), intstr.FromInt(0))
		oldPod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oldPod).Build()
		syncer := &StatelessRoleSyncer{cli: fakeClient, computeHashFunc: fakeComputeHashFunc}

		err := syncer.Rollout(ctx, rs, role)

		require.NoError(t, err)
		pods := &v1.PodList{}
		require.NoError(t, fakeClient.List(ctx, pods))
		var replacement *v1.Pod
		for i := range pods.Items {
			if assert.ObjectsAreEqual([]string{"node-c", "node-d"}, historicalNodeAffinityValues(t, &pods.Items[i])) {
				replacement = &pods.Items[i]
				break
			}
		}
		require.NotNil(t, replacement)
		assert.Equal(t, []string{"node-c", "node-d"}, historicalNodeAffinityValues(t, replacement))
	})

	t.Run("stateless scale-up does not inject", func(t *testing.T) {
		rs := newTestRoleSet("test-roleset", "test-ns")
		setHistoricalNodeBindingsAnnotation(t, rs, historicalNodeBindings{
			Roles: map[string][]string{"worker": {"node-c"}},
		})
		role := historicalNodeRole("worker", false, 1, intstr.FromInt(1), intstr.FromInt(1))
		syncer := &StatelessRoleSyncer{
			cli:             fake.NewClientBuilder().WithScheme(scheme).Build(),
			computeHashFunc: fakeComputeHashFunc,
		}

		changed, err := syncer.Scale(ctx, rs, role)

		require.NoError(t, err)
		assert.True(t, changed)
		pods := &v1.PodList{}
		require.NoError(t, syncer.cli.List(ctx, pods))
		require.Len(t, pods.Items, 1)
		assert.Empty(t, historicalNodeAffinityValues(t, &pods.Items[0]))
	})
}

func historicalNodeRole(name string, stateful bool, replicas int32, maxSurge, maxUnavailable intstr.IntOrString) *orchestrationv1alpha1.RoleSpec {
	role := newTestRoleSpec(name, replicas, &maxSurge, &maxUnavailable)
	role.Stateful = stateful
	role.UpdateStrategy.ReplacementScheduling = &orchestrationv1alpha1.RoleReplacementScheduling{
		HistoricalNode: &orchestrationv1alpha1.HistoricalNodeSchedulingPolicy{
			Mode: orchestrationv1alpha1.HistoricalNodeSchedulingPreferred,
		},
	}
	return role
}

func historicalNodePod(name, namespace, roleName, roleSetName, nodeName string, index *int) *v1.Pod {
	pod := newTestPod(name, namespace, roleName, roleSetName, true, false)
	pod.Spec.NodeName = nodeName
	if index != nil {
		pod.Annotations = map[string]string{
			constants.RoleReplicaIndexAnnotationKey: strconv.Itoa(*index),
		}
	}
	return pod
}

func setHistoricalNodeBindingsAnnotation(t *testing.T, rs *orchestrationv1alpha1.RoleSet, bindings historicalNodeBindings) {
	t.Helper()
	data, err := json.Marshal(bindings)
	require.NoError(t, err)
	if rs.Annotations == nil {
		rs.Annotations = map[string]string{}
	}
	rs.Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey] = string(data)
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

func historicalNodeAffinityValues(t *testing.T, pod *v1.Pod) []string {
	t.Helper()
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	preferred := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	for _, term := range preferred {
		for _, expr := range term.Preference.MatchExpressions {
			if expr.Key == v1.LabelHostname && expr.Operator == v1.NodeSelectorOpIn {
				return expr.Values
			}
		}
	}
	return nil
}
