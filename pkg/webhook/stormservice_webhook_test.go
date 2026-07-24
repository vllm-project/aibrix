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
