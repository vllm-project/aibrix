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
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func TestCalculateReplicas(t *testing.T) {
	type args struct {
		currentReplicas int32
		updatedReplicas int32
		desiredReplicas int32
		desiredCurrent  int32
		desiredUpdated  int32
	}
	tests := []args{
		{
			currentReplicas: 1,
			updatedReplicas: 1,
			desiredReplicas: 4,
			desiredCurrent:  2,
			desiredUpdated:  2,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 1,
			desiredReplicas: 9,
			desiredCurrent:  6,
			desiredUpdated:  3,
		},
		{
			currentReplicas: 6,
			updatedReplicas: 3,
			desiredReplicas: 3,
			desiredCurrent:  2,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 10,
			desiredReplicas: 10,
			desiredCurrent:  1,
			desiredUpdated:  9,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 20,
			desiredReplicas: 25,
			desiredCurrent:  1,
			desiredUpdated:  24,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 20,
			desiredReplicas: 100,
			desiredCurrent:  5,
			desiredUpdated:  95,
		},
		{
			currentReplicas: 10,
			updatedReplicas: 1,
			desiredReplicas: 10,
			desiredCurrent:  9,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 2,
			desiredReplicas: 5,
			desiredCurrent:  3,
			desiredUpdated:  2,
		},
		{
			currentReplicas: 5,
			updatedReplicas: 5,
			desiredReplicas: 3,
			desiredCurrent:  2,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 0,
			updatedReplicas: 0,
			desiredReplicas: 5,
			desiredCurrent:  0,
			desiredUpdated:  5,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 3,
			desiredReplicas: 0,
			desiredCurrent:  0,
			desiredUpdated:  0,
		},
	}
	for _, test := range tests {
		c, u := calculateReplicas(test.desiredReplicas, test.currentReplicas, test.updatedReplicas)
		if c != test.desiredCurrent || u != test.desiredUpdated {
			t.Errorf("failed %+v, current %d, updated %d", test, c, u)
		}
	}
}

func TestSetStormServiceAvailabilityConditionPreservesGangConditionTransitionTime(t *testing.T) {
	oldTransition := metav1.NewTime(time.Now().Add(-time.Hour))
	status := &orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{
			{
				Type:               orchestrationv1alpha1.StormServiceGangSchedulingError,
				Status:             corev1.ConditionFalse,
				Reason:             "GangSchedulingHealthy",
				LastTransitionTime: &oldTransition,
			},
		},
	}

	setStormServiceAvailabilityCondition(status, true)
	SetStormServiceCondition(status, *ctrlutils.NewCondition(
		orchestrationv1alpha1.StormServiceGangSchedulingError,
		corev1.ConditionFalse,
		"GangSchedulingHealthy",
		"",
	))

	gangCondition := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServiceGangSchedulingError)
	if gangCondition == nil {
		t.Fatal("expected gang scheduling condition to be preserved")
	}
	if gangCondition.LastTransitionTime == nil || !gangCondition.LastTransitionTime.Equal(&oldTransition) {
		t.Fatalf("expected gang scheduling LastTransitionTime %v, got %v", oldTransition, gangCondition.LastTransitionTime)
	}
}

func TestUpdateStatusPreservesProgressDeadlineAcrossReconcileError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add StormService scheme: %v", err)
	}

	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	ready := *ctrlutils.NewCondition(
		orchestrationv1alpha1.StormServiceReady,
		corev1.ConditionTrue,
		"Ready",
		"",
	)
	external := *ctrlutils.NewCondition(
		orchestrationv1alpha1.ConditionType("ExternalHealth"),
		corev1.ConditionTrue,
		"Healthy",
		"",
	)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "reconcile-error",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas:                ptr.To(int32(1)),
			Selector:                &metav1.LabelSelector{MatchLabels: map[string]string{"app": "reconcile-error"}},
			ProgressDeadlineSeconds: ptr.To(int32(1)),
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			Conditions: orchestrationv1alpha1.Conditions{
				ready,
				progressingCondition(corev1.ConditionTrue, ProgressingReason, started, started),
				external,
			},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&orchestrationv1alpha1.StormService{}).
		WithObjects(stormService).
		Build()
	reconciler := &StormServiceReconciler{Client: fakeClient, Scheme: scheme}
	currentRevision := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "revision-1"}}
	updateRevision := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "revision-2"}}

	current := &orchestrationv1alpha1.StormService{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stormService), current); err != nil {
		t.Fatalf("get StormService: %v", err)
	}
	readyResult, err := reconciler.updateStatus(
		context.Background(),
		current,
		errors.New("transient rollout error"),
		currentRevision,
		updateRevision,
		0,
	)
	if err != nil {
		t.Fatalf("update error status: %v", err)
	}
	if readyResult {
		t.Fatal("expected StormService with a reconcile error to be not ready")
	}

	failed := &orchestrationv1alpha1.StormService{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stormService), failed); err != nil {
		t.Fatalf("get failed StormService: %v", err)
	}
	if condition := ctrlutils.GetCondition(failed.Status.Conditions, orchestrationv1alpha1.StormServiceReady); condition != nil {
		t.Fatalf("expected stale Ready condition to be removed, got %#v", condition)
	}
	progressing := ctrlutils.GetCondition(failed.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	if progressing == nil || progressing.LastUpdateTime == nil || progressing.Status != corev1.ConditionFalse || progressing.Reason != ProgressDeadlineExceededReason {
		t.Fatalf("expected progress deadline to expire across reconcile error, got %#v", progressing)
	}
	if condition := ctrlutils.GetCondition(failed.Status.Conditions, orchestrationv1alpha1.StormServiceReplicaFailure); condition == nil {
		t.Fatal("expected ReplicaFailure condition")
	}
	if condition := ctrlutils.GetCondition(failed.Status.Conditions, external.Type); condition == nil {
		t.Fatal("expected unrelated condition to be preserved")
	}
	timedOutAt := progressing.LastUpdateTime.DeepCopy()

	readyResult, err = reconciler.updateStatus(
		context.Background(),
		failed,
		nil,
		currentRevision,
		updateRevision,
		0,
	)
	if err != nil {
		t.Fatalf("update recovered status: %v", err)
	}
	if readyResult {
		t.Fatal("expected StormService without RoleSets to be not ready")
	}

	recovered := &orchestrationv1alpha1.StormService{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stormService), recovered); err != nil {
		t.Fatalf("get recovered StormService: %v", err)
	}
	if condition := ctrlutils.GetCondition(recovered.Status.Conditions, orchestrationv1alpha1.StormServiceReplicaFailure); condition != nil {
		t.Fatalf("expected ReplicaFailure to clear after successful reconcile, got %#v", condition)
	}
	progressing = ctrlutils.GetCondition(recovered.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	if progressing == nil || progressing.LastUpdateTime == nil || !progressing.LastUpdateTime.Equal(timedOutAt) {
		t.Fatalf("expected progress deadline clock to remain at %v, got %#v", timedOutAt, progressing)
	}
	if condition := ctrlutils.GetCondition(recovered.Status.Conditions, external.Type); condition == nil {
		t.Fatal("expected unrelated condition to remain after recovery")
	}
}

func TestSyncHeadlessService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name            string
		stormService    *orchestrationv1alpha1.StormService
		existingService *corev1.Service
		wantError       bool
	}{
		{
			name: "create new headless service",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
					UID:       "test-stormservice-uid",
					Labels: map[string]string{
						"app": "test",
					},
				},
			},
			existingService: nil,
			wantError:       false,
		},
		{
			name: "service already exists",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
					UID:       "test-stormservice-uid",
				},
			},
			existingService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "StormService",
							APIVersion: "orchestration.aibrix.org/v1alpha1",
							UID:        "test-stormservice-uid",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector:  map[string]string{}, // empty selector that should be updated
				},
			},
			wantError: false,
		},
		{
			name: "service already exists with PublishNotReadyAddresses false",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
					UID:       "test-stormservice-uid",
				},
			},
			existingService: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "StormService",
							APIVersion: "orchestration.aibrix.org/v1alpha1",
							UID:        "test-stormservice-uid",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:                     corev1.ServiceTypeClusterIP,
					ClusterIP:                corev1.ClusterIPNone,
					Selector:                 map[string]string{constants.StormServiceNameLabelKey: "test-storm"},
					PublishNotReadyAddresses: false, // should be updated to true
				},
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.existingService != nil {
				objs = append(objs, tt.existingService)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			r := &StormServiceReconciler{
				Client:        fakeClient,
				EventRecorder: &record.FakeRecorder{},
			}

			err := r.syncHeadlessService(context.TODO(), tt.stormService)

			if (err != nil) != tt.wantError {
				t.Errorf("syncHeadlessService() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// Check if service was created/updated
			service := &corev1.Service{}
			err = fakeClient.Get(context.TODO(), client.ObjectKey{
				Name:      tt.stormService.Name,
				Namespace: tt.stormService.Namespace,
			}, service)

			if err != nil {
				t.Errorf("Failed to get service: %v", err)
				return
			}

			// Verify service properties
			if service.Spec.ClusterIP != corev1.ClusterIPNone {
				t.Errorf("Expected ClusterIP to be None, got %s", service.Spec.ClusterIP)
			}

			if len(service.OwnerReferences) == 0 {
				t.Error("Expected service to have an owner reference")
			} else {
				ownerRef := service.OwnerReferences[0]
				if ownerRef.Kind != orchestrationv1alpha1.StormServiceKind || ownerRef.UID != tt.stormService.UID {
					t.Errorf("Expected owner reference to be %s %s, got %s %s", orchestrationv1alpha1.StormServiceKind, tt.stormService.UID, ownerRef.Kind, ownerRef.UID)
				}
			}

			expectedSelector := map[string]string{constants.StormServiceNameLabelKey: tt.stormService.Name}
			if !reflect.DeepEqual(service.Spec.Selector, expectedSelector) {
				t.Errorf("Expected selector %v, got %v", expectedSelector, service.Spec.Selector)
			}

			if service.Spec.Type != corev1.ServiceTypeClusterIP {
				t.Errorf("Expected service type ClusterIP, got %v", service.Spec.Type)
			}

			if service.Spec.PublishNotReadyAddresses != true {
				t.Errorf("Expected PublishNotReadyAddresses to be true, got %v", service.Spec.PublishNotReadyAddresses)
			}
		})
	}
}

// newPooledStormServiceWithSurge returns a pooled StormService shaped the way the CRD
// persists it: mode: Pooled with updateStrategy.type defaulted to RollingUpdate and an
// explicit maxSurge. Before IsRollingUpdate was routed through EffectiveUpdateStrategyType,
// this combination handed scaling() a non-zero surge budget.
func newPooledStormServiceWithSurge(maxSurge int32) *orchestrationv1alpha1.StormService {
	surge := intstrutil.FromInt32(maxSurge)
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pooled-storm",
			Namespace: "default",
			UID:       "pooled-storm-uid",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(1)),
			Mode:     orchestrationv1alpha1.StormServicePooledMode,
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				// The CRD defaults type to RollingUpdate whenever the updateStrategy
				// block is present, so this is what a pooled object with maxSurge set
				// actually looks like in the API server.
				Type:     orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
				MaxSurge: &surge,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "pooled-storm"},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "pooled-storm"},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:     "engine",
							Replicas: ptr.To(int32(1)),
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "main", Image: "engine:v1"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// pooledRoleSet returns a RoleSet owned by newPooledStormServiceWithSurge's object at the
// given revision. Terminating RoleSets carry a DeletionTimestamp plus a finalizer so the
// fake client keeps them around, mirroring a RoleSet that is still tearing down.
func pooledRoleSet(name, revision string, terminating bool) *orchestrationv1alpha1.RoleSet {
	rs := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app":                                  "pooled-storm",
				constants.StormServiceNameLabelKey:     "pooled-storm",
				constants.StormServiceRevisionLabelKey: revision,
			},
		},
	}
	if terminating {
		now := metav1.Now()
		rs.DeletionTimestamp = &now
		rs.Finalizers = []string{"orchestration.aibrix.ai/test-teardown"}
	}
	return rs
}

// TestScalingPooledModeNeverCreatesSecondRoleSet covers the review scenario for
// mode: Pooled + CRD-defaulted RollingUpdate + maxSurge: 2: the surge budget must
// evaluate to 0 so scaling() never brings a second RoleSet into existence next to
// the one RoleSet a pooled StormService owns.
func TestScalingPooledModeNeverCreatesSecondRoleSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	const revision = "pooled-storm-rev1"

	tests := []struct {
		name             string
		existingRoleSets []*orchestrationv1alpha1.RoleSet
		wantScaling      bool
		wantRoleSets     int
	}{
		{
			name:             "steady state keeps the single roleset",
			existingRoleSets: []*orchestrationv1alpha1.RoleSet{pooledRoleSet("pooled-storm-roleset-a", revision, false)},
			wantScaling:      false,
			wantRoleSets:     1,
		},
		{
			name:             "scale out from zero creates exactly one roleset",
			existingRoleSets: nil,
			wantScaling:      true,
			wantRoleSets:     1,
		},
		{
			// The regression case: with the surge budget read from the raw
			// updateStrategy.type, scaling() created a replacement RoleSet while the
			// old one was still terminating, so two RoleSets existed at once.
			name:             "no replacement is surged while the old roleset terminates",
			existingRoleSets: []*orchestrationv1alpha1.RoleSet{pooledRoleSet("pooled-storm-roleset-a", revision, true)},
			wantScaling:      false,
			wantRoleSets:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			for _, rs := range tt.existingRoleSets {
				objs = append(objs, rs)
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				Build()

			r := &StormServiceReconciler{
				Client:        fakeClient,
				EventRecorder: &record.FakeRecorder{},
			}

			stormService := newPooledStormServiceWithSurge(2)
			cr := &appsv1.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{Name: revision, Namespace: "default"},
				Revision:   1,
			}

			scaling, err := r.scaling(context.TODO(), stormService, stormService, cr, cr)
			if err != nil {
				t.Fatalf("scaling() error = %v", err)
			}
			if scaling != tt.wantScaling {
				t.Errorf("scaling() = %v, want %v", scaling, tt.wantScaling)
			}

			roleSetList := &orchestrationv1alpha1.RoleSetList{}
			if err := fakeClient.List(context.TODO(), roleSetList); err != nil {
				t.Fatalf("failed to list roleSets: %v", err)
			}
			if len(roleSetList.Items) != tt.wantRoleSets {
				names := make([]string, 0, len(roleSetList.Items))
				for _, rs := range roleSetList.Items {
					names = append(names, rs.Name)
				}
				t.Fatalf("expected %d roleSet(s), got %d: %v", tt.wantRoleSets, len(roleSetList.Items), names)
			}
			// A pooled StormService must never own more than one RoleSet, no matter
			// how often scaling() runs; re-run to make sure the budget stays zero.
			if _, err := r.scaling(context.TODO(), stormService, stormService, cr, cr); err != nil {
				t.Fatalf("second scaling() error = %v", err)
			}
			if err := fakeClient.List(context.TODO(), roleSetList); err != nil {
				t.Fatalf("failed to list roleSets: %v", err)
			}
			if len(roleSetList.Items) > 1 {
				t.Fatalf("pooled stormservice ended up with %d roleSets, a second RoleSet must never be created", len(roleSetList.Items))
			}
		})
	}
}
