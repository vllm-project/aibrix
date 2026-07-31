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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestGetReadyReplicaCountForRole(t *testing.T) {
	readyPod := makeReadyPod("ready1")
	notReadyPod := makeNotReadyPod("notready1")
	pods := []*corev1.Pod{readyPod, readyPod, notReadyPod}
	count := GetReadyReplicaCountForRole(pods)
	assert.Equal(t, int32(2), count)
}

func TestSetAndRemoveRoleSetCondition(t *testing.T) {
	status := &orchestrationv1alpha1.RoleSetStatus{}
	now := metav1.Now()

	cond := orchestrationv1alpha1.Condition{
		Type:               orchestrationv1alpha1.RoleSetReady,
		Status:             corev1.ConditionTrue,
		Reason:             "AllReplicasReady",
		LastTransitionTime: &now,
	}

	SetRoleSetCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// Duplicate condition (same status and reason) shouldn't be added
	SetRoleSetCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// Remove condition
	RemoveRoleSetCondition(status, orchestrationv1alpha1.RoleSetReady)
	assert.Len(t, status.Conditions, 0)
}

func TestMaxSurgeAndUnavailable(t *testing.T) {
	replicas := int32(10)
	surge := intstrutil.FromString("20%") // should be 2
	unavail := intstrutil.FromInt(3)      // 3

	role := &orchestrationv1alpha1.RoleSpec{
		Replicas: &replicas,
		UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
			MaxSurge:       &surge,
			MaxUnavailable: &unavail,
		},
	}

	assert.Equal(t, int32(2), MaxSurge(role))
	assert.Equal(t, int32(3), MaxUnavailable(role))
}

func TestRenderStormServicePod_WithRoleIndex(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-set",
			Namespace: "test-ns",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
				"name":                             "test-name",
				"other-label":                      "should-not-copy",
			},
			Annotations: map[string]string{
				constants.RoleSetIndexAnnotationKey: "1",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	roleIndex := 0
	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}

	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, &roleIndex)

	// Verify labels
	assert.Equal(t, "test-role-set", pod.Labels[constants.RoleSetNameLabelKey])
	assert.Equal(t, "test-role", pod.Labels[constants.RoleNameLabelKey])
	assert.Equal(t, "test-service", pod.Labels[constants.StormServiceNameLabelKey])
	assert.Equal(t, "0", pod.Labels[constants.RoleReplicaIndexLabelKey])
	assert.Equal(t, "test-name", pod.Labels["name"])
	assert.NotContains(t, pod.Labels, "other-label")

	// Verify annotations
	assert.Equal(t, "1", pod.Annotations[constants.RoleSetIndexAnnotationKey])
	assert.Equal(t, "0", pod.Annotations[constants.RoleReplicaIndexAnnotationKey])

	// Verify hostname and subdomain
	assert.Equal(t, pod.Name, pod.Spec.Hostname)
	assert.Equal(t, pod.Labels[constants.StormServiceNameLabelKey], pod.Spec.Subdomain)
}

func TestRenderStormServicePod_WithoutRoleIndex(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-set",
			Namespace: "test-ns",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, nil)

	// Verify replica index is not set
	assert.NotContains(t, pod.Labels, constants.RoleReplicaIndexLabelKey)
	assert.NotContains(t, pod.Annotations, constants.RoleReplicaIndexAnnotationKey)
}

func TestRenderStormServicePod_WithRoleSetCoschedulingPodGroup(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			SchedulingStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				CoschedulingSchedulingStrategy: &orchestrationv1alpha1.CoschedulingSchedulingStrategySpec{
					MinMember: 4,
				},
			},
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	roleIndex := 0
	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, &roleIndex)

	// Verify pod group labels and annotations
	assert.Equal(t, "test-role-set", pod.Labels[constants.CoschedulingPodGroupNameLabelKey])
}

func TestRenderStormServicePod_WithRoleSetGodelPodGroup(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			SchedulingStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				GodelSchedulingStrategy: &orchestrationv1alpha1.GodelSchedulingStrategySpec{
					MinMember: 3,
				},
			},
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	roleIndex := 0
	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, &roleIndex)

	// Verify pod group labels and annotations
	assert.Equal(t, "test-role-set", pod.Labels[constants.GodelPodGroupNameAnnotationKey])
	assert.Equal(t, "test-role-set", pod.Annotations[constants.GodelPodGroupNameAnnotationKey])
}

func TestRenderStormServicePod_WithRoleSetVolcanoPodGroup(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			SchedulingStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				VolcanoSchedulingStrategy: &orchestrationv1alpha1.VolcanoSchedulingStrategySpec{
					MinMember: 3,
				},
			},
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	roleIndex := 0
	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, &roleIndex)

	// Verify pod group labels and annotations
	assert.Equal(t, "test-role-set", pod.Labels[constants.VolcanoPodGroupNameAnnotationKey])
	assert.Equal(t, "test-role-set", pod.Annotations[constants.VolcanoPodGroupNameAnnotationKey])
}

func TestRenderStormServicePod_EmptyLabelsAndAnnotations(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test-container"},
							},
						},
					},
				},
			},
		},
	}

	pod := &corev1.Pod{}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, nil)

	// Verify basic labels are set even when roleSet has no labels
	assert.Equal(t, "test-role-set", pod.Labels[constants.RoleSetNameLabelKey])
	assert.Equal(t, "test-role", pod.Labels[constants.RoleNameLabelKey])
	assert.Equal(t, "", pod.Labels[constants.StormServiceNameLabelKey])
}

func TestRenderStormServicePod_MultipleContainers(t *testing.T) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name: "test-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "container1"},
								{Name: "container2"},
							},
						},
					},
				},
			},
		},
	}

	// pod is supposed to be built from the pod template, here, we clone 2 containers from role template.
	pod := &corev1.Pod{
		Spec: *roleSet.Spec.Roles[0].Template.Spec.DeepCopy(),
	}
	renderStormServicePod(roleSet, &roleSet.Spec.Roles[0], pod, nil)

	// Verify all containers get env vars injected
	assert.Len(t, pod.Spec.Containers, 2)
	for _, c := range pod.Spec.Containers {
		assert.Len(t, c.Env, 5)
	}
}

func makeReadyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

func makeNotReadyPod(name string) *corev1.Pod {
	pod := makeReadyPod(name)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	return pod
}

func TestSortRolesByUpgradeOrder(t *testing.T) {
	int32Ptr := func(i int32) *int32 { return &i }

	tests := []struct {
		name     string
		roles    []orchestrationv1alpha1.RoleSpec
		expected []orchestrationv1alpha1.RoleSpec
	}{
		{
			name:     "empty roles",
			roles:    []orchestrationv1alpha1.RoleSpec{},
			expected: []orchestrationv1alpha1.RoleSpec{},
		},
		{
			name: "already sorted roles",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(2)},
				{Name: "role3", UpgradeOrder: int32Ptr(3)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(2)},
				{Name: "role3", UpgradeOrder: int32Ptr(3)},
			},
		},
		{
			name: "unsorted roles",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "role3", UpgradeOrder: int32Ptr(3)},
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(2)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(2)},
				{Name: "role3", UpgradeOrder: int32Ptr(3)},
			},
		},
		{
			name: "roles with nil upgrade order",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "role3", UpgradeOrder: int32Ptr(2)},
				{Name: "role1", UpgradeOrder: nil},
				{Name: "role2", UpgradeOrder: int32Ptr(1)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "role2", UpgradeOrder: int32Ptr(1)},
				{Name: "role3", UpgradeOrder: int32Ptr(2)},
				{Name: "role1", UpgradeOrder: nil},
			},
		},
		{
			name: "roles with same upgrade order",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(1)},
				{Name: "role3", UpgradeOrder: int32Ptr(1)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "role1", UpgradeOrder: int32Ptr(1)},
				{Name: "role2", UpgradeOrder: int32Ptr(1)},
				{Name: "role3", UpgradeOrder: int32Ptr(1)},
			},
		},
		{
			name: "mix of nil and non-nil upgrade orders",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "role4", UpgradeOrder: int32Ptr(2)},
				{Name: "role1", UpgradeOrder: nil},
				{Name: "role2", UpgradeOrder: nil},
				{Name: "role3", UpgradeOrder: int32Ptr(1)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "role3", UpgradeOrder: int32Ptr(1)},
				{Name: "role4", UpgradeOrder: int32Ptr(2)},
				{Name: "role1", UpgradeOrder: nil},
				{Name: "role2", UpgradeOrder: nil},
			},
		},
		{
			name: "multiple roles with explicit order and one missing (real-world scenario)",
			roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "api-server", UpgradeOrder: int32Ptr(2)},
				{Name: "database", UpgradeOrder: nil}, // Missing - should upgrade LAST
				{Name: "cache", UpgradeOrder: int32Ptr(1)},
				{Name: "monitoring", UpgradeOrder: int32Ptr(3)},
			},
			expected: []orchestrationv1alpha1.RoleSpec{
				{Name: "cache", UpgradeOrder: int32Ptr(1)},      // First
				{Name: "api-server", UpgradeOrder: int32Ptr(2)}, // Second
				{Name: "monitoring", UpgradeOrder: int32Ptr(3)}, // Third
				{Name: "database", UpgradeOrder: nil},           // Last (safest)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a copy of input roles to verify the original slice is not modified
			originalRoles := make([]orchestrationv1alpha1.RoleSpec, len(tt.roles))
			copy(originalRoles, tt.roles)

			result := sortRolesByUpgradeOrder(tt.roles)
			t.Logf("result len %d", len(result))

			// Check if the result matches expected
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("sortRolesByUpgradeOrder() = %v, want %v", result, tt.expected)
			}

			// Verify the original slice was not modified
			if !reflect.DeepEqual(tt.roles, originalRoles) {
				t.Errorf("Original roles were modified: got %v, want %v", tt.roles, originalRoles)
			}
		})
	}
}

func TestInjectContainerEnvVars(t *testing.T) {
	// Setup test data
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-role-set",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
			Annotations: map[string]string{
				constants.RoleSetIndexAnnotationKey: "1",
			},
		},
	}

	role := &orchestrationv1alpha1.RoleSpec{
		Name: "test-role",
	}

	roleIndex := 0
	templateHash := "test-hash"

	// Create container with:
	// 1. Some user-defined env vars in reverse alphabetical order
	// 2. A env var with same name as built-in env var
	container := &corev1.Container{
		Name: "test-container",
		Env: []corev1.EnvVar{
			{Name: "USER_VAR_Z", Value: "value-z"},                       // Last in alphabetical order
			{Name: "USER_VAR_M", Value: "value-m"},                       // Middle in alphabetical order
			{Name: constants.RoleSetNameEnvKey, Value: "override-value"}, // Same name as built-in
			{Name: "USER_VAR_A", Value: "value-a"},                       // First in alphabetical order
		},
	}

	// Call the function under test
	injectContainerEnvVars(container, roleSet, role, &roleIndex, templateHash)

	// Verify the result
	// Check that built-in env vars are not overridden
	builtInEnvNames := []string{
		constants.StormServiceNameEnvKey,
		constants.RoleSetNameEnvKey,
		constants.RoleSetIndexEnvKey,
		constants.RoleNameEnvKey,
		constants.RoleTemplateHashEnvKey,
		constants.RoleReplicaIndexEnvKey,
	}

	assert.GreaterOrEqual(t, len(container.Env), len(builtInEnvNames), "Should have at least all built-in env vars")

	// Track built-in env vars found
	foundBuiltInEnvs := make(map[string]bool)

	// Check the order and values
	// Verify built-in env vars are present at the beginning
	for i, env := range container.Env {
		if i < len(builtInEnvNames) {
			// First N env vars should be built-in
			assert.Contains(t, builtInEnvNames, env.Name, "Built-in env var should be at the beginning")
			foundBuiltInEnvs[env.Name] = true

			// Check built-in env var values
			switch env.Name {
			case constants.StormServiceNameEnvKey:
				assert.Equal(t, "test-service", env.Value)
			case constants.RoleSetNameEnvKey:
				assert.Equal(t, "test-role-set", env.Value) // Should not be overridden
			case constants.RoleSetIndexEnvKey:
				assert.Equal(t, "1", env.Value)
			case constants.RoleNameEnvKey:
				assert.Equal(t, "test-role", env.Value)
			case constants.RoleTemplateHashEnvKey:
				assert.Equal(t, "test-hash", env.Value)
			case constants.RoleReplicaIndexEnvKey:
				assert.Equal(t, "0", env.Value)
			}
		} else {
			// User-defined env vars should come after built-in ones
			assert.NotContains(t, builtInEnvNames, env.Name, "User-defined env var should not be a built-in name")
		}
	}

	// 3. Check that all built-in env vars are present
	for _, envName := range builtInEnvNames {
		assert.True(t, foundBuiltInEnvs[envName], "Built-in env var %s should be present", envName)
	}

	// 4. Check that user-defined env vars maintain their original order
	// Find the start index of user-defined env vars
	userEnvStartIndex := len(builtInEnvNames)
	assert.Less(t, userEnvStartIndex, len(container.Env), "Should have user-defined env vars")

	// Check user-defined env vars order
	expectedUserEnvOrder := []string{"USER_VAR_Z", "USER_VAR_M", "USER_VAR_A"}
	for i, expectedName := range expectedUserEnvOrder {
		actualIndex := userEnvStartIndex + i
		assert.Less(t, actualIndex, len(container.Env), "Should have enough user-defined env vars")
		assert.Equal(t, expectedName, container.Env[actualIndex].Name, "User-defined env var should maintain original order")
	}
}
