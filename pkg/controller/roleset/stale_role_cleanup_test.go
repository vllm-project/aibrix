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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// newSweepReconciler builds a RoleSetReconciler for the sweep tests. It carries an event recorder
// because the removed-role pod deletes now flow through controllerdrain.DeletePods.
func newSweepReconciler(cli client.Client, scheme *runtime.Scheme) *RoleSetReconciler {
	return &RoleSetReconciler{Client: cli, Scheme: scheme, EventRecorder: record.NewFakeRecorder(16)}
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
// RoleSet, with no role label, or already terminating -- are left untouched.
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
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed
		assert.Equal(t, map[string]struct{}{"prefill-0": {}}, podNameSet(t, cli), "only the surviving role's pod must remain")
	})

	t.Run("Pooled: a removed role's podsets are deleted, the surviving role kept", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPodSet("prefill-ps", ns, "prefill", "test-roleset", testRoleSetUID),
			newOrphanPodSet("decode-ps", ns, "decode", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed
		assert.Equal(t, map[string]struct{}{"prefill-ps": {}}, podSetNameSet(t, cli), "only the surviving role's podset must remain")
	})

	t.Run("no-op when every role is still in the spec", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("worker-0", ns, "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPodSet("worker-ps", ns, "worker", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("worker")))
		assert.Len(t, podNameSet(t, cli), 1)
		assert.Len(t, podSetNameSet(t, cli), 1)
	})

	t.Run("does not delete a removed role's pod owned by a different RoleSet", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, "different-uid"),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed
		assert.Len(t, podNameSet(t, cli), 1, "a pod owned by another RoleSet must not be swept")
	})

	t.Run("does not delete a pod that has no role label", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("no-role-0", ns, "", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill")))
		assert.Len(t, podNameSet(t, cli), 1, "a pod with no role label must not be swept")
	})

	t.Run("sweeps both pods and podsets of a removed role at once", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("prefill-0", ns, "prefill", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPodSet("decode-ps", ns, "decode", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed
		assert.Equal(t, map[string]struct{}{"prefill-0": {}}, podNameSet(t, cli))
		assert.Empty(t, podSetNameSet(t, cli))
	})

	t.Run("skips objects already being deleted", func(t *testing.T) {
		// Stale objects that are already terminating must not be re-deleted (or re-logged) every
		// reconcile. A finalizer keeps them in the store so we can observe that no Delete is issued.
		hold := []string{"aibrix.ai/test-hold"}
		termPod := newOrphanPod("decode-term", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID)
		termPod.Finalizers = hold
		termPod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		termPodSet := newOrphanPodSet("decode-term-ps", ns, "decode", "test-roleset", testRoleSetUID)
		termPodSet.Finalizers = hold
		termPodSet.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		liveStalePod := newOrphanPod("decode-live", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID)

		var deleted []string
		cli := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(termPod, termPodSet, liveStalePod).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleted = append(deleted, obj.GetName())
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
		r := newSweepReconciler(cli, scheme)

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed
		assert.NotContains(t, deleted, "decode-term", "an already-terminating pod must not be re-deleted")
		assert.NotContains(t, deleted, "decode-term-ps", "an already-terminating podset must not be re-deleted")
		assert.Contains(t, deleted, "decode-live", "a live stale pod is still deleted")
	})

	t.Run("records a StaleRoleCleanup event naming the removed role and counts", func(t *testing.T) {
		// The immediate-delete (nil-spec) drain path and the plain PodSet Delete emit no event of
		// their own, so the sweep records one on the RoleSet to make the deletion visible.
		objs := []client.Object{
			newOrphanPod("decode-0", ns, "decode", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			newOrphanPodSet("decode-ps", ns, "decode", "test-roleset", testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		rec := record.NewFakeRecorder(16)
		r := &RoleSetReconciler{Client: cli, Scheme: scheme, EventRecorder: rec}

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("prefill"))) // "decode" removed

		select {
		case e := <-rec.Events:
			assert.Contains(t, e, StaleRoleCleanupEventType)
			assert.Contains(t, e, "1 pod(s) and 1 podset(s)")
			assert.Contains(t, e, "decode")
		default:
			t.Fatal("expected a StaleRoleCleanup event, got none")
		}
		select {
		case e := <-rec.Events:
			t.Fatalf("expected exactly one event, got an extra: %s", e)
		default:
		}
	})

	t.Run("records no event when nothing is swept", func(t *testing.T) {
		objs := []client.Object{
			newOrphanPod("worker-0", ns, "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
		}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		rec := record.NewFakeRecorder(16)
		r := &RoleSetReconciler{Client: cli, Scheme: scheme, EventRecorder: rec}

		require.NoError(t, r.cleanupStaleRoles(ctx, roleSetWithRoles("worker"))) // nothing removed

		select {
		case e := <-rec.Events:
			t.Fatalf("expected no event on a no-op sweep, got: %s", e)
		default:
		}
	})
}
