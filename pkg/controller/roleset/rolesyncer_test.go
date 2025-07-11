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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const newHash = "new-hash"

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
			name:           "Scale up from 0 to 3 replicas",
			description:    "Should create 3 pods when scaling up from 0",
			roleSet:        newTestRoleSet("test-roleset", "test-ns"),
			role:           newTestRoleSpec("worker", 3, intStrPtr(intstr.FromString("25%")), intStrPtr(intstr.FromString("25%"))),
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
				newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-2", "test-ns", "worker", "test-roleset", true, false),
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
				newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-2", "test-ns", "worker", "test-roleset", true, false),
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
				newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-2", "test-ns", "worker", "test-roleset", true, false),
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
				newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-2", "test-ns", "worker", "test-roleset", true, false),
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
				newTestPod("pod-1", "test-ns", "worker", "test-roleset", true, false),
				newTestPod("pod-2", "test-ns", "worker", "test-roleset", true, false),
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
				newTestPodWithHash("pod-1", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-2", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-3", "test-ns", true, false, "old-hash"),
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
				newTestPodWithHash("pod-1", "test-ns", true, false, newHash),    // updated
				newTestPodWithHash("pod-2", "test-ns", true, false, "old-hash"), // outdated
				newTestPodWithHash("pod-3", "test-ns", true, false, "old-hash"), // outdated
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
				newTestPodWithHash("pod-1", "test-ns", true, false, newHash),
				newTestPodWithHash("pod-2", "test-ns", true, false, newHash),
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
				newTestPodWithHash("pod-1", "test-ns", true, false, newHash),     // updated & ready
				newTestPodWithHash("pod-2", "test-ns", false, false, "old-hash"), // outdated & not ready
				newTestPodWithHash("pod-3", "test-ns", true, false, "old-hash"),  // outdated & ready
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
				newTestPodWithHash("pod-1", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-2", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-terminating", "test-ns", true, true, "old-hash"), // terminating
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
				newTestPodWithHash("pod-1", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-2", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-3", "test-ns", true, false, "old-hash"),
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
				newTestPodWithHash("pod-1", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-2", "test-ns", true, false, "old-hash"),
				newTestPodWithHash("pod-3", "test-ns", true, false, "old-hash"),
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

func newTestRoleSpec(name string, replicas int32, maxSurge, maxUnavailable *intstr.IntOrString) *orchestrationv1alpha1.RoleSpec {
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
