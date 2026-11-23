/*
Copyright 2024 The Aibrix Team.

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

func newRevertTestClient(t *testing.T, pods ...*v1.Pod) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	objs := make([]client.Object, 0, len(pods))
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func markRolloutDraining(pod *v1.Pod) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[aibrixconst.PodDrainingAnnotationKey] = "true"
	pod.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey] = time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339)
	pod.Annotations[aibrixconst.PodDrainReasonAnnotationKey] = aibrixconst.PodDrainReasonRollout
	pod.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey] = aibrixconst.PodDrainTargetActionDelete
}

func drainingPodNames(t *testing.T, c client.Client) []string {
	t.Helper()
	pods := &v1.PodList{}
	require.NoError(t, c.List(context.Background(), pods))
	var names []string
	for i := range pods.Items {
		if podutil.IsPodDraining(&pods.Items[i]) {
			names = append(names, pods.Items[i].Name)
		}
	}
	return names
}

// TestStatelessScaleCancelsRolloutDrainAfterRevert covers #3572: a rollout
// drains an outdated pod, the template is changed back before the drain
// completes, and the pod matches the current template again.
func TestStatelessScaleCancelsRolloutDrainAfterRevert(t *testing.T) {
	ctx := context.Background()
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	timeout := int32(60)
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout}
	c := newRevertTestClient(t,
		newTestPodWithHash("pod-a", "test-ns", true, false, oldHash),
		newTestPodWithHash("pod-b", "test-ns", true, false, oldHash))
	syncer := &StatelessRoleSyncer{cli: c, computeHashFunc: fakeComputeHashFunc, recorder: record.NewFakeRecorder(10)}

	// The rollout to newHash starts draining one of the two pods.
	_, err := syncer.Rollout(ctx, roleSet, role)
	require.NoError(t, err)
	require.Len(t, drainingPodNames(t, c), 1)

	// The template goes back to oldHash before the drain completes.
	syncer.computeHashFunc = func(*v1.PodTemplateSpec, *int32) string { return oldHash }
	result, err := syncer.Scale(ctx, roleSet, role)

	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Empty(t, drainingPodNames(t, c), "the pod matches the current template again, so its rollout drain should be cancelled")
	pods := &v1.PodList{}
	require.NoError(t, c.List(ctx, pods))
	require.Len(t, pods.Items, 2)
	for i := range pods.Items {
		assert.Empty(t, pods.Items[i].Annotations[aibrixconst.PodDrainReasonAnnotationKey])
		assert.Empty(t, pods.Items[i].Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
	}
}

func TestStatefulScaleCancelsRolloutDrainAfterRevert(t *testing.T) {
	ctx := context.Background()
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	timeout := int32(60)
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout}
	pod0 := newTestPod("pod-0", "test-ns", "worker", "test-roleset", true, false)
	pod0.Annotations = map[string]string{constants.RoleReplicaIndexAnnotationKey: "0"}
	markRolloutDraining(pod0)
	pod1 := newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false)
	pod1.Annotations = map[string]string{constants.RoleReplicaIndexAnnotationKey: "1"}
	c := newRevertTestClient(t, pod0, pod1)
	// newTestPod labels pods with "test-hash", which is the current template here.
	syncer := &StatefulRoleSyncer{
		cli:             c,
		computeHashFunc: func(*v1.PodTemplateSpec, *int32) string { return "test-hash" },
		recorder:        record.NewFakeRecorder(10),
	}

	result, err := syncer.Scale(ctx, roleSet, role)

	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Empty(t, drainingPodNames(t, c))
}

// TestScaleKeepsRolloutDrainOnOutdatedPod checks that a rollout drain on a pod
// that is still outdated is left to the rollout.
func TestScaleKeepsRolloutDrainOnOutdatedPod(t *testing.T) {
	ctx := context.Background()
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	timeout := int32(60)
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout}
	outdated := newTestPodWithHash("pod-a", "test-ns", true, false, oldHash)
	markRolloutDraining(outdated)
	c := newRevertTestClient(t, outdated, newTestPodWithHash("pod-b", "test-ns", true, false, oldHash))
	syncer := &StatelessRoleSyncer{cli: c, computeHashFunc: fakeComputeHashFunc, recorder: record.NewFakeRecorder(10)}

	result, err := syncer.Scale(ctx, roleSet, role)

	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.Equal(t, []string{"pod-a"}, drainingPodNames(t, c))
}
