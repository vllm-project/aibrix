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
