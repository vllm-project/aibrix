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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

type MockClient struct {
	mock.Mock
	client.Client
	failOnCreate bool
	failOnUpdate bool
	failOnGet    bool
	failOnList   bool
	failOnDelete bool
}

func (m *MockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if m.failOnCreate {
		return assert.AnError
	}
	args := m.Called(ctx, obj, opts)
	return args.Error(0)
}

func (m *MockClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if m.failOnUpdate {
		return assert.AnError
	}
	args := m.Called(ctx, obj, opts)
	return args.Error(0)
}

func (m *MockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if m.failOnGet {
		return assert.AnError
	}
	args := m.Called(ctx, key, obj, opts)
	return args.Error(0)
}

func (m *MockClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if m.failOnList {
		return assert.AnError
	}
	args := m.Called(ctx, list, opts)
	return args.Error(0)
}

func (m *MockClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if m.failOnDelete {
		return assert.AnError
	}
	args := m.Called(ctx, obj, opts)
	return args.Error(0)
}

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

func TestTruncateHistory(t *testing.T) {
	tests := []struct {
		name         string
		stormService *orchestrationv1alpha1.StormService
		revisions    []*appsv1.ControllerRevision
		current      *appsv1.ControllerRevision
		update       *appsv1.ControllerRevision
		mockSetup    func(mockClient *MockClient)
		wantErr      bool
	}{
		{
			name: "no history to truncate",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
				},
			},
			revisions: []*appsv1.ControllerRevision{},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "history within limit",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
				},
			},
			revisions: []*appsv1.ControllerRevision{
				{ObjectMeta: metav1.ObjectMeta{Name: "rev1"}},
			},
			current: &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "current"}},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					list := args.Get(1).(*orchestrationv1alpha1.RoleSetList)
					list.Items = []orchestrationv1alpha1.RoleSet{
						{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid1")}},
					}
				}).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "history exceeds limit",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					RevisionHistoryLimit: func() *int32 { i := int32(1); return &i }(),
				},
			},
			revisions: []*appsv1.ControllerRevision{
				{ObjectMeta: metav1.ObjectMeta{Name: "rev1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev3"}},
			},
			current: &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "current"}},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					list := args.Get(1).(*orchestrationv1alpha1.RoleSetList)
					list.Items = []orchestrationv1alpha1.RoleSet{
						{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid1")}},
					}
				}).Return(nil)
				m.On("Delete", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "error getting role sets",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
				},
			},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError)
			},
			wantErr: true,
		},
		{
			name: "error deleting revisions",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					RevisionHistoryLimit: func() *int32 { i := int32(1); return &i }(),
				},
			},
			revisions: []*appsv1.ControllerRevision{
				{ObjectMeta: metav1.ObjectMeta{Name: "rev1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev3"}},
			},
			current: &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "current"}},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					list := args.Get(1).(*orchestrationv1alpha1.RoleSetList)
					list.Items = []orchestrationv1alpha1.RoleSet{
						{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid1")}},
					}
				}).Return(nil)
				m.On("Delete", mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError)
			},
			wantErr: true,
		},
		{
			name: "history exceeds limit with update revision",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					RevisionHistoryLimit: func() *int32 { i := int32(1); return &i }(),
				},
			},
			revisions: []*appsv1.ControllerRevision{
				{ObjectMeta: metav1.ObjectMeta{Name: "rev1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rev3"}},
			},
			current: &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "current"}},
			update:  &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "update-rev"}},
			mockSetup: func(m *MockClient) {
				m.On("List", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					list := args.Get(1).(*orchestrationv1alpha1.RoleSetList)
					list.Items = []orchestrationv1alpha1.RoleSet{
						{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid1")}},
					}
				}).Return(nil)
				m.On("Delete", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockClient{}
			if tt.mockSetup != nil {
				tt.mockSetup(mockClient)
			}

			r := &StormServiceReconciler{
				Client: mockClient,
			}

			err := r.truncateHistory(context.Background(), tt.stormService, tt.revisions, tt.current, tt.update)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

func TestSyncRevision(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apps.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	tests := []struct {
		name           string
		stormService   *orchestrationv1alpha1.StormService
		revisions      []*apps.ControllerRevision
		wantCurrentRev *apps.ControllerRevision
		wantUpdateRev  *apps.ControllerRevision
		wantCollision  int32
		wantErr        bool
	}{
		{
			name: "no existing revisions, create new revision",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
			},
			revisions: []*apps.ControllerRevision{},
			wantCurrentRev: &apps.ControllerRevision{
				Revision: 1,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantUpdateRev: &apps.ControllerRevision{
				Revision: 1,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantCollision: 0,
			wantErr:       false,
		},
		{
			name: "with existing revision matching current",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
				Status: orchestrationv1alpha1.StormServiceStatus{
					CurrentRevision: "test-rev-1",
				},
			},
			revisions: []*apps.ControllerRevision{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-rev-1",
						OwnerReferences: []metav1.OwnerReference{
							{
								Name: "test",
								UID:  "test-uid",
							},
						},
					},
					Revision: 1,
				},
			},
			wantCurrentRev: &apps.ControllerRevision{
				Revision: 1,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantUpdateRev: &apps.ControllerRevision{
				Revision: 2,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantCollision: 0,
			wantErr:       false,
		},
		{
			name: "with collision count",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "test-uid"},
				Status: orchestrationv1alpha1.StormServiceStatus{
					CollisionCount: func() *int32 { i := int32(1); return &i }(),
				},
			},
			revisions: []*apps.ControllerRevision{},
			wantCurrentRev: &apps.ControllerRevision{
				Revision: 1,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantUpdateRev: &apps.ControllerRevision{
				Revision: 1,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "test",
							UID:  "test-uid",
						},
					},
				},
			},
			wantCollision: 1,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &StormServiceReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
				Scheme: scheme,
			}

			currentRev, updateRev, collisionCount, err := r.syncRevision(context.Background(), tt.stormService, tt.revisions)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			if tt.wantCurrentRev == nil {
				assert.Nil(t, currentRev)
			} else {
				assert.Equal(t, tt.wantCurrentRev.Revision, currentRev.Revision)
				assert.Equal(t, 1, len(currentRev.OwnerReferences))
				assert.Equal(t, tt.wantCurrentRev.OwnerReferences[0].Name, currentRev.OwnerReferences[0].Name)
				assert.Equal(t, tt.wantCurrentRev.OwnerReferences[0].UID, currentRev.OwnerReferences[0].UID)
			}

			if tt.wantUpdateRev == nil {
				assert.Nil(t, updateRev)
			} else {
				assert.Equal(t, tt.wantUpdateRev.Revision, updateRev.Revision)
				assert.Equal(t, 1, len(updateRev.OwnerReferences))
				assert.Equal(t, tt.wantUpdateRev.OwnerReferences[0].Name, updateRev.OwnerReferences[0].Name)
				assert.Equal(t, tt.wantUpdateRev.OwnerReferences[0].UID, updateRev.OwnerReferences[0].UID)
			}

			assert.Equal(t, tt.wantCollision, collisionCount)
		})
	}
}

func TestGetControllerRevision(t *testing.T) {
	tests := []struct {
		name        string
		setupMock   func(*MockClient, metav1.Object)
		obj         metav1.Object
		expected    []*appsv1.ControllerRevision
		expectedErr string
	}{
		{
			name: "successful retrieval with matching owner references",
			obj: &metav1.ObjectMeta{
				Namespace: "test-ns",
				Name:      "test-name",
				UID:       "test-uid",
			},
			setupMock: func(m *MockClient, obj metav1.Object) {
				revisionList := &appsv1.ControllerRevisionList{
					Items: []appsv1.ControllerRevision{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "rev1",
								Namespace: obj.GetNamespace(),
								OwnerReferences: []metav1.OwnerReference{
									{
										UID: obj.GetUID(),
									},
								},
							},
						},
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "rev2",
								Namespace: obj.GetNamespace(),
								OwnerReferences: []metav1.OwnerReference{
									{
										UID: "other-uid",
									},
								},
							},
						},
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "rev3",
								Namespace: obj.GetNamespace(),
								OwnerReferences: []metav1.OwnerReference{
									{
										UID: obj.GetUID(),
									},
								},
							},
						},
					},
				}
				m.On("List", mock.Anything, mock.AnythingOfType("*v1.ControllerRevisionList"), mock.Anything).
					Run(func(args mock.Arguments) {
						list := args.Get(1).(*appsv1.ControllerRevisionList)
						list.Items = revisionList.Items
					}).
					Return(nil)
			},
			expected: []*appsv1.ControllerRevision{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rev1",
						Namespace: "test-ns",
						OwnerReferences: []metav1.OwnerReference{
							{
								UID: "test-uid",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rev3",
						Namespace: "test-ns",
						OwnerReferences: []metav1.OwnerReference{
							{
								UID: "test-uid",
							},
						},
					},
				},
			},
		},
		{
			name: "no matching owner references",
			obj: &metav1.ObjectMeta{
				Namespace: "test-ns",
				Name:      "test-name",
				UID:       "test-uid",
			},
			setupMock: func(m *MockClient, obj metav1.Object) {
				revisionList := &appsv1.ControllerRevisionList{
					Items: []appsv1.ControllerRevision{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "rev1",
								Namespace: obj.GetNamespace(),
								OwnerReferences: []metav1.OwnerReference{
									{
										UID: "other-uid-1",
									},
								},
							},
						},
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "rev2",
								Namespace: obj.GetNamespace(),
								OwnerReferences: []metav1.OwnerReference{
									{
										UID: "other-uid-2",
									},
								},
							},
						},
					},
				}
				m.On("List", mock.Anything, mock.AnythingOfType("*v1.ControllerRevisionList"), mock.Anything).
					Run(func(args mock.Arguments) {
						list := args.Get(1).(*appsv1.ControllerRevisionList)
						list.Items = revisionList.Items
					}).
					Return(nil)
			},
			expected: []*appsv1.ControllerRevision{},
		},
		{
			name: "empty revision list",
			obj: &metav1.ObjectMeta{
				Namespace: "test-ns",
				Name:      "test-name",
				UID:       "test-uid",
			},
			setupMock: func(m *MockClient, obj metav1.Object) {
				m.On("List", mock.Anything, mock.AnythingOfType("*v1.ControllerRevisionList"), mock.Anything).
					Return(nil)
			},
			expected: []*appsv1.ControllerRevision{},
		},
		{
			name: "client list error",
			obj: &metav1.ObjectMeta{
				Namespace: "test-ns",
				Name:      "test-name",
				UID:       "test-uid",
			},
			setupMock: func(m *MockClient, obj metav1.Object) {
				m.On("List", mock.Anything, mock.AnythingOfType("*v1.ControllerRevisionList"), mock.Anything).
					Return(errors.New("list error"))
			},
			expectedErr: "list controller revision failed, list error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockClient{}
			tt.setupMock(mockClient, tt.obj)

			r := &StormServiceReconciler{
				Client: mockClient,
			}

			result, err := r.getControllerRevision(context.Background(), tt.obj)

			if tt.expectedErr != "" {
				assert.ErrorContains(t, err, tt.expectedErr)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, len(tt.expected), len(result))

			for i := range tt.expected {
				assert.Equal(t, tt.expected[i].Name, result[i].Name)
				assert.Equal(t, tt.expected[i].Namespace, result[i].Namespace)
				assert.Equal(t, tt.expected[i].OwnerReferences[0].UID, result[i].OwnerReferences[0].UID)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

func TestCreateControllerRevision(t *testing.T) {
	// Setup common test data
	name := "test-parent"
	namespace := "test-ns"
	qualifiedResource := schema.GroupResource{
		Group:    "apps",
		Resource: "controllerrevisions",
	}

	parent := &metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
	}
	revision := &apps.ControllerRevision{
		Data: runtime.RawExtension{Raw: []byte("test-data")},
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					Name: name,
				},
			},
		},
	}

	t.Run("successful creation on first attempt", func(t *testing.T) {
		collisionCount := int32(0)
		mockClient := &MockClient{}
		r := &StormServiceReconciler{Client: mockClient}

		// Expect Create to be called once and succeed
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).Return(nil)

		result, err := r.createControllerRevision(context.Background(), parent, revision, &collisionCount)
		require.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, name, result.OwnerReferences[0].Name)
		assert.Equal(t, namespace, result.Namespace)
		assert.Equal(t, int32(0), collisionCount) // Should not be incremented
		mockClient.AssertExpectations(t)
	})

	t.Run("returns error when collisionCount is nil", func(t *testing.T) {
		r := &StormServiceReconciler{}
		_, err := r.createControllerRevision(context.Background(), parent, revision, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "collisionCount should not be nil")
	})

	t.Run("handles creation error (non-already-exists)", func(t *testing.T) {
		collisionCount := int32(0)
		mockClient := &MockClient{}
		r := &StormServiceReconciler{Client: mockClient}

		expectedErr := fmt.Errorf("some creation error")
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).Return(expectedErr)

		_, err := r.createControllerRevision(context.Background(), parent, revision, &collisionCount)
		require.Error(t, err)
		assert.Equal(t, expectedErr, err)
		assert.Equal(t, int32(0), collisionCount) // Should not be incremented
		mockClient.AssertExpectations(t)
	})

	t.Run("handles collision with identical revision", func(t *testing.T) {
		collisionCount := int32(0)
		mockClient := &MockClient{}
		r := &StormServiceReconciler{Client: mockClient}

		// First Create fails with AlreadyExists
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).
			Return(apierrors.NewAlreadyExists(qualifiedResource, "alreadyExist")).Once()

		// Setup the existing object that will be returned by Get
		existing := &apps.ControllerRevision{
			Data: runtime.RawExtension{Raw: []byte("test-data")},
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						Name: name,
					},
				},
			},
		}
		mockClient.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				obj := args.Get(2).(*apps.ControllerRevision)
				*obj = *existing
			}).
			Return(nil)

		result, err := r.createControllerRevision(context.Background(), parent, revision, &collisionCount)
		require.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, existing, result)
		assert.Equal(t, int32(0), collisionCount) // Should not be incremented
		mockClient.AssertExpectations(t)
	})

	t.Run("handles collision with different revision (retries)", func(t *testing.T) {
		collisionCount := int32(0)
		mockClient := &MockClient{}
		r := &StormServiceReconciler{Client: mockClient}

		// First Create fails with AlreadyExists
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).
			Return(apierrors.NewAlreadyExists(qualifiedResource, "alreadyExist")).Once()

		// Setup the existing object that will be returned by Get (with different data)
		existing := &apps.ControllerRevision{
			Data: runtime.RawExtension{Raw: []byte("different-data")},
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						Name: name,
					},
				},
			},
		}
		mockClient.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				obj := args.Get(2).(*apps.ControllerRevision)
				*obj = *existing
			}).
			Return(nil)

		// Second Create succeeds
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).
			Return(nil).Once()

		result, err := r.createControllerRevision(context.Background(), parent, revision, &collisionCount)
		require.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, name, result.OwnerReferences[0].Name)
		assert.Equal(t, namespace, result.Namespace)
		assert.Equal(t, int32(1), collisionCount) // Should be incremented
		mockClient.AssertExpectations(t)
	})

	t.Run("handles error when getting existing revision", func(t *testing.T) {
		collisionCount := int32(0)
		mockClient := &MockClient{}
		r := &StormServiceReconciler{Client: mockClient}

		// First Create fails with AlreadyExists
		mockClient.On("Create", mock.Anything, mock.Anything, mock.Anything).
			Return(apierrors.NewAlreadyExists(qualifiedResource, "alreadyExist")).Once()

		// Get fails
		expectedErr := fmt.Errorf("get error")
		mockClient.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(expectedErr)

		_, err := r.createControllerRevision(context.Background(), parent, revision, &collisionCount)
		require.Error(t, err)
		assert.Equal(t, expectedErr, err)
		assert.Equal(t, int32(0), collisionCount) // Should not be incremented
		mockClient.AssertExpectations(t)
	})
}
