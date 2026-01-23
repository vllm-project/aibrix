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
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const revisionName = "test-revision-1"

func TestRenderRoleSet(t *testing.T) {
	replicas := int32(2)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	reconciler := &StormServiceReconciler{}

	roleSet, err := reconciler.renderRoleSet(stormService, nil, revisionName, nil)
	assert.NoError(t, err)
	assert.NotNil(t, roleSet)
	assert.Equal(t, "default", roleSet.Namespace)
	assert.Contains(t, roleSet.GenerateName, "test-storm-roleset-")
	assert.Equal(t, "test-storm", roleSet.Labels[constants.StormServiceNameLabelKey])
	assert.Equal(t, revisionName, roleSet.Labels[constants.StormServiceRevisionLabelKey])
	assert.Equal(t, revisionName, roleSet.Annotations[constants.RoleSetRevisionAnnotationKey])
	assert.Len(t, roleSet.OwnerReferences, 1)
	assert.Equal(t, "test-role", roleSet.Spec.Roles[0].Name)
}

func TestRenderRoleSetWithIndex(t *testing.T) {
	replicas := int32(1)
	index := 5
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	reconciler := &StormServiceReconciler{}

	roleSet, err := reconciler.renderRoleSet(stormService, &index, revisionName, nil)
	assert.NoError(t, err)
	assert.NotNil(t, roleSet)
	assert.Equal(t, "5", roleSet.Annotations[constants.RoleSetIndexAnnotationKey])
}

func TestRenderRoleSetSelectorMismatch(t *testing.T) {
	replicas := int32(1)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "different",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	reconciler := &StormServiceReconciler{}

	_, err := reconciler.renderRoleSet(stormService, nil, revisionName, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "does not match stormService selector")
}

func TestCreateRoleSetNilTemplate(t *testing.T) {
	replicas := int32(1)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: nil,
			},
		},
	}

	reconciler := &StormServiceReconciler{}

	_, err := reconciler.createRoleSet(stormService, 1, "test-revision", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "bad stormService template: nil")
}

func TestGetRoleSetList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		selector    *metav1.LabelSelector
		setupFunc   func(*fake.ClientBuilder)
		expectError bool
		expectedLen int
	}{
		{
			name:        "nil selector",
			selector:    nil,
			expectError: true,
			expectedLen: 0,
		},
		{
			name: "invalid selector",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "app",
						Operator: "InvalidOperator",
						Values:   []string{"test"},
					},
				},
			},
			expectError: true,
			expectedLen: 0,
		},
		{
			name: "valid selector",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			setupFunc: func(builder *fake.ClientBuilder) {
				roleSet := &orchestrationv1alpha1.RoleSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-roleset",
						Namespace: "default",
						Labels: map[string]string{
							"app": "test",
						},
					},
				}
				builder.WithObjects(roleSet)
			},
			expectError: false,
			expectedLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)

			if tt.setupFunc != nil {
				tt.setupFunc(clientBuilder)
			}

			testClient := clientBuilder.Build()
			reconciler := &StormServiceReconciler{
				Client: testClient,
			}

			roleSets, err := reconciler.getRoleSetList(context.TODO(), tt.selector)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, roleSets)
				assert.Equal(t, tt.expectedLen, len(roleSets))
			}
		})
	}
}

func TestDeleteRoleSetSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &StormServiceReconciler{
		Client: client,
	}

	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-roleset",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "test-role"},
			},
		},
	}

	err := client.Create(context.TODO(), roleSet)
	assert.NoError(t, err)

	createdRoleSet := &orchestrationv1alpha1.RoleSet{}
	err = client.Get(context.TODO(), types.NamespacedName{
		Name:      roleSet.Name,
		Namespace: roleSet.Namespace,
	}, createdRoleSet)
	assert.NoError(t, err)

	deleted, err := reconciler.deleteRoleSet([]*orchestrationv1alpha1.RoleSet{roleSet})
	assert.NoError(t, err)
	assert.Equal(t, 1, deleted)

	err = client.Get(context.TODO(), types.NamespacedName{
		Name:      roleSet.Name,
		Namespace: roleSet.Namespace,
	}, createdRoleSet)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestDeleteRoleSetNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &StormServiceReconciler{
		Client: client,
	}

	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-roleset",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "test-role"},
			},
		},
	}

	deleted, err := reconciler.deleteRoleSet([]*orchestrationv1alpha1.RoleSet{roleSet})
	assert.NoError(t, err)
	assert.Equal(t, 1, deleted)
}

func TestUpdateRoleSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &StormServiceReconciler{
		Client: client,
	}

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "updated-role"},
					},
				},
			},
		},
	}

	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-roleset",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
			Annotations: map[string]string{
				constants.RoleSetIndexAnnotationKey: "0",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			Roles: []orchestrationv1alpha1.RoleSpec{
				{Name: "old-role"},
			},
		},
	}

	err := client.Create(context.TODO(), roleSet)
	assert.NoError(t, err)

	updated, err := reconciler.updateRoleSet(stormService, []*orchestrationv1alpha1.RoleSet{roleSet}, "new-revision", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, updated)

	updatedRoleSet := &orchestrationv1alpha1.RoleSet{}
	err = client.Get(context.TODO(), types.NamespacedName{
		Name:      roleSet.Name,
		Namespace: roleSet.Namespace,
	}, updatedRoleSet)
	assert.NoError(t, err)
	assert.Equal(t, "updated-role", updatedRoleSet.Spec.Roles[0].Name)
	assert.Equal(t, "new-revision", updatedRoleSet.Labels[constants.StormServiceRevisionLabelKey])
	assert.Equal(t, "0", updatedRoleSet.Annotations[constants.RoleSetIndexAnnotationKey])
}

func TestCreateRoleSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &StormServiceReconciler{
		Client: client,
	}

	replicas := int32(2)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test",
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "test-role"},
					},
				},
			},
		},
	}

	created, err := reconciler.createRoleSet(stormService, 2, "test-revision", nil)
	assert.NoError(t, err)
	assert.Equal(t, 2, created)

	roleSetList := &orchestrationv1alpha1.RoleSetList{}
	err = client.List(context.TODO(), roleSetList)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(roleSetList.Items))

	for _, roleSet := range roleSetList.Items {
		assert.Equal(t, "default", roleSet.Namespace)
		assert.Contains(t, roleSet.Name, "test-storm-roleset-")
		assert.Equal(t, "test-storm", roleSet.Labels[constants.StormServiceNameLabelKey])
		assert.Equal(t, "test-revision", roleSet.Labels[constants.StormServiceRevisionLabelKey])
		assert.Equal(t, "test-revision", roleSet.Annotations[constants.RoleSetRevisionAnnotationKey])
		assert.Equal(t, "test-role", roleSet.Spec.Roles[0].Name)
		assert.Equal(t, "test", roleSet.Labels["app"])
	}
}
