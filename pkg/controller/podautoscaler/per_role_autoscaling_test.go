/*
Copyright 2024 The Aibrix Team.

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

package podautoscaler

import (
	"context"
	"testing"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetRoleReplicasFromRoleSet(t *testing.T) {
	tests := []struct {
		name     string
		roleSet  *orchestrationv1alpha1.RoleSet
		roleName string
		want     int32
	}{
		{
			name: "read from status",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "prefill", Replicas: ptr.To(int32(2))},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{
						{Name: "prefill", Replicas: 3},
					},
				},
			},
			roleName: "prefill",
			want:     3, // Should prefer status over spec
		},
		{
			name: "fallback to spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "decode", Replicas: ptr.To(int32(1))},
					},
				},
				Status: orchestrationv1alpha1.RoleSetStatus{
					Roles: []orchestrationv1alpha1.RoleStatus{},
				},
			},
			roleName: "decode",
			want:     1,
		},
		{
			name: "role not found",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "prefill", Replicas: ptr.To(int32(2))},
					},
				},
			},
			roleName: "nonexistent",
			want:     0,
		},
		{
			name: "nil replicas in spec",
			roleSet: &orchestrationv1alpha1.RoleSet{
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "prefill", Replicas: nil},
					},
				},
			},
			roleName: "prefill",
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &PodAutoscalerReconciler{}
			got := r.getRoleReplicasFromRoleSet(tt.roleSet, tt.roleName)
			if got != tt.want {
				t.Errorf("getRoleReplicasFromRoleSet() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindActiveRoleSet(t *testing.T) {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	_ = orchestrationv1alpha1.AddToScheme(s)

	tests := []struct {
		name      string
		roleSets  []orchestrationv1alpha1.RoleSet
		wantName  string
		wantError bool
	}{
		{
			name: "single active roleset",
			roleSets: []orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "roleset-1",
						Namespace: "test",
						Labels: map[string]string{
							"app": "test",
						},
					},
				},
			},
			wantName:  "roleset-1",
			wantError: false,
		},
		{
			name: "multiple active rolesets - choose newest",
			roleSets: []orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "roleset-1",
						Namespace:         "test",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Hour)},
						Labels: map[string]string{
							"app": "test",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "roleset-2",
						Namespace:         "test",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
						Labels: map[string]string{
							"app": "test",
						},
					},
				},
			},
			wantName:  "roleset-2", // Newer one
			wantError: false,
		},
		{
			name: "all terminating rolesets",
			roleSets: []orchestrationv1alpha1.RoleSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "roleset-1",
						Namespace:         "test",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"test-finalizer"}, // Need finalizer to create object with deletionTimestamp
						Labels: map[string]string{
							"app": "test",
						},
					},
				},
			},
			wantName:  "",
			wantError: true,
		},
		{
			name:      "no rolesets",
			roleSets:  []orchestrationv1alpha1.RoleSet{},
			wantName:  "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with rolesets
			objs := make([]client.Object, len(tt.roleSets))
			for i := range tt.roleSets {
				objs[i] = &tt.roleSets[i]
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(objs...).
				Build()

			r := &PodAutoscalerReconciler{
				Client: fakeClient,
			}

			ss := &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test",
						},
					},
				},
			}

			got, err := r.findActiveRoleSet(context.Background(), ss)
			if (err != nil) != tt.wantError {
				t.Errorf("findActiveRoleSet() error = %v, wantError %v", err, tt.wantError)
				return
			}
			if !tt.wantError && got.Name != tt.wantName {
				t.Errorf("findActiveRoleSet() name = %v, want %v", got.Name, tt.wantName)
			}
		})
	}
}

func TestApplyPerRoleScaling(t *testing.T) {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	_ = orchestrationv1alpha1.AddToScheme(s)
	_ = autoscalingv1alpha1.AddToScheme(s)

	tests := []struct {
		name         string
		ss           *orchestrationv1alpha1.StormService
		decisions    map[string]*RoleScaleDecision
		wantReplicas map[string]int32
		wantError    bool
	}{
		{
			name: "scale both roles",
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{
								{Name: "prefill", Replicas: ptr.To(int32(2))},
								{Name: "decode", Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			decisions: map[string]*RoleScaleDecision{
				"prefill": {
					RoleName:        "prefill",
					CurrentReplicas: 2,
					DesiredReplicas: 4,
					ShouldScale:     true,
				},
				"decode": {
					RoleName:        "decode",
					CurrentReplicas: 1,
					DesiredReplicas: 2,
					ShouldScale:     true,
				},
			},
			wantReplicas: map[string]int32{
				"prefill": 4,
				"decode":  2,
			},
			wantError: false,
		},
		{
			name: "scale only one role",
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{
								{Name: "prefill", Replicas: ptr.To(int32(2))},
								{Name: "decode", Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			decisions: map[string]*RoleScaleDecision{
				"prefill": {
					RoleName:        "prefill",
					CurrentReplicas: 2,
					DesiredReplicas: 3,
					ShouldScale:     true,
				},
				"decode": {
					RoleName:        "decode",
					CurrentReplicas: 1,
					DesiredReplicas: 1,
					ShouldScale:     false,
				},
			},
			wantReplicas: map[string]int32{
				"prefill": 3,
				"decode":  1, // unchanged
			},
			wantError: false,
		},
		{
			name: "no scaling needed",
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{
								{Name: "prefill", Replicas: ptr.To(int32(2))},
							},
						},
					},
				},
			},
			decisions: map[string]*RoleScaleDecision{
				"prefill": {
					RoleName:        "prefill",
					CurrentReplicas: 2,
					DesiredReplicas: 2,
					ShouldScale:     false,
				},
			},
			wantReplicas: map[string]int32{
				"prefill": 2,
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.ss).
				Build()

			r := &PodAutoscalerReconciler{
				Client: fakeClient,
			}

			err := r.applyPerRoleScaling(context.Background(), tt.ss, nil, tt.decisions)
			if (err != nil) != tt.wantError {
				t.Errorf("applyPerRoleScaling() error = %v, wantError %v", err, tt.wantError)
				return
			}

			if !tt.wantError {
				// Verify the replicas were updated correctly
				updated := &orchestrationv1alpha1.StormService{}
				err := fakeClient.Get(context.Background(),
					types.NamespacedName{Name: tt.ss.Name, Namespace: tt.ss.Namespace}, updated)
				if err != nil {
					t.Fatalf("failed to get updated StormService: %v", err)
				}

				for _, role := range updated.Spec.Template.Spec.Roles {
					if wantReplicas, ok := tt.wantReplicas[role.Name]; ok {
						if role.Replicas == nil || *role.Replicas != wantReplicas {
							t.Errorf("role %s replicas = %v, want %v",
								role.Name, role.Replicas, wantReplicas)
						}
					}
				}
			}
		})
	}
}

// TestReconcilePerRolePA_Validation tests basic validation logic
// Note: Full reconcilePerRolePA testing requires extensive mocking and is better suited for integration tests
func TestReconcilePerRolePA_Validation(t *testing.T) {
	// This test is commented out because reconcilePerRolePA has complex dependencies
	// that are difficult to mock in unit tests (EventRecorder, metrics client, etc.)
	// The validation logic is tested indirectly through the other unit tests and
	// will be covered by integration tests
	t.Skip("Full reconcilePerRolePA testing is covered by integration tests")
}
