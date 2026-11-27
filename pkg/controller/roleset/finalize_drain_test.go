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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
)

// A RoleSet being deleted, with one pod in a role (podGroupSize 1), after one
// finalize pass.
func TestRoleSetFinalizeHonorsRoleDrain(t *testing.T) {
	drain30 := &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To(int32(30))}
	cases := []struct {
		name     string
		drain    *orchestrationv1alpha1.RoleDrainSpec
		ready    bool
		wantKept bool
	}{
		{"no drain configured (control)", nil, true, false},
		{"drain.timeoutSeconds 30", drain30, true, true},
		{"drain.timeoutSeconds 30, pod not ready", drain30, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newTestScheme(t)
			roleSet := newTestRoleSet("test-roleset", "test-ns")
			role := newTestRoleSpec("worker", 1, nil, nil) // 1 replica; podGroupSize unset
			role.Drain = tc.drain
			roleSet.Spec.Roles = []orchestrationv1alpha1.RoleSpec{*role}
			now := metav1.Now()
			roleSet.DeletionTimestamp = &now
			roleSet.Finalizers = []string{RoleSetFinalizer}

			pod := newTestPod("worker-0", "test-ns", "worker", "test-roleset", tc.ready, false)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleSet, pod).Build()
			r := &RoleSetReconciler{Client: cli, Scheme: scheme, EventRecorder: record.NewFakeRecorder(100)}

			done, requeueAfter, err := r.finalize(ctx, roleSet)
			require.NoError(t, err)
			assert.False(t, done)
			if tc.wantKept {
				// requeue when the drain window ends, not after the fixed default
				assert.InDelta(t, (30 * time.Second).Seconds(), requeueAfter.Seconds(), 1)
			} else {
				assert.Zero(t, requeueAfter)
			}

			got := &v1.Pod{}
			err = cli.Get(ctx, client.ObjectKeyFromObject(pod), got)
			kept := err == nil
			t.Logf("pod kept: %v, draining annotation: %q", kept, got.Annotations[aibrixconst.PodDrainingAnnotationKey])
			assert.Equal(t, tc.wantKept, kept, "pod kept after the first finalize pass")
			if tc.wantKept && kept {
				assert.Equal(t, "true", got.Annotations[aibrixconst.PodDrainingAnnotationKey])
			}
		})
	}
}

// A pod whose drain started before the window ended is deleted on the next
// finalize pass, and the pass after that completes finalization.
func TestRoleSetFinalizeDeletesPodAfterDrainWindow(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, nil, nil)
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To(int32(30))}
	roleSet.Spec.Roles = []orchestrationv1alpha1.RoleSpec{*role}
	now := metav1.Now()
	roleSet.DeletionTimestamp = &now
	roleSet.Finalizers = []string{RoleSetFinalizer}

	pod := newTestPod("worker-0", "test-ns", "worker", "test-roleset", true, false)
	pod.Annotations = map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    time.Now().Add(-31 * time.Second).UTC().Format(time.RFC3339),
		aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonScaleIn,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleSet, pod).Build()
	r := &RoleSetReconciler{Client: cli, Scheme: scheme, EventRecorder: record.NewFakeRecorder(100),
		DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())} // no PodGroup CRDs installed

	done, _, err := r.finalize(ctx, roleSet)
	require.NoError(t, err)
	assert.False(t, done)
	err = cli.Get(ctx, client.ObjectKeyFromObject(pod), &v1.Pod{})
	assert.True(t, apierrors.IsNotFound(err), "pod deleted once the drain window has passed")

	done, _, err = r.finalize(ctx, roleSet)
	require.NoError(t, err)
	assert.True(t, done, "finalize completes after the drained pod is gone")
}
