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
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestNextRevision(t *testing.T) {
	tests := []struct {
		name      string
		revisions []*apps.ControllerRevision
		expected  int64
	}{
		{
			name:      "empty revisions",
			revisions: []*apps.ControllerRevision{},
			expected:  1,
		},
		{
			name: "single revision",
			revisions: []*apps.ControllerRevision{
				{Revision: 5},
			},
			expected: 6,
		},
		{
			name: "multiple revisions",
			revisions: []*apps.ControllerRevision{
				{Revision: 1},
				{Revision: 3},
				{Revision: 7},
			},
			expected: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := nextRevision(tt.revisions)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetRevisionLabels(t *testing.T) {
	obj := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-storm",
		},
	}

	labels := getRevisionLabels(obj)
	expected := map[string]string{
		"name": "test-storm",
	}

	assert.Equal(t, expected, labels)
}

func TestGetPatch(t *testing.T) {
	replicas := int32(3)
	stormService := &orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	patch, err := getPatch(stormService)
	assert.NoError(t, err)
	assert.NotEmpty(t, patch)
	assert.Contains(t, string(patch), "template")
	assert.Contains(t, string(patch), "$patch")
}

func TestNewRevision(t *testing.T) {
	replicas := int32(2)
	collisionCount := int32(0)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	revision, err := newRevision(stormService, 1, &collisionCount)
	assert.NoError(t, err)
	assert.NotNil(t, revision)
	assert.Equal(t, int64(1), revision.Revision)
	assert.NotNil(t, revision.ObjectMeta.Annotations)
	assert.NotEmpty(t, revision.Data.Raw)
}

func TestApplyRevision(t *testing.T) {
	replicas := int32(3)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "original-role"},
					},
				},
			},
		},
	}

	modifiedReplicas := int32(5)
	modifiedStorm := stormService.DeepCopy()
	modifiedStorm.Spec.Replicas = &modifiedReplicas
	modifiedStorm.Spec.Template.Spec.Roles[0].Name = "modified-role"

	patch, err := getPatch(modifiedStorm)
	assert.NoError(t, err)

	revision := &apps.ControllerRevision{
		Data: runtime.RawExtension{Raw: patch},
	}

	restored, err := applyRevision(stormService, revision)
	assert.NoError(t, err)
	assert.NotNil(t, restored)
	assert.Equal(t, "modified-role", restored.Spec.Template.Spec.Roles[0].Name)
}
