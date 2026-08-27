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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// Defaulting spec.mode would pin an inferred mode onto objects that never declared
// one, which then blocks a later scale-up through validateStormServiceMode.
func TestStormServiceDefault_LeavesModeUnset(t *testing.T) {
	defaulter := &StormServiceCustomDefaulter{}

	ss := &orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To[int32](1),
		},
	}

	require.NoError(t, defaulter.Default(context.Background(), ss))
	assert.Empty(t, ss.Spec.Mode)
}

func TestStormServiceValidateCreate_ModeReplicas(t *testing.T) {
	validator := &StormServiceCustomDefaulter{}

	tests := map[string]struct {
		replicas    *int32
		mode        orchestrationv1alpha1.StormServiceMode
		expectError bool
	}{
		"pooled with replicas > 1 is rejected":  {replicas: ptr.To[int32](3), mode: orchestrationv1alpha1.StormServicePooledMode, expectError: true},
		"pooled with replicas 1 is allowed":     {replicas: ptr.To[int32](1), mode: orchestrationv1alpha1.StormServicePooledMode, expectError: false},
		"pooled with replicas unset is allowed": {replicas: nil, mode: orchestrationv1alpha1.StormServicePooledMode, expectError: false},
		"replica with replicas > 1 is allowed":  {replicas: ptr.To[int32](3), mode: orchestrationv1alpha1.StormServiceReplicaMode, expectError: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ss := &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: tc.replicas,
					Mode:     tc.mode,
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{},
					},
				},
			}
			_, err := validator.ValidateCreate(context.Background(), ss)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestStormServiceValidateUpdate_ModeReplicas(t *testing.T) {
	validator := &StormServiceCustomDefaulter{}

	tests := map[string]struct {
		oldMode     orchestrationv1alpha1.StormServiceMode
		oldReplicas *int32
		newMode     orchestrationv1alpha1.StormServiceMode
		newReplicas *int32
		expectError bool
	}{
		"inferred mode scales up":               {oldReplicas: ptr.To[int32](1), newReplicas: ptr.To[int32](3), expectError: false},
		"declared replica mode scales up":       {oldMode: orchestrationv1alpha1.StormServiceReplicaMode, oldReplicas: ptr.To[int32](1), newMode: orchestrationv1alpha1.StormServiceReplicaMode, newReplicas: ptr.To[int32](3), expectError: false},
		"declared pooled mode rejects scale up": {oldMode: orchestrationv1alpha1.StormServicePooledMode, oldReplicas: ptr.To[int32](1), newMode: orchestrationv1alpha1.StormServicePooledMode, newReplicas: ptr.To[int32](3), expectError: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			oldSS := &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: tc.oldReplicas,
					Mode:     tc.oldMode,
				},
			}
			newSS := &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: tc.newReplicas,
					Mode:     tc.newMode,
				},
			}
			_, err := validator.ValidateUpdate(context.Background(), oldSS, newSS)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func volcanoStrategy(minMember int32, minTaskMember map[string]int32) *orchestrationv1alpha1.SchedulingStrategy {
	return &orchestrationv1alpha1.SchedulingStrategy{
		VolcanoSchedulingStrategy: &orchestrationv1alpha1.VolcanoSchedulingStrategySpec{
			MinMember:     minMember,
			MinTaskMember: minTaskMember,
		},
	}
}

func roleWithStrategy(name string, strategy *orchestrationv1alpha1.SchedulingStrategy) orchestrationv1alpha1.RoleSpec {
	return orchestrationv1alpha1.RoleSpec{Name: name, SchedulingStrategy: strategy}
}

func stormServiceWithTemplate(roleSetStrategy *orchestrationv1alpha1.SchedulingStrategy, roles ...orchestrationv1alpha1.RoleSpec) *orchestrationv1alpha1.StormService {
	ss := &orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles:              roles,
					SchedulingStrategy: roleSetStrategy,
				},
			},
		},
	}
	ss.Name = "test"
	return ss
}

func TestStormServiceValidateCreate_SchedulingStrategy(t *testing.T) {
	validator := &StormServiceCustomDefaulter{}
	prefill := roleWithStrategy("prefill", nil)
	decode := roleWithStrategy("decode", nil)
	const roleSetVolcanoPath = "spec.template.spec.schedulingStrategy.volcanoSchedulingStrategy"

	tests := map[string]struct {
		roleSetStrategy *orchestrationv1alpha1.SchedulingStrategy
		roles           []orchestrationv1alpha1.RoleSpec
		// wantErrs lists substrings that must all appear in the error; empty means the object is valid.
		wantErrs []string
	}{
		"no scheduling strategy": {
			roles: []orchestrationv1alpha1.RoleSpec{prefill, decode},
		},
		"valid volcano gang with minTaskMember": {
			roleSetStrategy: volcanoStrategy(6, map[string]int32{"prefill": 4, "decode": 2}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
		},
		"valid volcano gang without minTaskMember": {
			roleSetStrategy: volcanoStrategy(2, nil),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
		},
		"minMember above minTaskMember sum is allowed": {
			roleSetStrategy: volcanoStrategy(8, map[string]int32{"prefill": 4, "decode": 2}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
		},
		"minMember zero is rejected": {
			roleSetStrategy: volcanoStrategy(0, nil),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill},
			wantErrs:        []string{roleSetVolcanoPath + ".minMember", "must be greater than 0"},
		},
		"minMember negative is rejected": {
			roleSetStrategy: volcanoStrategy(-1, nil),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill},
			wantErrs:        []string{roleSetVolcanoPath + ".minMember", "must be greater than 0"},
		},
		"minTaskMember zero is rejected": {
			roleSetStrategy: volcanoStrategy(4, map[string]int32{"prefill": 0, "decode": 4}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
			wantErrs:        []string{roleSetVolcanoPath + ".minTaskMember[prefill]", "must be greater than 0"},
		},
		"minTaskMember negative is rejected": {
			roleSetStrategy: volcanoStrategy(4, map[string]int32{"prefill": -2}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill},
			wantErrs:        []string{roleSetVolcanoPath + ".minTaskMember[prefill]", "must be greater than 0"},
		},
		"minTaskMember key not matching a role is rejected": {
			roleSetStrategy: volcanoStrategy(4, map[string]int32{"worker": 4}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
			wantErrs:        []string{roleSetVolcanoPath + ".minTaskMember[worker]", "must match a role name", "decode, prefill"},
		},
		"minMember below minTaskMember sum is rejected": {
			roleSetStrategy: volcanoStrategy(4, map[string]int32{"prefill": 4, "decode": 2}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill, decode},
			wantErrs:        []string{roleSetVolcanoPath + ".minMember", "sum of minTaskMember values (6)"},
		},
		"roleset-level and role-level strategies together are rejected": {
			roleSetStrategy: volcanoStrategy(2, nil),
			roles:           []orchestrationv1alpha1.RoleSpec{roleWithStrategy("prefill", volcanoStrategy(1, nil))},
			wantErrs:        []string{"spec.template.spec.roles[0].schedulingStrategy", "Forbidden", "mutually exclusive"},
		},
		"roleset-level and role-level strategies together are rejected for non-volcano schedulers": {
			roleSetStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				GodelSchedulingStrategy: &orchestrationv1alpha1.GodelSchedulingStrategySpec{MinMember: 2},
			},
			roles: []orchestrationv1alpha1.RoleSpec{
				prefill,
				roleWithStrategy("decode", &orchestrationv1alpha1.SchedulingStrategy{
					CoschedulingSchedulingStrategy: &orchestrationv1alpha1.CoschedulingSchedulingStrategySpec{MinMember: 1},
				}),
			},
			wantErrs: []string{"spec.template.spec.roles[1].schedulingStrategy", "Forbidden"},
		},
		"role-level volcano keyed by its own role is allowed": {
			roles: []orchestrationv1alpha1.RoleSpec{prefill, roleWithStrategy("decode", volcanoStrategy(2, map[string]int32{"decode": 2}))},
		},
		"role-level volcano keyed by another role is rejected": {
			roles:    []orchestrationv1alpha1.RoleSpec{prefill, roleWithStrategy("decode", volcanoStrategy(2, map[string]int32{"prefill": 2}))},
			wantErrs: []string{"spec.template.spec.roles[1].schedulingStrategy.volcanoSchedulingStrategy.minTaskMember[prefill]", "valid names are: decode"},
		},
		"non-volcano strategies are not checked": {
			roleSetStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				GodelSchedulingStrategy: &orchestrationv1alpha1.GodelSchedulingStrategySpec{MinMember: 0},
			},
			roles: []orchestrationv1alpha1.RoleSpec{prefill},
		},
		"all errors are reported together": {
			roleSetStrategy: volcanoStrategy(0, map[string]int32{"worker": 0}),
			roles:           []orchestrationv1alpha1.RoleSpec{prefill},
			wantErrs: []string{
				roleSetVolcanoPath + ".minMember: Invalid value: 0",
				roleSetVolcanoPath + ".minTaskMember[worker]: Invalid value: 0",
				roleSetVolcanoPath + ".minTaskMember[worker]: Invalid value: \"worker\"",
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ss := stormServiceWithTemplate(tc.roleSetStrategy, tc.roles...)
			_, err := validator.ValidateCreate(context.Background(), ss)
			if len(tc.wantErrs) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), "expected an Invalid error, got %v", err)
			for _, want := range tc.wantErrs {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestValidateRoleSetSchedulingStrategy_NilSpec(t *testing.T) {
	assert.Empty(t, validateRoleSetSchedulingStrategy(nil, field.NewPath("spec")))
}

// Objects created before the scheduling validation existed must keep accepting updates that do
// not touch the RoleSet template, otherwise the controller's finalizer removal would be rejected
// and the object could never be deleted.
func TestStormServiceValidateUpdate_SchedulingStrategy(t *testing.T) {
	validator := &StormServiceCustomDefaulter{}
	prefill := roleWithStrategy("prefill", nil)
	invalid := func() *orchestrationv1alpha1.StormService {
		return stormServiceWithTemplate(volcanoStrategy(0, nil), prefill)
	}
	valid := func() *orchestrationv1alpha1.StormService {
		return stormServiceWithTemplate(volcanoStrategy(1, nil), prefill)
	}

	tests := map[string]struct {
		old         *orchestrationv1alpha1.StormService
		new         *orchestrationv1alpha1.StormService
		expectError bool
	}{
		"unchanged invalid template is allowed": {old: invalid(), new: invalid(), expectError: false},
		"metadata-only update on invalid template is allowed": {
			old: invalid(),
			new: func() *orchestrationv1alpha1.StormService {
				ss := invalid()
				ss.Finalizers = []string{"orchestration.aibrix.ai/stormservice-finalizer"}
				ss.Spec.Replicas = ptr.To[int32](3)
				return ss
			}(),
			expectError: false,
		},
		"template change introducing invalid config is rejected": {old: valid(), new: invalid(), expectError: true},
		"template change that keeps invalid config is rejected": {
			old: invalid(),
			new: func() *orchestrationv1alpha1.StormService {
				ss := invalid()
				ss.Spec.Template.Spec.Roles[0].Replicas = ptr.To[int32](2)
				return ss
			}(),
			expectError: true,
		},
		"template change fixing invalid config is allowed": {old: invalid(), new: valid(), expectError: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := validator.ValidateUpdate(context.Background(), tc.old, tc.new)
			if tc.expectError {
				require.Error(t, err)
				assert.True(t, apierrors.IsInvalid(err), "expected an Invalid error, got %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
