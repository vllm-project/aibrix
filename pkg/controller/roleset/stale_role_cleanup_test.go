/*
Copyright 2025 The Aibrix Team.

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// roleSetWithRoles returns the shared test RoleSet with the given role names in Spec.Roles.
func roleSetWithRoles(names ...string) *orchestrationv1alpha1.RoleSet {
	rs := newOrphanTestRoleSet("test-roleset", "test-ns")
	for _, n := range names {
		rs.Spec.Roles = append(rs.Spec.Roles, orchestrationv1alpha1.RoleSpec{Name: n})
	}
	return rs
}

func podNameSet(t *testing.T, cli client.Client) map[string]struct{} {
	t.Helper()
	list := &v1.PodList{}
	require.NoError(t, cli.List(context.Background(), list, client.InNamespace("test-ns")))
	out := map[string]struct{}{}
	for i := range list.Items {
		out[list.Items[i].Name] = struct{}{}
	}
	return out
}

func podSetNameSet(t *testing.T, cli client.Client) map[string]struct{} {
	t.Helper()
	list := &orchestrationv1alpha1.PodSetList{}
	require.NoError(t, cli.List(context.Background(), list, client.InNamespace("test-ns")))
	out := map[string]struct{}{}
	for i := range list.Items {
		out[list.Items[i].Name] = struct{}{}
	}
	return out
}

// TestRoleSetReconciler_CleanupStaleRoles covers the removed-role sweep (#3587): when a role is
// dropped from Spec.Roles (as an in-place StormService update does), the RoleSet-owned Pods and
// PodSets of that role must be deleted, while surviving roles -- and objects owned by a different
// RoleSet, or with no role label -- are left untouched.
func TestRoleSetReconciler_CleanupStaleRoles(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()
	const ns = "test-ns"

	t.Run("Replica+InPlaceUpdate: a removed role's pods are deleted, the surviving role kept", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("prefill-0", ns, "prefill", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPod("decode-1", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill")) // "decode" removed
		require.NoError(t, err)
		assert.True(t, changed)

		remaining := podNameSet(t, cli)
		assert.Equal(t, map[string]struct{}{"prefill-0": {}}, remaining, "only the surviving role's pod must remain")
	})

	t.Run("Pooled: a removed role's podsets are deleted, the surviving role kept", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPodSet("prefill-ps", ns, "prefill", "test-roleset", testRoleSetUID),
			newOrphanPodSet("decode-ps", ns, "decode", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill")) // "decode" removed
		require.NoError(t, err)
		assert.True(t, changed)

		assert.Equal(t, map[string]struct{}{"prefill-ps": {}}, podSetNameSet(t, cli), "only the surviving role's podset must remain")
	})

	t.Run("no-op when every role is still in the spec", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("worker-0", ns, "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPodSet("worker-ps", ns, "worker", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("worker"))
		require.NoError(t, err)
		assert.False(t, changed, "nothing is stale")
		assert.Len(t, podNameSet(t, cli), 1)
		assert.Len(t, podSetNameSet(t, cli), 1)
	})

	t.Run("does not delete a removed role's pod owned by a different RoleSet", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, "different-uid"),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill")) // "decode" removed
		require.NoError(t, err)
		assert.False(t, changed, "a pod owned by another RoleSet must not be swept")
		assert.Len(t, podNameSet(t, cli), 1)
	})

	t.Run("does not delete a pod that has no role label", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("no-role-0", ns, "", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))
		require.NoError(t, err)
		assert.False(t, changed, "a pod with no role label must not be swept")
		assert.Len(t, podNameSet(t, cli), 1)
	})

	t.Run("sweeps both pods and podsets of a removed role at once", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("prefill-0", ns, "prefill", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPodSet("decode-ps", ns, "decode", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &RoleSetReconciler{Client: cli, Scheme: scheme}

		changed, err := r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill")) // "decode" removed
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, map[string]struct{}{"prefill-0": {}}, podNameSet(t, cli))
		assert.Empty(t, podSetNameSet(t, cli))
	})
}
