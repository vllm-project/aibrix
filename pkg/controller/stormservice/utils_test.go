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
