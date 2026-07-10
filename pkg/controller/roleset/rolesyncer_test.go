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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const (
	newHash    = "new-hash"
	oldHash    = "old-hash"
	nginxImage = "nginx:v1"
	nginxV2    = "nginx:v2"
	testPodTwo = "pod-2"
)

var fakeComputeHashFunc = func(template *v1.PodTemplateSpec, collisionCount *int32) string {
	return newHash
}

func TestStatelessRoleSyncer_Scale(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	tests := []struct {
		name           string
		description    string
		roleSet        *orchestrationv1alpha1.RoleSet
		role           *orchestrationv1alpha1.RoleSpec
		existingPods   []*v1.Pod
		expectedChange bool
		expectedCreate int
		expectedDelete int
	}{
		{
			name:        "Scale up from 0 to 3 replicas",
			description: "Should create 3 pods when scaling up from 0",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role: newTestRoleSpec(
				"worker",
				3,
				intStrPtr(intstr.FromString("25%")),
				intStrPtr(intstr.FromString("25%")),
			),
			existingPods:   []*v1.Pod{},
			expectedChange: true,
			expectedCreate: 3,
			expectedDelete: 0,
		},
		{
			name:        "scale down from 5 to 2 replicas",
			description: "Should delete 3 pods when scaling down from 5 to 2",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 2, intStrPtr(intstr.FromString("25%")), intStrPtr(intstr.FromString("25%"))),
			existingPods: []*v1.Pod{
				newTestPod(testPodOne, "test-ns", "worker", "test-roleset", true, false),
				newTestPod(testPodTwo, "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-3", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-4", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-5", "test-ns", "worker", "test-roleset", true, false),
			},
			expectedChange: true,
			expectedCreate: 0,
			expectedDelete: 3,
		},
		{
			name:    "no change when replicas match",
			roleSet: newTestRoleSet("test-roleset", "test-ns"),
			role:    newTestRoleSpec("worker", 2, intStrPtr(intstr.FromString("25%")), intStrPtr(intstr.FromString("25%"))),
			existingPods: []*v1.Pod{
				newTestPod(testPodOne, "test-ns", "worker", "test-roleset", true, false),
				newTestPod(testPodTwo, "test-ns", "worker", "test-roleset", true, false),
			},
			expectedChange: false,
			expectedCreate: 0,
			expectedDelete: 0,
			description:    "Should not change when current replicas match expected",
		},
		{
			name:    "cleanup terminated pods",
			roleSet: newTestRoleSet("test-roleset", "test-ns"),
			role:    newTestRoleSpec("worker", 2, intStrPtr(intstr.FromString("25%")), intStrPtr(intstr.FromString("25%"))),
			existingPods: []*v1.Pod{
				newTestPod(testPodOne, "test-ns", "worker", "test-roleset", true, false),
				newTestPod(testPodTwo, "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-terminated", "test-ns", "worker", "test-roleset", false, true),
			},
			expectedChange: true,
			expectedCreate: 0,
			expectedDelete: 1,
			description:    "Should cleanup terminated pods",
		},
		{
			// readyPods: 2
			// expectedReplicas: 3
			// maxUnavailable: 1
			// minAvailable: 2 (expectedReplicas - maxUnavailable)
			// expectedDelete: 0 (readyPods - minAvailable)
			name:    "respect maxUnavailable during scale down",
			roleSet: newTestRoleSet("test-roleset", "test-ns"),
			role:    newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPod(testPodOne, "test-ns", "worker", "test-roleset", true, false),
				newTestPod(testPodTwo, "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-3", "test-ns", "worker", "test-roleset", false, false), // not ready
				newTestPod("pod-4", "test-ns", "worker", "test-roleset", false, false),
			},
			expectedChange: false,
			expectedCreate: 0,
			expectedDelete: 0,
			description:    "Should respect maxUnavailable constraint during scale down",
		},
		{
			// expectedReplicas: 5
			// activepPods: 2
			// maxSurge: 0
			// expectedCreate: 5-2+0-1=2 (expectedReplicas - activePods + maxSurge - terminatingPods)
			name:    "respect maxSurge during scale up",
			roleSet: newTestRoleSet("test-roleset", "test-ns"),
			role:    newTestRoleSpec("worker", 5, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(0))),
			existingPods: []*v1.Pod{
				newTestPod(testPodOne, "test-ns", "worker", "test-roleset", true, false),
				newTestPod(testPodTwo, "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-terminating", "test-ns", "worker", "test-roleset", true, true),
			},
			expectedChange: true,
			expectedCreate: 2,
			expectedDelete: 0,
			description:    "Should respect maxSurge constraint and account for terminating pods",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, len(tt.existingPods))
			for i, pod := range tt.existingPods {
				objs[i] = pod
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			syncer := &StatelessRoleSyncer{
				cli:             fakeClient,
				computeHashFunc: fakeComputeHashFunc,
			}

			initialPods := &v1.PodList{}
			err := fakeClient.List(ctx, initialPods)
			require.NoError(t, err)
			initialCount := len(initialPods.Items)

			changed, err := syncer.Scale(ctx, tt.roleSet, tt.role)

			assert.NoError(t, err, tt.description)

			assert.Equal(t, tt.expectedChange, changed, tt.description)

			finalPods := &v1.PodList{}
			err = fakeClient.List(ctx, finalPods)
			require.NoError(t, err)
			finalCount := len(finalPods.Items)
			netChange := finalCount - initialCount

			if tt.expectedCreate > 0 {
				assert.Equal(t, tt.expectedCreate, netChange, "Expected net increase in pods for scale up scenario")
			} else if tt.expectedDelete > 0 && tt.expectedCreate == 0 {
				assert.Equal(t, tt.expectedDelete, -netChange, "Expected net decrease in pods for scale down scenario")
			}
		})
	}
}

func TestStatelessRoleSyncer_Rollout(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	tests := []struct {
		name           string
		description    string
		roleSet        *orchestrationv1alpha1.RoleSet
		role           *orchestrationv1alpha1.RoleSpec
		existingPods   []*v1.Pod
		expectedError  bool
		expectedCreate int
		expectedDelete int
	}{
		{
			name:        "rollout from outdated to updated pods",
			description: "Should replace outdated pods with updated pods",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash),
				newTestPodWithHash(testPodTwo, "test-ns", true, false, oldHash),
				newTestPodWithHash("pod-3", "test-ns", true, false, oldHash),
			},
			expectedError:  false,
			expectedCreate: 1,
			expectedDelete: 1,
		},
		{
			name:        "rollout with partial updated pods",
			description: "Should only replace remaining outdated pods",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, newHash), // updated
				newTestPodWithHash(testPodTwo, "test-ns", true, false, oldHash), // outdated
				newTestPodWithHash("pod-3", "test-ns", true, false, oldHash),    // outdated
			},
			expectedError:  false,
			expectedCreate: 1,
			expectedDelete: 1,
		},
		{
			name:        "rollout with all pods already updated",
			description: "Should not create or delete any pods when all are updated",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, newHash),
				newTestPodWithHash(testPodTwo, "test-ns", true, false, newHash),
				newTestPodWithHash("pod-3", "test-ns", true, false, newHash),
			},
			expectedError:  false,
			expectedCreate: 0,
			expectedDelete: 0,
		},
		{
			name:        "rollout with not ready outdated pods",
			description: "Should delete not ready outdated pods immediately",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, newHash),  // updated & ready
				newTestPodWithHash(testPodTwo, "test-ns", false, false, oldHash), // outdated & not ready
				newTestPodWithHash("pod-3", "test-ns", true, false, oldHash),     // outdated & ready
			},
			expectedError:  false,
			expectedCreate: 1,
			expectedDelete: 1,
		},
		{
			name:        "rollout with terminating pods",
			description: "Should consider terminating pods in create budget",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash),
				newTestPodWithHash(testPodTwo, "test-ns", true, false, oldHash),
				newTestPodWithHash("pod-terminating", "test-ns", true, true, oldHash), // terminating
			},
			expectedError:  false,
			expectedCreate: 2,
			expectedDelete: 0,
		},
		{
			name:        "rollout with maxSurge=0",
			description: "Should respect maxSurge=0 constraint",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash),
				newTestPodWithHash(testPodTwo, "test-ns", true, false, oldHash),
				newTestPodWithHash("pod-3", "test-ns", true, false, oldHash),
			},
			expectedError:  false,
			expectedCreate: 0,
			expectedDelete: 1,
		},
		{
			name:        "rollout with maxUnavailable=0",
			description: "Should respect maxUnavailable=0 constraint",
			roleSet:     newTestRoleSet("test-roleset", "test-ns"),
			role:        newTestRoleSpec("worker", 3, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(0))),
			existingPods: []*v1.Pod{
				newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash),
				newTestPodWithHash(testPodTwo, "test-ns", true, false, oldHash),
				newTestPodWithHash("pod-3", "test-ns", true, false, oldHash),
			},
			expectedError:  false,
			expectedCreate: 1,
			expectedDelete: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, len(tt.existingPods))
			for i, pod := range tt.existingPods {
				objs[i] = pod
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			syncer := &StatelessRoleSyncer{
				cli:             fakeClient,
				computeHashFunc: fakeComputeHashFunc,
			}

			initialPods := &v1.PodList{}
			err := fakeClient.List(ctx, initialPods)
			require.NoError(t, err)
			initialCount := len(initialPods.Items)

			// Execute rollout
			err = syncer.Rollout(ctx, tt.roleSet, tt.role)

			// Assert error expectation
			if tt.expectedError {
				assert.Error(t, err, tt.description)
				return
			}
			assert.NoError(t, err, tt.description)

			// Count final pods
			finalPods := &v1.PodList{}
			err = fakeClient.List(ctx, finalPods)
			require.NoError(t, err)
			finalCount := len(finalPods.Items)

			// Calculate actual changes
			netChange := finalCount - initialCount

			// Verify the net change matches expectation
			expectedNetChange := tt.expectedCreate - tt.expectedDelete
			assert.Equal(t, expectedNetChange, netChange,
				"Expected net change: %d, actual: %d (create: %d, delete: %d)",
				expectedNetChange, netChange, tt.expectedCreate, tt.expectedDelete)
		})
	}
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleUsesInPlaceForImageOnlyChange(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()

	role.Template.Spec.Containers[0].Image = nginxV2
	pod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	pod.Name = testPodOne
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	recorder := record.NewFakeRecorder(1)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
		recorder:        recorder,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	pods := &v1.PodList{}
	require.NoError(t, fakeClient.List(ctx, pods))
	require.Len(t, pods.Items, 1)

	updated := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, nginxV2, updated.Spec.Containers[0].Image)
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])

	require.Len(t, recorder.Events, 1)
	event := <-recorder.Events
	assert.Contains(t, event, InPlaceUpdateStartedEventType)
	assert.Contains(t, event, testPodOne)
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleRecordsCompletedEvent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxV2

	pod, err := buildRenderedPod(roleSet, role, nil)
	require.NoError(t, err)
	pod.Name = testPodOne
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = newHash
	pod.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey] = `{"lastContainerStatuses":{"master":{"imageID":"docker-pullable://nginx@sha256:old"}}}`
	pod.Annotations[constants.RoleInPlaceUpdatePendingReasonAnnotationKey] = constants.RoleInPlaceUpdatePendingReasonImageIDUnchanged
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:    "master",
		Image:   nginxV2,
		ImageID: "docker-pullable://nginx@sha256:new",
		Ready:   true,
	}}

	recorder := record.NewFakeRecorder(1)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
		recorder:        recorder,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updated := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, newHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdatePendingReasonAnnotationKey)

	require.Len(t, recorder.Events, 1)
	event := <-recorder.Events
	assert.Contains(t, event, InPlaceUpdateCompletedEventType)
	assert.Contains(t, event, testPodOne)
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleFallsBackToRecreateForNonImageChange(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Command = []string{"serve", "--version=v1"}
	oldRole := role.DeepCopy()

	role.Template.Spec.Containers[0].Command = []string{"serve", "--version=v2"}
	pod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	pod.Name = testPodOne
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: "nginx",
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	recorder := record.NewFakeRecorder(1)
	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
		recorder:        recorder,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	pods := &v1.PodList{}
	require.NoError(t, fakeClient.List(ctx, pods))
	require.Len(t, pods.Items, 1)

	replacement := pods.Items[0]
	assert.NotEqual(t, testPodOne, replacement.Name)
	assert.Equal(t, []string{"serve", "--version=v2"}, replacement.Spec.Containers[0].Command)
	assert.NotEqual(t, oldHash, replacement.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, replacement.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)

	event := <-recorder.Events
	assert.Contains(t, event, InPlaceFallbackEventType)
	assert.Contains(t, event, "role worker pod pod-1 cannot be updated in place")
	assert.Contains(t, event, "non-image pod fields changed")
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleRespectsMaxUnavailable(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()

	role.Template.Spec.Containers[0].Image = nginxV2
	pod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	pod.Name = testPodOne
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}
	notReadyPod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	notReadyPod.Name = testPodTwo
	notReadyPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	notReadyPod.Status.Phase = v1.PodRunning
	notReadyPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}
	notReadyPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: false,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod, notReadyPod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updated := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, nginxImage, updated.Spec.Containers[0].Image)
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)

	updatedNotReady := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(notReadyPod), updatedNotReady))
	assert.Equal(t, nginxV2, updatedNotReady.Spec.Containers[0].Image)
	assert.Equal(t, newHash, updatedNotReady.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleWaitsForRuntimeImage(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxV2

	pod, err := buildRenderedPod(roleSet, role, nil)
	require.NoError(t, err)
	pod.Name = testPodOne
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = newHash
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updated := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, nginxV2, updated.Spec.Containers[0].Image)
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
}

func TestStatelessRoleSyncer_RolloutInPlaceIfPossibleCountsInProgressReadyPodsAgainstBudget(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()
	role.Template.Spec.Containers[0].Image = nginxV2

	inProgressPod, err := buildRenderedPod(roleSet, role, nil)
	require.NoError(t, err)
	inProgressPod.Name = testPodOne
	inProgressPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	inProgressPod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = newHash
	inProgressPod.Status.Phase = v1.PodRunning
	inProgressPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	inProgressPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	waitingPod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	waitingPod.Name = testPodTwo
	waitingPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	waitingPod.Status.Phase = v1.PodRunning
	waitingPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	waitingPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(inProgressPod, waitingPod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updatedWaiting := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(waitingPod), updatedWaiting))
	assert.Equal(t, nginxImage, updatedWaiting.Spec.Containers[0].Image)
	assert.NotContains(t, updatedWaiting.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
}

func TestStatefulRoleSyncer_RolloutInPlaceIfPossible(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.Stateful = true
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()
	role.Template.Spec.Containers[0].Image = nginxV2

	index := 0
	pod, err := buildRenderedPod(roleSet, oldRole, &index)
	require.NoError(t, err)
	pod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	pod.Status.Phase = v1.PodRunning
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	pod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	syncer := &StatefulRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updated := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, nginxV2, updated.Spec.Containers[0].Image)
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
}

func TestStatefulRoleSyncer_RolloutInPlaceIfPossibleCountsInProgressReadyPodsAgainstBudget(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.Stateful = true
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()
	role.Template.Spec.Containers[0].Image = nginxV2

	firstIndex := 0
	inProgressPod, err := buildRenderedPod(roleSet, role, &firstIndex)
	require.NoError(t, err)
	inProgressPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	inProgressPod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = newHash
	inProgressPod.Status.Phase = v1.PodRunning
	inProgressPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	inProgressPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	secondIndex := 1
	waitingPod, err := buildRenderedPod(roleSet, oldRole, &secondIndex)
	require.NoError(t, err)
	waitingPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	waitingPod.Status.Phase = v1.PodRunning
	waitingPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	waitingPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:  "master",
		Image: nginxImage,
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(inProgressPod, waitingPod).
		Build()

	syncer := &StatefulRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))

	updatedWaiting := &v1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(waitingPod), updatedWaiting))
	assert.Equal(t, nginxImage, updatedWaiting.Spec.Containers[0].Image)
	assert.NotContains(t, updatedWaiting.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
}

func TestStatelessRoleSyncer_RolloutByStepInPlaceIfPossibleUsesInPlaceForImageOnlyChange(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 2, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(1)))
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()
	role.Template.Spec.Containers[0].Image = nginxV2

	firstPod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	firstPod.Name = testPodOne
	firstPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	firstPod.Status.Phase = v1.PodRunning
	firstPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	firstPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:    "master",
		Image:   nginxImage,
		ImageID: "docker-pullable://nginx@sha256:old-1",
		Ready:   true,
	}}
	secondPod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)
	secondPod.Name = testPodTwo
	secondPod.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	secondPod.Status.Phase = v1.PodRunning
	secondPod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	secondPod.Status.ContainerStatuses = []v1.ContainerStatus{{
		Name:    "master",
		Image:   nginxImage,
		ImageID: "docker-pullable://nginx@sha256:old-2",
		Ready:   true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(firstPod, secondPod).
		Build()

	syncer := &StatelessRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	require.NoError(t, syncer.RolloutByStep(ctx, roleSet, role, 1))

	pods := &v1.PodList{}
	require.NoError(t, fakeClient.List(ctx, pods))
	require.Len(t, pods.Items, 2)

	var patched int
	for _, pod := range pods.Items {
		if pod.Spec.Containers[0].Image == nginxV2 {
			patched++
			assert.Equal(t, oldHash, pod.Labels[constants.RoleTemplateHashLabelKey])
			assert.Equal(t, newHash, pod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
		}
	}
	assert.Equal(t, 1, patched)
}

func TestPodSetRoleSyncer_RolloutInPlaceIfPossibleRecordsFallbackEvent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", 1, intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1)))
	role.PodGroupSize = ptr.To[int32](2)
	role.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType
	role.Template.Spec.Containers[0].Image = nginxImage
	oldRole := role.DeepCopy()
	role.Template.Spec.Containers[0].Image = nginxV2

	recorder := record.NewFakeRecorder(1)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	syncer := GetRoleSyncerWithRecorder(fakeClient, role, recorder)
	podSetSyncer, ok := syncer.(*PodSetRoleSyncer)
	require.True(t, ok)

	podSet := podSetSyncer.createPodSetForRole(roleSet, oldRole, ptr.To(0))
	podSet.Labels[constants.RoleTemplateHashLabelKey] = oldHash
	require.NoError(t, fakeClient.Create(ctx, podSet))

	require.NoError(t, syncer.Rollout(ctx, roleSet, role))
	require.Len(t, recorder.Events, 1)
	event := <-recorder.Events
	assert.Contains(t, event, InPlaceFallbackEventType)
	assert.Contains(t, event, "PodSet")
}

// Helper function to create a pod with specific template hash
func newTestPodWithHash(name, namespace string, ready, terminating bool, hash string) *v1.Pod {
	pod := newTestPod(name, namespace, "worker", "test-roleset", ready, terminating)
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[constants.RoleTemplateHashLabelKey] = hash
	return pod
}

func newTestRoleSet(name, namespace string) *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			UpdateStrategy: orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType,
		},
	}
}

func newTestPod(name, namespace, roleName, roleSetName string, ready, terminating bool) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.RoleNameLabelKey:         roleName,
				constants.RoleSetNameLabelKey:      roleSetName,
				constants.RoleTemplateHashLabelKey: "test-hash",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "master", Image: "nginx"},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}
	if ready {
		pod.Status.Conditions = []v1.PodCondition{
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
		}
	} else {
		pod.Status.Conditions = []v1.PodCondition{
			{
				Type:   v1.PodReady,
				Status: v1.ConditionFalse,
			},
		}
	}
	if terminating {
		pod.Status.Phase = v1.PodFailed
	}
	return pod
}

func newTestRoleSpec(
	name string,
	replicas int32,
	maxSurge, maxUnavailable *intstr.IntOrString,
) *orchestrationv1alpha1.RoleSpec {
	return &orchestrationv1alpha1.RoleSpec{
		Name:     name,
		Replicas: ptr.To(replicas),
		Template: v1.PodTemplateSpec{
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "master", Image: "nginx"},
				},
			},
		},
		UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
			MaxSurge:       maxSurge,
			MaxUnavailable: maxUnavailable,
		},
	}
}

func intStrPtr(i intstr.IntOrString) *intstr.IntOrString {
	return &i
}
