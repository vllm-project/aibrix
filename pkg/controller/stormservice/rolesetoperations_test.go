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

// stormServiceFor builds a StormService with an explicit UID: metav1.IsControlledBy
// compares owner UIDs, so two objects left with the zero UID would look related.
func stormServiceFor(name, namespace string, selector map[string]string) *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(namespace + "/" + name),
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

// roleSetFor mirrors renderRoleSet: the controller stamps the storm-service-name
// label and a controller reference on every RoleSet it creates.
func roleSetFor(name string, owner *orchestrationv1alpha1.StormService, extraLabels map[string]string) *orchestrationv1alpha1.RoleSet {
	roleSet := roleSetWithoutOwner(name, owner, extraLabels)
	roleSet.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(owner, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.StormServiceKind)),
	}
	return roleSet
}

// roleSetWithoutOwner carries the name label but no controller reference, which is
// what a hand-made RoleSet or one that had its ownerReferences stripped looks like.
func roleSetWithoutOwner(name string, owner *orchestrationv1alpha1.StormService, extraLabels map[string]string) *orchestrationv1alpha1.RoleSet {
	labels := map[string]string{constants.StormServiceNameLabelKey: owner.Name}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: owner.Namespace,
			Labels:    labels,
		},
	}
}

func TestGetRoleSetList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	appLabels := map[string]string{"app": "test"}
	owner := stormServiceFor("test-storm", "default", appLabels)
	sameNamespacePeer := stormServiceFor("other-storm", "default", appLabels)
	otherNamespaceNamesake := stormServiceFor("test-storm", "other-ns", appLabels)

	noSelector := stormServiceFor("test-storm", "default", nil)
	noSelector.Spec.Selector = nil

	tests := []struct {
		name         string
		stormService *orchestrationv1alpha1.StormService
		roleSets     []*orchestrationv1alpha1.RoleSet
		expectedLen  int
	}{
		{
			name:         "owned roleset is returned",
			stormService: owner,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetFor("test-roleset", owner, appLabels)},
			expectedLen:  1,
		},
		{
			// RoleSets are namespaced and the name label is only unique within a
			// namespace, so a namesake next door owns its own RoleSets.
			name:         "roleset of a namesake in another namespace is excluded",
			stormService: otherNamespaceNamesake,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetFor("test-roleset", owner, appLabels)},
			expectedLen:  0,
		},
		{
			// Overlapping selectors within one namespace must not let one
			// StormService count, update or delete another's RoleSets.
			name:         "roleset owned by another stormservice is excluded",
			stormService: sameNamespacePeer,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetFor("test-roleset", owner, appLabels)},
			expectedLen:  0,
		},
		{
			// Without a controller reference nobody claimed the RoleSet, and acting
			// on it is how the previous label-only lookup adopted foreign objects.
			name:         "roleset without a controller reference is excluded",
			stormService: owner,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetWithoutOwner("test-roleset", owner, appLabels)},
			expectedLen:  0,
		},
		{
			// The lookup keys on the stamped name label, not on spec.selector, so an
			// owned RoleSet stays visible to scaling, status and finalize even after
			// its labels drift or the selector is edited. Listing by the selector
			// would drop it here and leak it on delete.
			name:         "owned roleset that no longer matches spec.selector is returned",
			stormService: owner,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetFor("test-roleset", owner, map[string]string{"app": "drifted"})},
			expectedLen:  1,
		},
		{
			// spec.selector is a pointer, and finalize turns any lookup error into a
			// retry, so depending on the selector here would hang deletion forever.
			name:         "nil selector still resolves owned rolesets",
			stormService: noSelector,
			roleSets:     []*orchestrationv1alpha1.RoleSet{roleSetFor("test-roleset", noSelector, nil)},
			expectedLen:  1,
		},
		{
			name:         "only owned rolesets are returned when several match",
			stormService: owner,
			roleSets: []*orchestrationv1alpha1.RoleSet{
				roleSetFor("owned-a", owner, appLabels),
				roleSetFor("owned-b", owner, appLabels),
				roleSetFor("foreign", sameNamespacePeer, appLabels),
				roleSetWithoutOwner("orphan", owner, appLabels),
			},
			expectedLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
			for _, rs := range tt.roleSets {
				clientBuilder = clientBuilder.WithObjects(rs)
			}

			reconciler := &StormServiceReconciler{Client: clientBuilder.Build()}

			roleSets, err := reconciler.getRoleSetList(context.TODO(), tt.stormService)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedLen, len(roleSets))
			for _, rs := range roleSets {
				assert.Equal(t, tt.stormService.Namespace, rs.Namespace)
				assert.True(t, metav1.IsControlledBy(rs, tt.stormService))
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
				constants.RoleSetIndexAnnotationKey:                  "0",
				constants.RoleSetHistoricalNodeBindingsAnnotationKey: `{"replicaSlots":{"worker/0":"node-a"}}`,
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
	assert.Equal(t, `{"replicaSlots":{"worker/0":"node-a"}}`, updatedRoleSet.Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey])
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
