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

package orchestration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestComputeHashRoleSetTemplate(t *testing.T) {
	t1 := &orchestrationv1alpha1.RoleSetTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"a": "b"},
	}}
	t2 := &orchestrationv1alpha1.RoleSetTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"a": "c"},
	}}
	h1 := ComputeHash(t1, nil)
	h2 := ComputeHash(t2, nil)
	assert.NotEqual(t, h1, h2)

	var c1, c2 int32 = 1, 2
	assert.NotEqual(t, ComputeHash(t1, &c1), ComputeHash(t1, &c2))
}

func TestValidateControllerRef(t *testing.T) {
	ref := &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}
	assert.NoError(t, ValidateControllerRef(ref))

	table := []struct {
		name string
		ref  *metav1.OwnerReference
		err  string
	}{
		{"nil ref", nil, "controllerRef is nil"},
		{"empty version", &metav1.OwnerReference{Kind: "A", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}, "empty APIVersion"},
		{"empty kind", &metav1.OwnerReference{APIVersion: "v1", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}, "empty Kind"},
		{"controller false", &metav1.OwnerReference{APIVersion: "v1", Kind: "X", Controller: boolPtr(false), BlockOwnerDeletion: boolPtr(true)}, "not set to true"},
		{"blockOwnerDeletion false", &metav1.OwnerReference{APIVersion: "v1", Kind: "X", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(false)}, "not set"},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateControllerRef(tc.ref)
			assert.ErrorContains(t, err, tc.err)
		})
	}
}

func TestIsRoleSetReady(t *testing.T) {
	rs := &orchestrationv1alpha1.RoleSet{
		Status: orchestrationv1alpha1.RoleSetStatus{
			Conditions: []orchestrationv1alpha1.Condition{
				{Type: orchestrationv1alpha1.RoleSetReady, Status: corev1.ConditionTrue},
			},
		},
	}
	assert.True(t, IsRoleSetReady(rs))
	rs.Status.Conditions[0].Status = corev1.ConditionFalse
	assert.False(t, IsRoleSetReady(rs))
}

func TestConditionHelpers(t *testing.T) {
	cond := NewCondition("Available", corev1.ConditionTrue, "Ready", "all good")
	assert.Equal(t, "Available", string(cond.Type))
	conds := []orchestrationv1alpha1.Condition{*cond}
	found := GetCondition(conds, "Available")
	assert.NotNil(t, found)
	assert.Equal(t, "Ready", found.Reason)

	filtered := FilterOutCondition(conds, "Available")
	assert.Len(t, filtered, 0)
}

func TestDeepCopyMap(t *testing.T) {
	m := map[string]string{"a": "1"}
	copy := DeepCopyMap(m)
	assert.Equal(t, m, copy)
	copy["a"] = "2"
	assert.NotEqual(t, m["a"], copy["a"])
}

func TestSlowStartBatch_AllSuccess(t *testing.T) {
	count := 5
	successFn := func(i int) error {
		return nil
	}
	succeeded, err := SlowStartBatch(count, 1, successFn)
	assert.NoError(t, err)
	assert.Equal(t, count, succeeded)
}

func TestSlowStartBatch_WithFailure(t *testing.T) {
	count := 5
	failFn := func(i int) error {
		if i == 1 {
			return errors.New("fail")
		}
		return nil
	}
	succeeded, err := SlowStartBatch(count, 1, failFn)
	assert.Error(t, err)
	assert.Less(t, succeeded, count)
}

func TestMinIntHelpers(t *testing.T) {
	assert.Equal(t, int32(3), MinInt32(3, 4))
	assert.Equal(t, int32(-1), MinInt32(-1, 5))
	assert.Equal(t, 2, MinInt(2, 3))
	assert.Equal(t, -5, MinInt(-5, 0))
}

func boolPtr(b bool) *bool { return &b }

type MockPodGroup struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

func (m *MockPodGroup) GetObjectKind() schema.ObjectKind {
	return &m.TypeMeta
}

func (m *MockPodGroup) DeepCopyObject() runtime.Object {
	return &MockPodGroup{
		TypeMeta:   m.TypeMeta,
		ObjectMeta: *m.ObjectMeta.DeepCopy(),
	}
}

func TestEnsurePodGroupExist(t *testing.T) {
	ctx := context.Background()
	name := "test-podgroup"
	namespace := "test-namespace"
	crdName := "podgroups.scheduling.volcano.sh"

	podGroup := &MockPodGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "scheduling.volcano.sh/v1beta1",
			Kind:       "PodGroup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	tests := []struct {
		name           string
		setupMocks     func(*MockDynamicInterface, *MockNamespaceableResourceInterface, *MockResourceInterface)
		expectedResult bool
		expectedError  bool
	}{
		{
			name: "CRD not installed",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewNotFound(schema.GroupResource{}, ""))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)
			},
			expectedResult: false,
			expectedError:  false,
		},
		{
			name: "CRD check returns other error",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewInternalError(fmt.Errorf("internal error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)
			},
			expectedResult: false,
			expectedError:  true,
		},
		{
			name: "PodGroup not found and create successfully",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				// CRD exist
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				// PodGroup not exist
				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewNotFound(schema.GroupResource{}, ""))

				// Create success
				pgResource.On("Create", ctx, mock.AnythingOfType("*unstructured.Unstructured"), metav1.CreateOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name: "PodGroup creation fails",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				// CRD exist
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				// PodGroup not exists
				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewNotFound(schema.GroupResource{}, ""))

				pgResource.On("Create", ctx, mock.AnythingOfType("*unstructured.Unstructured"), metav1.CreateOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewInternalError(fmt.Errorf("create error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedResult: false,
			expectedError:  true,
		},
		{
			name: "PodGroup get returns other error",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				// CRD exist
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewInternalError(fmt.Errorf("get error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedResult: false,
			expectedError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDC := new(MockDynamicInterface)
			mockCrdResource := new(MockNamespaceableResourceInterface)
			mockPgResource := new(MockResourceInterface)

			tt.setupMocks(mockDC, mockCrdResource, mockPgResource)

			created, err := EnsurePodGroupExist(ctx, mockDC, podGroup, name, namespace)

			// check result
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectedResult, created)

			mockDC.AssertExpectations(t)
			mockCrdResource.AssertExpectations(t)
			mockPgResource.AssertExpectations(t)
		})
	}
}

func TestFinalizePodGroup(t *testing.T) {
	ctx := context.Background()
	name := "test-podgroup"
	namespace := "test-namespace"
	crdName := "podgroups.scheduling.volcano.sh"

	podGroup := &MockPodGroup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "scheduling.volcano.sh/v1beta1",
			Kind:       "PodGroup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	tests := []struct {
		name          string
		setupMocks    func(*MockDynamicInterface, *MockNamespaceableResourceInterface, *MockResourceInterface)
		expectedError bool
	}{
		{
			name: "CRD not installed",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewNotFound(schema.GroupResource{}, ""))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)
			},
			expectedError: false,
		},
		{
			name: "CRD check returns other error",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewInternalError(fmt.Errorf("internal error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)
			},
			expectedError: true,
		},
		{
			name: "PodGroup not found",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {
				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewNotFound(schema.GroupResource{}, ""))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedError: false,
		},
		{
			name: "PodGroup exists and delete successfully",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {

				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Delete", ctx, name, metav1.DeleteOptions{}).
					Return(nil)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedError: false,
		},
		{
			name: "PodGroup delete fails",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {

				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Delete", ctx, name, metav1.DeleteOptions{}).
					Return(apierrors.NewInternalError(fmt.Errorf("delete error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedError: true,
		},
		{
			name: "PodGroup get returns other error",
			setupMocks: func(dc *MockDynamicInterface, crdResource *MockNamespaceableResourceInterface, pgResource *MockResourceInterface) {

				crdResource.On("Get", ctx, crdName, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, nil)

				pgResource.On("Get", ctx, name, metav1.GetOptions{}).
					Return(&unstructured.Unstructured{}, apierrors.NewInternalError(fmt.Errorf("get error")))

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				}).Return(crdResource)

				dc.On("Resource", schema.GroupVersionResource{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				}).Return(crdResource)

				crdResource.On("Namespace", namespace).Return(pgResource)
			},
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDC := new(MockDynamicInterface)
			mockCrdResource := new(MockNamespaceableResourceInterface)
			mockPgResource := new(MockResourceInterface)
			mockClient := client.Client(nil)

			tt.setupMocks(mockDC, mockCrdResource, mockPgResource)

			err := FinalizePodGroup(ctx, mockDC, mockClient, podGroup, name, namespace)

			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			mockDC.AssertExpectations(t)
			mockCrdResource.AssertExpectations(t)
			mockPgResource.AssertExpectations(t)
		})
	}
}
