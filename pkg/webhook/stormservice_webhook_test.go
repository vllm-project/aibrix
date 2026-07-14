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

func TestStormServiceDefault_ResolvesMode(t *testing.T) {
	defaulter := &StormServiceCustomDefaulter{}

	tests := map[string]struct {
		replicas *int32
		mode     orchestrationv1alpha1.StormServiceMode
		want     orchestrationv1alpha1.StormServiceMode
	}{
		"unset replicas defaults to pooled": {replicas: nil, want: orchestrationv1alpha1.StormServicePooledMode},
		"replicas 1 defaults to pooled":     {replicas: ptr.To[int32](1), want: orchestrationv1alpha1.StormServicePooledMode},
		"replicas 3 defaults to replica":    {replicas: ptr.To[int32](3), want: orchestrationv1alpha1.StormServiceReplicaMode},
		"explicit pooled is preserved":      {replicas: ptr.To[int32](1), mode: orchestrationv1alpha1.StormServicePooledMode, want: orchestrationv1alpha1.StormServicePooledMode},
		"explicit replica is preserved":     {replicas: ptr.To[int32](1), mode: orchestrationv1alpha1.StormServiceReplicaMode, want: orchestrationv1alpha1.StormServiceReplicaMode},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ss := &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: tc.replicas,
					Mode:     tc.mode,
				},
			}
			require.NoError(t, defaulter.Default(context.Background(), ss))
			assert.Equal(t, tc.want, ss.Spec.Mode)
		})
	}
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
