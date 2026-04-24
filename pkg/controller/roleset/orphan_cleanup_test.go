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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const testRoleSetUID types.UID = "test-roleset-uid-123"

func newOrphanTestRoleSet(name, namespace string) *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       testRoleSetUID,
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-storm-service",
			},
			Annotations: map[string]string{
				constants.RoleSetIndexAnnotationKey: "0",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			UpdateStrategy: orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType,
		},
	}
}

func newOrphanPod(name, namespace, roleName, roleSetName string, ownerKind string, ownerUID types.UID) *v1.Pod {
	controllerRef := true
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.RoleNameLabelKey:         roleName,
				constants.RoleSetNameLabelKey:      roleSetName,
				constants.RoleTemplateHashLabelKey: "test-hash",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: orchestrationv1alpha1.SchemeGroupVersion.String(),
					Kind:       ownerKind,
					Name:       roleSetName,
					UID:        ownerUID,
					Controller: &controllerRef,
				},
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "main", Image: "nginx"},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
			},
		},
	}
	return pod
}

func newOrphanPodSet(name, namespace, roleName, roleSetName string, ownerUID types.UID) *orchestrationv1alpha1.PodSet {
	controllerRef := true
	return &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.RoleNameLabelKey:         roleName,
				constants.RoleSetNameLabelKey:      roleSetName,
				constants.RoleTemplateHashLabelKey: "test-hash",
				constants.StormServiceNameLabelKey: "test-storm-service",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: orchestrationv1alpha1.SchemeGroupVersion.String(),
					Kind:       orchestrationv1alpha1.RoleSetKind,
					Name:       roleSetName,
					UID:        ownerUID,
					Controller: &controllerRef,
				},
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 3,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
		},
	}
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	return scheme
}

func TestPodSetRoleSyncer_Scale_CleansUpOrphanPods(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	tests := []struct {
		name              string
		description       string
		existingPods      []client.Object
		existingPodSets   []client.Object
		expectedPodCount  int
		expectedScaled    bool
		expectedPodSetCnt int
	}{
		{
			name:        "cleans up orphan pods owned by RoleSet when switching to PodSet mode",
			description: "When podGroupSize changes from 1 to 3, old direct Pods owned by RoleSet should be deleted",
			existingPods: []client.Object{
				newOrphanPod("pod-0", "test-ns", "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			},
			existingPodSets:   []client.Object{},
			expectedPodCount:  0,
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:        "does not clean up pods owned by PodSet",
			description: "Pods owned by PodSet should not be deleted during orphan cleanup",
			existingPods: []client.Object{
				newOrphanPod("pod-0", "test-ns", "worker", "test-roleset", orchestrationv1alpha1.PodSetKind, testRoleSetUID),
			},
			existingPodSets:   []client.Object{},
			expectedPodCount:  1,
			expectedScaled:    true, // Scale proceeds normally and creates PodSet
			expectedPodSetCnt: 1,
		},
		{
			name:              "no orphan pods - normal PodSet scale proceeds",
			description:       "When no orphan Pods exist, Scale should proceed normally and create PodSets",
			existingPods:      []client.Object{},
			existingPodSets:   []client.Object{},
			expectedPodCount:  0,
			expectedScaled:    true,
			expectedPodSetCnt: 1,
		},
		{
			name:        "cleans multiple orphan pods",
			description: "Multiple orphan Pods owned by RoleSet should all be cleaned up",
			existingPods: []client.Object{
				newOrphanPod("pod-0", "test-ns", "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
				newOrphanPod("pod-1", "test-ns", "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, testRoleSetUID),
			},
			existingPodSets:   []client.Object{},
			expectedPodCount:  0,
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:        "does not clean up pods owned by different RoleSet UID",
			description: "Pods owned by a RoleSet with the same name but different UID should not be deleted",
			existingPods: []client.Object{
				newOrphanPod("pod-0", "test-ns", "worker", "test-roleset", orchestrationv1alpha1.RoleSetKind, "different-uid"),
			},
			existingPodSets:   []client.Object{},
			expectedPodCount:  1,
			expectedScaled:    true, // Scale proceeds normally and creates PodSet
			expectedPodSetCnt: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var allObjects []client.Object
			allObjects = append(allObjects, tt.existingPods...)
			allObjects = append(allObjects, tt.existingPodSets...)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(allObjects...).
				Build()

			roleSet := newOrphanTestRoleSet("test-roleset", "test-ns")
			role := &orchestrationv1alpha1.RoleSpec{
				Name:         "worker",
				Replicas:     ptr.To(int32(1)),
				PodGroupSize: ptr.To(int32(3)),
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "main", Image: "nginx"},
						},
					},
				},
				UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
					MaxSurge:       intStrPtr(intstr.FromInt(1)),
					MaxUnavailable: intStrPtr(intstr.FromInt(1)),
				},
			}

			syncer := &PodSetRoleSyncer{
				cli:             fakeClient,
				computeHashFunc: fakeComputeHashFunc,
			}

			scaled, err := syncer.Scale(ctx, roleSet, role)
			require.NoError(t, err, tt.description)
			assert.Equal(t, tt.expectedScaled, scaled, tt.description)

			finalPods := &v1.PodList{}
			err = fakeClient.List(ctx, finalPods, client.InNamespace("test-ns"))
			require.NoError(t, err)
			assert.Equal(t, tt.expectedPodCount, len(finalPods.Items), "pod count mismatch: %s", tt.description)

			finalPodSets := &orchestrationv1alpha1.PodSetList{}
			err = fakeClient.List(ctx, finalPodSets, client.InNamespace("test-ns"))
			require.NoError(t, err)
			assert.Equal(t, tt.expectedPodSetCnt, len(finalPodSets.Items), "podset count mismatch: %s", tt.description)
		})
	}
}

func TestStatefulRoleSyncer_Scale_CleansUpOrphanPodSets(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	tests := []struct {
		name              string
		description       string
		existingPods      []client.Object
		existingPodSets   []client.Object
		expectedScaled    bool
		expectedPodSetCnt int
	}{
		{
			name:         "cleans up orphan PodSets when switching to Pod mode",
			description:  "When podGroupSize changes from 3 to 1, old PodSets should be deleted",
			existingPods: []client.Object{},
			existingPodSets: []client.Object{
				newOrphanPodSet("podset-0", "test-ns", "worker", "test-roleset", testRoleSetUID),
			},
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:              "no orphan PodSets - normal Pod scale proceeds",
			description:       "When no orphan PodSets exist, Scale should proceed normally",
			existingPods:      []client.Object{},
			existingPodSets:   []client.Object{},
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:         "cleans multiple orphan PodSets",
			description:  "Multiple orphan PodSets should all be cleaned up",
			existingPods: []client.Object{},
			existingPodSets: []client.Object{
				newOrphanPodSet("podset-0", "test-ns", "worker", "test-roleset", testRoleSetUID),
				newOrphanPodSet("podset-1", "test-ns", "worker", "test-roleset", testRoleSetUID),
			},
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:         "does not clean up PodSets owned by different RoleSet UID",
			description:  "PodSets owned by a RoleSet with the same name but different UID should not be deleted",
			existingPods: []client.Object{},
			existingPodSets: []client.Object{
				newOrphanPodSet("podset-0", "test-ns", "worker", "test-roleset", "different-uid"),
			},
			expectedScaled:    true, // Scale proceeds normally and creates Pod
			expectedPodSetCnt: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var allObjects []client.Object
			allObjects = append(allObjects, tt.existingPods...)
			allObjects = append(allObjects, tt.existingPodSets...)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(allObjects...).
				Build()

			roleSet := newOrphanTestRoleSet("test-roleset", "test-ns")
			role := &orchestrationv1alpha1.RoleSpec{
				Name:     "worker",
				Replicas: ptr.To(int32(1)),
				Stateful: true,
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "main", Image: "nginx"},
						},
					},
				},
				UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
					MaxSurge:       intStrPtr(intstr.FromInt(1)),
					MaxUnavailable: intStrPtr(intstr.FromInt(1)),
				},
			}

			syncer := &StatefulRoleSyncer{
				cli:             fakeClient,
				computeHashFunc: fakeComputeHashFunc,
			}

			scaled, err := syncer.Scale(ctx, roleSet, role)
			require.NoError(t, err, tt.description)
			assert.Equal(t, tt.expectedScaled, scaled, tt.description)

			finalPodSets := &orchestrationv1alpha1.PodSetList{}
			err = fakeClient.List(ctx, finalPodSets, client.InNamespace("test-ns"))
			require.NoError(t, err)
			assert.Equal(t, tt.expectedPodSetCnt, len(finalPodSets.Items), "podset count mismatch: %s", tt.description)
		})
	}
}

func TestStatelessRoleSyncer_Scale_CleansUpOrphanPodSets(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	tests := []struct {
		name              string
		description       string
		existingPodSets   []client.Object
		expectedScaled    bool
		expectedPodSetCnt int
	}{
		{
			name:        "cleans up orphan PodSets when switching to Pod mode",
			description: "When podGroupSize changes from 3 to 1, old PodSets should be deleted",
			existingPodSets: []client.Object{
				newOrphanPodSet("podset-0", "test-ns", "worker", "test-roleset", testRoleSetUID),
			},
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:              "no orphan PodSets - normal Pod scale proceeds",
			description:       "When no orphan PodSets exist, Scale should proceed normally",
			existingPodSets:   []client.Object{},
			expectedScaled:    true,
			expectedPodSetCnt: 0,
		},
		{
			name:        "does not clean up PodSets owned by different RoleSet UID",
			description: "PodSets owned by a RoleSet with the same name but different UID should not be deleted",
			existingPodSets: []client.Object{
				newOrphanPodSet("podset-0", "test-ns", "worker", "test-roleset", "different-uid"),
			},
			expectedScaled:    true, // Scale proceeds normally and creates Pod
			expectedPodSetCnt: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingPodSets...).
				Build()

			roleSet := newOrphanTestRoleSet("test-roleset", "test-ns")
			role := &orchestrationv1alpha1.RoleSpec{
				Name:     "worker",
				Replicas: ptr.To(int32(1)),
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "main", Image: "nginx"},
						},
					},
				},
				UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
					MaxSurge:       intStrPtr(intstr.FromInt(1)),
					MaxUnavailable: intStrPtr(intstr.FromInt(1)),
				},
			}

			syncer := &StatelessRoleSyncer{
				cli:             fakeClient,
				computeHashFunc: fakeComputeHashFunc,
			}

			scaled, err := syncer.Scale(ctx, roleSet, role)
			require.NoError(t, err, tt.description)
			assert.Equal(t, tt.expectedScaled, scaled, tt.description)

			finalPodSets := &orchestrationv1alpha1.PodSetList{}
			err = fakeClient.List(ctx, finalPodSets, client.InNamespace("test-ns"))
			require.NoError(t, err)
			assert.Equal(t, tt.expectedPodSetCnt, len(finalPodSets.Items), "podset count mismatch: %s", tt.description)
		})
	}
}

func TestCleanupOrphanPodSets_Idempotent(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	roleSet := newOrphanTestRoleSet("test-roleset", "test-ns")
	role := &orchestrationv1alpha1.RoleSpec{
		Name:     "worker",
		Replicas: ptr.To(int32(1)),
	}

	cleaned, err := cleanupOrphanPodSets(ctx, fakeClient, roleSet, role)
	require.NoError(t, err, "cleanup should not return error when no PodSets exist")
	assert.False(t, cleaned, "cleanup should report no change when no PodSets exist")
}

func TestCleanupOrphanPods_Idempotent(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	roleSet := newOrphanTestRoleSet("test-roleset", "test-ns")
	role := &orchestrationv1alpha1.RoleSpec{
		Name:     "worker",
		Replicas: ptr.To(int32(1)),
	}

	syncer := &PodSetRoleSyncer{
		cli:             fakeClient,
		computeHashFunc: fakeComputeHashFunc,
	}

	cleaned, err := syncer.cleanupOrphanPods(ctx, roleSet, role)
	require.NoError(t, err, "cleanup should not return error when no orphan Pods exist")
	assert.False(t, cleaned, "cleanup should report no change when no orphan Pods exist")
}
