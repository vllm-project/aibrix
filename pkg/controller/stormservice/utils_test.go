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

package stormservice

import (
	"testing"

	"github.com/stretchr/testify/assert"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
)

func newStormService(replicas int32, surge, unavail string) orchestrationv1alpha1.StormService {
	s := intstrutil.FromString(surge)
	u := intstrutil.FromString(unavail)
	return orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type:           orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
				MaxSurge:       &s,
				MaxUnavailable: &u,
			},
		},
	}
}

// Helper function to create a RoleSet with specified properties
func createRoleSet(name, revision string, ready bool) *orchestrationv1alpha1.RoleSet {
	rs := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.StormServiceRevisionLabelKey: revision,
			},
		},
	}

	if ready {
		rs.Status = orchestrationv1alpha1.RoleSetStatus{
			Conditions: orchestrationv1alpha1.Conditions{
				{
					Type:   "Ready",
					Status: corev1.ConditionTrue,
				},
			},
		}
	} else {
		rs.Status = orchestrationv1alpha1.RoleSetStatus{
			Conditions: orchestrationv1alpha1.Conditions{
				{
					Type:   "Ready",
					Status: corev1.ConditionFalse,
				},
			},
		}
	}

	return rs
}

func TestMaxSurge_MaxUnavailable_MinAvailable_StormService(t *testing.T) {
	ss := newStormService(10, "25%", "20%") // surge=2, unavail=2
	assert.Equal(t, int32(3), MaxSurge(&ss))
	assert.Equal(t, int32(2), MaxUnavailable(ss))
	assert.Equal(t, int32(8), MinAvailable(&ss))
}

func TestSetAndRemoveStormServiceCondition(t *testing.T) {
	now := metav1.Now()
	status := &orchestrationv1alpha1.StormServiceStatus{}
	cond := orchestrationv1alpha1.Condition{
		Type:               orchestrationv1alpha1.StormServiceReady,
		Status:             corev1.ConditionFalse,
		Reason:             "Initializing",
		LastTransitionTime: &now,
	}

	SetStormServiceCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// identical updates ignored
	SetStormServiceCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	RemoveStormServiceCondition(status, orchestrationv1alpha1.StormServiceReady)
	assert.Empty(t, status.Conditions)
}

func TestIsRollingUpdate(t *testing.T) {
	replicas := int32(1)
	ss := orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{Replicas: &replicas},
	}
	assert.True(t, IsRollingUpdate(&ss)) // default empty string treated as rolling

	ss.Spec.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType
	assert.False(t, IsRollingUpdate(&ss))
}

func TestSortRoleSetByRevision(t *testing.T) {
	tests := []struct {
		name            string
		roleSets        []*orchestrationv1alpha1.RoleSet
		updatedRevision string
		expectedOrder   []string // Expected order of RoleSet names after sorting
	}{
		{
			name: "single matching revision",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				createRoleSet("rs1", "rev1", true),
			},
			updatedRevision: "rev1",
			expectedOrder:   []string{"rs1"},
		},
		{
			name: "old revision should come first",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				createRoleSet("rs1", "rev1", true),
				createRoleSet("rs2", "rev2", true),
			},
			updatedRevision: "rev2",
			expectedOrder:   []string{"rs1", "rs2"},
		},
		{
			name: "unready RoleSet should come before ready ones for same revision",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				createRoleSet("rs1", "rev2", false),
				createRoleSet("rs2", "rev2", true),
			},
			updatedRevision: "rev2",
			expectedOrder:   []string{"rs1", "rs2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy of the original slice to avoid modifying the test case
			roleSetsCopy := make([]*orchestrationv1alpha1.RoleSet, len(tt.roleSets))
			copy(roleSetsCopy, tt.roleSets)

			sortRoleSetByRevision(roleSetsCopy, tt.updatedRevision)

			// Extract names in the sorted order
			actualOrder := make([]string, len(roleSetsCopy))
			for i, rs := range roleSetsCopy {
				actualOrder[i] = rs.Name
			}

			assert.Equal(t, tt.expectedOrder, actualOrder, "RoleSets should be sorted correctly")
		})
	}
}

func TestFilterTerminatingRoleSets(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name            string
		input           []*orchestrationv1alpha1.RoleSet
		wantActive      int
		wantTerminating int
	}{
		{
			name:            "empty input",
			input:           []*orchestrationv1alpha1.RoleSet{},
			wantActive:      0,
			wantTerminating: 0,
		},
		{
			name: "all active RoleSets",
			input: []*orchestrationv1alpha1.RoleSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "active-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "active-2"}},
			},
			wantActive:      2,
			wantTerminating: 0,
		},
		{
			name: "all terminating RoleSets",
			input: []*orchestrationv1alpha1.RoleSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "terminating-1", DeletionTimestamp: &now}},
				{ObjectMeta: metav1.ObjectMeta{Name: "terminating-2", DeletionTimestamp: &now}},
			},
			wantActive:      0,
			wantTerminating: 2,
		},
		{
			name: "mixed active and terminating RoleSets",
			input: []*orchestrationv1alpha1.RoleSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "active-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "terminating-1", DeletionTimestamp: &now}},
				{ObjectMeta: metav1.ObjectMeta{Name: "active-2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "terminating-2", DeletionTimestamp: &now}},
			},
			wantActive:      2,
			wantTerminating: 2,
		},
		{
			name: "nil DeletionTimestamp should be considered active",
			input: []*orchestrationv1alpha1.RoleSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "active-1", DeletionTimestamp: nil}},
			},
			wantActive:      1,
			wantTerminating: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, terminating := filterTerminatingRoleSets(tt.input)

			assert.Len(t, active, tt.wantActive, "active RoleSets count mismatch")
			assert.Len(t, terminating, tt.wantTerminating, "terminating RoleSets count mismatch")

			// Verify that all active RoleSets have nil DeletionTimestamp
			for _, rs := range active {
				assert.Nil(t, rs.DeletionTimestamp, "active RoleSet should have nil DeletionTimestamp")
			}

			// Verify that all terminating RoleSets have non-nil DeletionTimestamp
			for _, rs := range terminating {
				assert.NotNil(t, rs.DeletionTimestamp, "terminating RoleSet should have non-nil DeletionTimestamp")
			}

			// Verify that the combined output contains all input items
			assert.Equal(t, len(tt.input), len(active)+len(terminating), "total output count should match input count")
		})
	}
}

func TestResolveFenceposts(t *testing.T) {
	tests := []struct {
		name                string
		maxSurge            *intstrutil.IntOrString
		maxUnavailable      *intstrutil.IntOrString
		desired             int32
		expectedSurge       int32
		expectedUnavailable int32
		expectError         bool
	}{
		{
			name:                "nil inputs with desired 10",
			maxSurge:            nil,
			maxUnavailable:      nil,
			desired:             10,
			expectedSurge:       0,
			expectedUnavailable: 1, // defaults to 1 when both are 0
			expectError:         false,
		},
		{
			name:                "int values",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 3},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 2},
			desired:             10,
			expectedSurge:       3,
			expectedUnavailable: 2,
			expectError:         false,
		},
		{
			name:                "percentage values",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "50%"},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "30%"},
			desired:             10,
			expectedSurge:       5, // 50% of 10
			expectedUnavailable: 3, // 30% of 10
			expectError:         false,
		},
		{
			name:                "mixed int and percentage",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 2},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "25%"},
			desired:             8,
			expectedSurge:       2,
			expectedUnavailable: 2, // 25% of 8
			expectError:         false,
		},
		// TODO: revisit this case later.
		{
			name:                "zero desired",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 1},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 1},
			desired:             0,
			expectedSurge:       1,
			expectedUnavailable: 1,
			expectError:         false,
		},
		{
			name:           "invalid percentage format",
			maxSurge:       &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "50"},
			maxUnavailable: &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 1},
			desired:        10,
			expectError:    true,
		},
		{
			name:                "both zero with desired > 0",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 0},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 0},
			desired:             5,
			expectedSurge:       0,
			expectedUnavailable: 1, // defaults to 1 when both are 0
			expectError:         false,
		},
		{
			name:                "rounding up for surge",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "33%"},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 1},
			desired:             10,
			expectedSurge:       4, // 33% of 10 rounds up to 4
			expectedUnavailable: 1,
			expectError:         false,
		},
		{
			name:                "rounding down for unavailable",
			maxSurge:            &intstrutil.IntOrString{Type: intstrutil.Int, IntVal: 1},
			maxUnavailable:      &intstrutil.IntOrString{Type: intstrutil.String, StrVal: "33%"},
			desired:             10,
			expectedSurge:       1,
			expectedUnavailable: 3, // 33% of 10 rounds down to 3
			expectError:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			surge, unavailable, err := ResolveFenceposts(tt.maxSurge, tt.maxUnavailable, tt.desired)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedSurge, surge)
			assert.Equal(t, tt.expectedUnavailable, unavailable)
		})
	}
}

func TestGetRoleSetRevision(t *testing.T) {
	tests := []struct {
		name     string
		roleSet  *orchestrationv1alpha1.RoleSet
		expected string
	}{
		{
			name: "Label exists with value",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "rev123",
					},
				},
			},
			expected: "rev123",
		},
		{
			name: "Label exists but empty",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "",
					},
				},
			},
			expected: "",
		},
		{
			name: "Label doesn't exist",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			expected: "",
		},
		{
			name: "Nil labels map",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: nil,
				},
			},
			expected: "",
		},
		{
			name: "Nil RoleSet metadata",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRoleSetRevision(tt.roleSet)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsRoleSetMatchRevision(t *testing.T) {
	tests := []struct {
		name     string
		roleSet  *orchestrationv1alpha1.RoleSet
		revision string
		expected bool
	}{
		{
			name: "empty revision should match roleSet with empty annotations",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: nil,
				},
			},
			revision: "",
			expected: true,
		},
		{
			name: "empty revision should not match roleSet with non-empty revision",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "test-revision",
					},
				},
			},
			revision: "",
			expected: false,
		},
		{
			name: "should match when revisions are equal",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "test-revision",
					},
				},
			},
			revision: "test-revision",
			expected: true,
		},
		{
			name: "should not match when revisions are different",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "test-revision",
					},
				},
			},
			revision: "different-revision",
			expected: false,
		},
		{
			name: "should match case-sensitive revisions",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "Test-Revision",
					},
				},
			},
			revision: "Test-Revision",
			expected: true,
		},
		{
			name: "should not match when case differs",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "test-revision",
					},
				},
			},
			revision: "Test-Revision",
			expected: false,
		},
		{
			name: "should work with other annotations present",
			roleSet: &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.StormServiceRevisionLabelKey: "test-revision",
						"another-annotation":                   "value",
					},
				},
			},
			revision: "test-revision",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isRoleSetMatchRevision(tt.roleSet, tt.revision)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsAllRoleUpdated(t *testing.T) {
	tests := []struct {
		name     string
		roleSet  *orchestrationv1alpha1.RoleSet
		expected bool
	}{
		{
			name: "All roles updated",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
						{Name: "role2", Replicas: int32Ptr(2)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", Replicas: 3, UpdatedReplicas: 3},
						{Name: "role2", Replicas: 2, UpdatedReplicas: 2},
					},
				},
			},
			expected: true,
		},
		{
			name: "Some roles not updated",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
						{Name: "role2", Replicas: int32Ptr(2)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", Replicas: 3, UpdatedReplicas: 3},
						{Name: "role2", Replicas: 2, UpdatedReplicas: 1},
					},
				},
			},
			expected: false,
		},
		{
			name: "No roles in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", Replicas: 3, UpdatedReplicas: 3},
					},
				},
			},
			expected: true,
		},
		{
			name: "No roles in status",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{},
				},
			},
			expected: true,
		},
		{
			name: "Nil replicas in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: nil},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", Replicas: 0, UpdatedReplicas: 0},
					},
				},
			},
			expected: true,
		},
		{
			name: "Mismatched role names",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role2", Replicas: 3, UpdatedReplicas: 3},
					},
				},
			},
			expected: true,
		},
		{
			name: "Status role not found in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:     "role1",
							Replicas: int32Ptr(3),
						},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{
							Name:            "role1",
							Replicas:        3,
							UpdatedReplicas: 3,
						},
						{
							Name:            "unknown-role",
							Replicas:        2,
							UpdatedReplicas: 2,
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Empty RoleSet",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec:   orchestrationv1alpha1.RoleSetSpec{},
				Status: orchestrationv1alpha1.RoleSetStatus{},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := isAllRoleUpdated(tt.roleSet)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestIsAllRoleUpdatedAndReady(t *testing.T) {
	tests := []struct {
		name     string
		roleSet  *orchestrationv1alpha1.RoleSet
		expected bool
	}{
		{
			name: "All roles updated and ready",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
						{Name: "role2", Replicas: int32Ptr(2)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", UpdatedReadyReplicas: 3},
						{Name: "role2", UpdatedReadyReplicas: 2},
					},
				},
			},
			expected: true,
		},
		{
			name: "One role not fully updated",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
						{Name: "role2", Replicas: int32Ptr(2)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", UpdatedReadyReplicas: 3},
						{Name: "role2", UpdatedReadyReplicas: 1},
					},
				},
			},
			expected: false,
		},
		// TODO: revisit this case.
		//{
		//	name: "Status missing for one role",
		//	roleSet: &orchestrationv1alpha1.RoleSet{
		//		Spec: orchestrationv1alpha1.RoleSetSpec{
		//			Roles: []orchestrationv1alpha1.RoleSpec{
		//				{Name: "role1", Replicas: int32Ptr(3)},
		//				{Name: "role2", Replicas: int32Ptr(2)},
		//			},
		//		},
		//		Status: orchestrationv1alpha1.RoleSetStatus{
		//			Roles: []orchestrationv1alpha1.RoleStatus{
		//				{Name: "role1", UpdatedReadyReplicas: 3},
		//			},
		//		},
		//	},
		//	expected: false,
		//},
		{
			name: "Extra status role not in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: int32Ptr(3)},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", UpdatedReadyReplicas: 3},
						{Name: "role2", UpdatedReadyReplicas: 2},
					},
				},
			},
			expected: true,
		},
		{
			name: "No roles in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", UpdatedReadyReplicas: 3},
					},
				},
			},
			expected: true,
		},
		{
			name: "Nil replicas in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "role1", Replicas: nil},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "role1", UpdatedReadyReplicas: 0},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllRoleUpdatedAndReady(tt.roleSet)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

// makeRoleSetWithRoles creates a RoleSet with the given revision and roles
func makeRoleSetWithRoles(revision string, roles ...orchestrationv1alpha1.RoleStatus) *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				constants.StormServiceRevisionLabelKey: revision,
			},
		},
		Status: orchestrationv1alpha1.RoleSetStatus{
			Roles: roles,
		},
	}
}

func TestFilterRoleSetByRevision(t *testing.T) {
	tests := []struct {
		name      string
		roleSets  []*orchestrationv1alpha1.RoleSet
		revision  string
		wantMatch []*orchestrationv1alpha1.RoleSet
		wantNot   []*orchestrationv1alpha1.RoleSet
	}{
		{
			name:      "empty input",
			roleSets:  []*orchestrationv1alpha1.RoleSet{},
			revision:  "rev1",
			wantMatch: []*orchestrationv1alpha1.RoleSet{},
			wantNot:   []*orchestrationv1alpha1.RoleSet{},
		},
		{
			name: "all match",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
			},
			revision: "rev1",
			wantMatch: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
			},
			wantNot: []*orchestrationv1alpha1.RoleSet{},
		},
		{
			name: "none match",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev2"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev3"},
					},
				},
			},
			revision:  "rev1",
			wantMatch: []*orchestrationv1alpha1.RoleSet{},
			wantNot: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev2"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev3"},
					},
				},
			},
		},
		{
			name: "mixed match",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev2"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
			},
			revision: "rev1",
			wantMatch: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev1"},
					},
				},
			},
			wantNot: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{constants.StormServiceRevisionLabelKey: "rev2"},
					},
				},
			},
		},
		{
			name: "no revision label",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"other": "label"},
					},
				},
			},
			revision:  "rev1",
			wantMatch: []*orchestrationv1alpha1.RoleSet{},
			wantNot: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"other": "label"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMatch, gotNot := filterRoleSetByRevision(tt.roleSets, tt.revision)
			assert.Equal(t, tt.wantMatch, gotMatch)
			assert.Equal(t, tt.wantNot, gotNot)
		})
	}
}

func TestFilterReadyRoleSets(t *testing.T) {
	tests := []struct {
		name     string
		input    []*orchestrationv1alpha1.RoleSet
		expected struct {
			ready    int
			notReady int
		}
	}{
		{
			name: "all ready role sets",
			input: []*orchestrationv1alpha1.RoleSet{
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expected: struct {
				ready    int
				notReady int
			}{ready: 2, notReady: 0},
		},
		{
			name: "all not ready role sets",
			input: []*orchestrationv1alpha1.RoleSet{
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
			},
			expected: struct {
				ready    int
				notReady int
			}{ready: 0, notReady: 2},
		},
		{
			name: "mixed ready and not ready role sets",
			input: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-1"},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "not-ready-1"},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-2"},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expected: struct {
				ready    int
				notReady int
			}{ready: 2, notReady: 1},
		},
		{
			name:  "empty input",
			input: []*orchestrationv1alpha1.RoleSet{},
			expected: struct {
				ready    int
				notReady int
			}{ready: 0, notReady: 0},
		},
		{
			name: "nil input counts as notReady",
			input: []*orchestrationv1alpha1.RoleSet{
				nil,
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReady,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expected: struct {
				ready    int
				notReady int
			}{ready: 1, notReady: 1},
		},
		{
			name: "type is not RoleSetReady but condition True",
			input: []*orchestrationv1alpha1.RoleSet{
				{
					Status: orchestrationv1alpha1.RoleSetStatus{
						Conditions: orchestrationv1alpha1.Conditions{
							{
								Type:   orchestrationv1alpha1.RoleSetReplicaFailure,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expected: struct {
				ready    int
				notReady int
			}{ready: 0, notReady: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, notReady := filterReadyRoleSets(tt.input)
			assert.Equal(t, tt.expected.ready, len(ready))
			assert.Equal(t, tt.expected.notReady, len(notReady))

			// Verify the actual ready status of returned role sets
			for _, rs := range ready {
				assert.True(t, utils.IsRoleSetReady(rs))
			}
			for _, rs := range notReady {
				assert.False(t, utils.IsRoleSetReady(rs))
			}
		})
	}
}

// TestComputeRoleRevisions tests the core per-role revision tracking logic
func TestComputeRoleRevisions(t *testing.T) {
	// Helper to create ControllerRevision
	makeControllerRevision := func(name string, revision int64) *apps.ControllerRevision {
		return &apps.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Revision: revision,
		}
	}

	// Helper to create StormService with roles
	makeStormServiceWithRoles := func(name string, roles []orchestrationv1alpha1.RoleSpec) *orchestrationv1alpha1.StormService {
		return &orchestrationv1alpha1.StormService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: orchestrationv1alpha1.StormServiceSpec{
				Template: orchestrationv1alpha1.RoleSetTemplateSpec{
					Spec: &orchestrationv1alpha1.RoleSetSpec{
						Roles: roles,
					},
				},
			},
		}
	}

	tests := []struct {
		name      string
		current   *orchestrationv1alpha1.StormService
		update    *orchestrationv1alpha1.StormService
		currentCR *apps.ControllerRevision
		updateCR  *apps.ControllerRevision
		validate  func(t *testing.T, result map[string]*apps.ControllerRevision)
	}{
		{
			name: "role template changed - should use updateCR",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "old-image:v1"},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "new-image:v2"}, // Changed!
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Contains(t, result, "prefill")
				assert.Equal(t, int64(2), result["prefill"].Revision, "Changed role should use updateCR")
				assert.Equal(t, "test-ss-rev2", result["prefill"].Name)
			},
		},
		{
			name: "role template unchanged - should use currentCR",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "same-image:v1"},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "same-image:v1"}, // Unchanged
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Contains(t, result, "decode")
				assert.Equal(t, int64(1), result["decode"].Revision, "Unchanged role should keep currentCR")
				assert.Equal(t, "test-ss-rev1", result["decode"].Name)
			},
		},
		{
			name:    "new role added - should use updateCR",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "new-role",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "new-image:v1"},
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Contains(t, result, "new-role")
				assert.Equal(t, int64(2), result["new-role"].Revision, "New role should use updateCR")
			},
		},
		{
			name: "multiple roles with mixed changes",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "prefill:v1"},
							},
						},
					},
				},
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "decode:v1"},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "prefill:v2"}, // Changed!
							},
						},
					},
				},
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "decode:v1"}, // Unchanged
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 2)
				// prefill changed → revision 2
				assert.Equal(t, int64(2), result["prefill"].Revision, "Changed prefill should use updateCR")
				// decode unchanged → revision 1
				assert.Equal(t, int64(1), result["decode"].Revision, "Unchanged decode should keep currentCR")
			},
		},
		{
			name:    "nil current StormService - all roles use updateCR",
			current: nil,
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "role1",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "image:v1"},
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Equal(t, int64(2), result["role1"].Revision, "Should use updateCR when current is nil")
			},
		},
		{
			name:      "empty roles in update - should return empty map",
			current:   makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{{Name: "role1"}}),
			update:    makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 0, "Empty update roles should return empty map")
			},
		},
		{
			name:    "nil Spec.Template.Spec in current - should not panic",
			current: &orchestrationv1alpha1.StormService{Spec: orchestrationv1alpha1.StormServiceSpec{Template: orchestrationv1alpha1.RoleSetTemplateSpec{}}},
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "role1",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "image:v1"},
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev1", 1),
			updateCR:  makeControllerRevision("test-ss-rev2", 2),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Equal(t, int64(2), result["role1"].Revision, "Should treat nil Spec as new role")
			},
		},
		{
			name: "three roles - one new, one changed, one unchanged",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "prefill:v1"},
							},
						},
					},
				},
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "decode:v1"},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "prefill",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "prefill:v2"}, // Changed
							},
						},
					},
				},
				{
					Name: "decode",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "decode:v1"}, // Unchanged
							},
						},
					},
				},
				{
					Name: "cache",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "cache:v1"}, // New
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev5", 5),
			updateCR:  makeControllerRevision("test-ss-rev6", 6),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 3)
				assert.Equal(t, int64(6), result["prefill"].Revision, "Changed prefill → rev 6")
				assert.Equal(t, int64(5), result["decode"].Revision, "Unchanged decode → rev 5")
				assert.Equal(t, int64(6), result["cache"].Revision, "New cache → rev 6")
			},
		},
		{
			name: "complex pod template changes - env vars modified",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "worker",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "worker:v1",
									Env: []corev1.EnvVar{
										{Name: "KEY1", Value: "value1"},
									},
								},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "worker",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "worker:v1",
									Env: []corev1.EnvVar{
										{Name: "KEY1", Value: "value1"},
										{Name: "KEY2", Value: "value2"}, // Added env var
									},
								},
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev3", 3),
			updateCR:  makeControllerRevision("test-ss-rev4", 4),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Equal(t, int64(4), result["worker"].Revision, "Template with env change should use updateCR")
			},
		},
		{
			name: "role with identical deep copy template - should use currentCR",
			current: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "api",
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "api", "version": "v1"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "api-server",
									Image: "api:v1.0.0",
									Ports: []corev1.ContainerPort{
										{Name: "http", ContainerPort: 8080},
									},
								},
							},
						},
					},
				},
			}),
			update: makeStormServiceWithRoles("test-ss", []orchestrationv1alpha1.RoleSpec{
				{
					Name: "api",
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "api", "version": "v1"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "api-server",
									Image: "api:v1.0.0",
									Ports: []corev1.ContainerPort{
										{Name: "http", ContainerPort: 8080},
									},
								},
							},
						},
					},
				},
			}),
			currentCR: makeControllerRevision("test-ss-rev10", 10),
			updateCR:  makeControllerRevision("test-ss-rev11", 11),
			validate: func(t *testing.T, result map[string]*apps.ControllerRevision) {
				assert.Len(t, result, 1)
				assert.Equal(t, int64(10), result["api"].Revision, "Identical template should keep currentCR")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeRoleRevisions(tt.current, tt.update, tt.currentCR, tt.updateCR)
			tt.validate(t, result)
		})
	}
}

func TestAggregateRoleStatuses(t *testing.T) {
	tests := []struct {
		name           string
		roleSets       []*orchestrationv1alpha1.RoleSet
		updateRevision string
		expected       []orchestrationv1alpha1.RoleStatus
	}{
		{
			name:           "empty RoleSet list",
			roleSets:       []*orchestrationv1alpha1.RoleSet{},
			updateRevision: "rev1",
			expected:       []orchestrationv1alpha1.RoleStatus{},
		},
		{
			name: "single RoleSet with one role - replica mode",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "prefill",
								Replicas:             5,
								ReadyReplicas:        3,
								NotReadyReplicas:     2,
								UpdatedReplicas:      4,
								UpdatedReadyReplicas: 3,
							},
						},
					},
				},
			},
			updateRevision: "rev1",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "prefill",
					Replicas:             5,
					ReadyReplicas:        3,
					NotReadyReplicas:     2,
					UpdatedReplicas:      5,
					UpdatedReadyReplicas: 3,
				},
			},
		},
		{
			name: "single RoleSet with multiple roles - pool mode",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "prefill",
								Replicas:             5,
								ReadyReplicas:        4,
								NotReadyReplicas:     1,
								UpdatedReplicas:      5,
								UpdatedReadyReplicas: 4,
							},
							{
								Name:                 "decode",
								Replicas:             10,
								ReadyReplicas:        10,
								NotReadyReplicas:     0,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 10,
							},
						},
					},
				},
			},
			updateRevision: "rev1",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "decode",
					Replicas:             10,
					ReadyReplicas:        10,
					NotReadyReplicas:     0,
					UpdatedReplicas:      10,
					UpdatedReadyReplicas: 10,
				},
				{
					Name:                 "prefill",
					Replicas:             5,
					ReadyReplicas:        4,
					NotReadyReplicas:     1,
					UpdatedReplicas:      5,
					UpdatedReadyReplicas: 4,
				},
			},
		},
		{
			name: "multiple RoleSets with same role - replica mode aggregation",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "worker",
								Replicas:             10,
								ReadyReplicas:        8,
								NotReadyReplicas:     2,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 8,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "worker",
								Replicas:             10,
								ReadyReplicas:        10,
								NotReadyReplicas:     0,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 10,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "worker",
								Replicas:             10,
								ReadyReplicas:        5,
								NotReadyReplicas:     5,
								UpdatedReplicas:      8,
								UpdatedReadyReplicas: 5,
							},
						},
					},
				},
			},
			updateRevision: "rev1",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "worker",
					Replicas:             30,
					ReadyReplicas:        23,
					NotReadyReplicas:     7,
					UpdatedReplicas:      30,
					UpdatedReadyReplicas: 23,
				},
			},
		},
		{
			name: "multiple RoleSets with multiple roles - pool mode aggregation",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "prefill",
								Replicas:             5,
								ReadyReplicas:        4,
								NotReadyReplicas:     1,
								UpdatedReplicas:      5,
								UpdatedReadyReplicas: 4,
							},
							{
								Name:                 "decode",
								Replicas:             10,
								ReadyReplicas:        10,
								NotReadyReplicas:     0,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 10,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "prefill",
								Replicas:             5,
								ReadyReplicas:        5,
								NotReadyReplicas:     0,
								UpdatedReplicas:      5,
								UpdatedReadyReplicas: 5,
							},
							{
								Name:                 "decode",
								Replicas:             10,
								ReadyReplicas:        8,
								NotReadyReplicas:     2,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 8,
							},
						},
					},
				},
			},
			updateRevision: "rev1",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "decode",
					Replicas:             20,
					ReadyReplicas:        18,
					NotReadyReplicas:     2,
					UpdatedReplicas:      20,
					UpdatedReadyReplicas: 18,
				},
				{
					Name:                 "prefill",
					Replicas:             10,
					ReadyReplicas:        9,
					NotReadyReplicas:     1,
					UpdatedReplicas:      10,
					UpdatedReadyReplicas: 9,
				},
			},
		},
		{
			name: "RoleSet with no roles in status",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{},
					},
				},
			},
			updateRevision: "rev1",
			expected:       []orchestrationv1alpha1.RoleStatus{},
		},
		{
			name: "mixed RoleSets - some with status, some without",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "worker",
								Replicas:             10,
								ReadyReplicas:        10,
								NotReadyReplicas:     0,
								UpdatedReplicas:      10,
								UpdatedReadyReplicas: 10,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.StormServiceRevisionLabelKey: "rev1",
						},
					},
					Status: orchestrationv1alpha1.RoleSetStatus{
						Roles: []orchestrationv1alpha1.RoleStatus{
							{
								Name:                 "worker",
								Replicas:             5,
								ReadyReplicas:        3,
								NotReadyReplicas:     2,
								UpdatedReplicas:      5,
								UpdatedReadyReplicas: 3,
							},
						},
					},
				},
			},
			updateRevision: "rev1",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "worker",
					Replicas:             15,
					ReadyReplicas:        13,
					NotReadyReplicas:     2,
					UpdatedReplicas:      15,
					UpdatedReadyReplicas: 13,
				},
			},
		},
		{
			name: "rollout scenario - replica mode with mixed revisions (old ready, new not ready)",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				// Old revision RoleSets - all ready
				makeRoleSetWithRoles("rev1",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0}),
				makeRoleSetWithRoles("rev1",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0}),
				makeRoleSetWithRoles("rev1",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 1, NotReadyReplicas: 0}),
				// New revision RoleSets - not ready
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1}),
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1}),
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 1, ReadyReplicas: 0, NotReadyReplicas: 1}),
			},
			updateRevision: "rev2",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "decode",
					Replicas:             6,
					ReadyReplicas:        3,
					NotReadyReplicas:     3,
					UpdatedReplicas:      3,
					UpdatedReadyReplicas: 0,
				},
				{
					Name:                 "prefill",
					Replicas:             6,
					ReadyReplicas:        3,
					NotReadyReplicas:     3,
					UpdatedReplicas:      3,
					UpdatedReadyReplicas: 0,
				},
			},
		},
		{
			name: "rollout scenario - replica mode with mixed revisions (partial ready)",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				makeRoleSetWithRoles("rev1",
					orchestrationv1alpha1.RoleStatus{Name: "worker", Replicas: 10, ReadyReplicas: 10, NotReadyReplicas: 0}),
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "worker", Replicas: 10, ReadyReplicas: 7, NotReadyReplicas: 3}),
			},
			updateRevision: "rev2",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "worker",
					Replicas:             20,
					ReadyReplicas:        17,
					NotReadyReplicas:     3,
					UpdatedReplicas:      10,
					UpdatedReadyReplicas: 7,
				},
			},
		},
		{
			name: "rollout complete - all rolesets at new revision",
			roleSets: []*orchestrationv1alpha1.RoleSet{
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 2, ReadyReplicas: 2, NotReadyReplicas: 0},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 4, ReadyReplicas: 4, NotReadyReplicas: 0}),
				makeRoleSetWithRoles("rev2",
					orchestrationv1alpha1.RoleStatus{Name: "prefill", Replicas: 2, ReadyReplicas: 2, NotReadyReplicas: 0},
					orchestrationv1alpha1.RoleStatus{Name: "decode", Replicas: 4, ReadyReplicas: 4, NotReadyReplicas: 0}),
			},
			updateRevision: "rev2",
			expected: []orchestrationv1alpha1.RoleStatus{
				{
					Name:                 "decode",
					Replicas:             8,
					ReadyReplicas:        8,
					NotReadyReplicas:     0,
					UpdatedReplicas:      8,
					UpdatedReadyReplicas: 8,
				},
				{
					Name:                 "prefill",
					Replicas:             4,
					ReadyReplicas:        4,
					NotReadyReplicas:     0,
					UpdatedReplicas:      4,
					UpdatedReadyReplicas: 4,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := aggregateRoleStatuses(tt.roleSets, tt.updateRevision)
			assert.Equal(t, tt.expected, result)
		})
	}
}
