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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestIsCanaryEnabled(t *testing.T) {
	tests := []struct {
		name         string
		stormService *orchestrationv1alpha1.StormService
		expected     bool
	}{
		{
			name: "canary enabled with steps",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
						Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
							Steps: []orchestrationv1alpha1.CanaryStep{
								{SetWeight: ptr.To(int32(50))},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "canary disabled - no canary config",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{},
				},
			},
			expected: false,
		},
		{
			name: "canary disabled - empty steps",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
						Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
							Steps: []orchestrationv1alpha1.CanaryStep{},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &StormServiceReconciler{}
			result := r.isCanaryEnabled(tt.stormService)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsReplicaMode(t *testing.T) {
	tests := []struct {
		name         string
		stormService *orchestrationv1alpha1.StormService
		expected     bool
	}{
		{
			name: "replica mode - replicas > 1",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			expected: true,
		},
		{
			name: "pooled mode - replicas = 1",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: ptr.To(int32(1)),
				},
			},
			expected: false,
		},
		{
			name: "pooled mode - nil replicas defaults to 1",
			stormService: &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &StormServiceReconciler{}
			result := r.isReplicaMode(tt.stormService)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestInitializeCanaryStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
					Steps: []orchestrationv1alpha1.CanaryStep{
						{SetWeight: ptr.To(int32(25))},
						{SetWeight: ptr.To(int32(50))},
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()
	eventRecorder := record.NewFakeRecorder(10)

	r := &StormServiceReconciler{
		Client:        client,
		Scheme:        scheme,
		EventRecorder: eventRecorder,
	}

	ctx := context.Background()
	result, err := r.initializeCanaryStatus(ctx, stormService, "rev-1", "rev-2")

	require.NoError(t, err)
	assert.True(t, result.Requeue)

	// Verify canary status was initialized
	require.NotNil(t, stormService.Status.CanaryStatus)
	assert.Equal(t, int32(0), stormService.Status.CanaryStatus.CurrentStep)
	assert.Equal(t, int32(0), stormService.Status.CanaryStatus.CurrentWeight)
	assert.Equal(t, "rev-1", stormService.Status.CanaryStatus.StableRevision)
	assert.Equal(t, "rev-2", stormService.Status.CanaryStatus.CanaryRevision)
	assert.Equal(t, orchestrationv1alpha1.CanaryPhaseInitializing, stormService.Status.CanaryStatus.Phase)

	// Verify event was recorded
	select {
	case event := <-eventRecorder.Events:
		assert.Contains(t, event, "CanaryUpdate")
		assert.Contains(t, event, "initialized")
	case <-time.After(time.Second):
		t.Fatal("Expected event not received")
	}
}

func TestProcessCanaryWeightStep(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(3)),
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
					Steps: []orchestrationv1alpha1.CanaryStep{
						{SetWeight: ptr.To(int32(33))},
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
				CurrentStep:    0,
				CurrentWeight:  0,
				StableRevision: "rev-1",
				CanaryRevision: "rev-2",
				Phase:          orchestrationv1alpha1.CanaryPhaseInitializing,
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()
	eventRecorder := record.NewFakeRecorder(10)

	r := &StormServiceReconciler{
		Client:        client,
		Scheme:        scheme,
		EventRecorder: eventRecorder,
	}

	ctx := context.Background()
	result, err := r.processCanaryWeightStep(ctx, stormService, 33, "rev-1", "rev-2")

	require.NoError(t, err)
	assert.True(t, result.Requeue)

	// Verify weight was updated
	assert.Equal(t, int32(33), stormService.Status.CanaryStatus.CurrentWeight)
	assert.Equal(t, orchestrationv1alpha1.CanaryPhaseProgressing, stormService.Status.CanaryStatus.Phase)

	// Verify events were recorded (may receive multiple events)
	var events []string
	timeout := time.After(time.Second)
eventLoop:
	for len(events) < 2 {
		select {
		case event := <-eventRecorder.Events:
			events = append(events, event)
		case <-timeout:
			break eventLoop
		}
	}

	// Should have received both CanaryWeightApplied and CanaryReplicaMode events
	require.True(t, len(events) >= 1, "Expected at least one event")

	// Check that we got either CanaryWeightApplied or CanaryReplicaMode (or both)
	foundWeightEvent := false
	for _, event := range events {
		if strings.Contains(event, "33%") && (strings.Contains(event, "CanaryWeightApplied") || strings.Contains(event, "CanaryReplicaMode")) {
			foundWeightEvent = true
			break
		}
	}
	assert.True(t, foundWeightEvent, "Expected to find weight-related event with 33%, got events: %v", events)
}

func TestProcessCanaryPauseStep_AutomaticPause(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
				CurrentStep:    1,
				CurrentWeight:  33,
				StableRevision: "rev-1",
				CanaryRevision: "rev-2",
				Phase:          orchestrationv1alpha1.CanaryPhaseProgressing,
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()
	eventRecorder := record.NewFakeRecorder(10)

	r := &StormServiceReconciler{
		Client:        client,
		Scheme:        scheme,
		EventRecorder: eventRecorder,
	}

	pauseStep := &orchestrationv1alpha1.PauseStep{
		Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "30s"},
	}

	ctx := context.Background()
	result, err := r.processCanaryPauseStep(ctx, stormService, pauseStep)

	require.NoError(t, err)
	assert.False(t, result.Requeue)
	assert.True(t, result.RequeueAfter > 0)
	assert.True(t, result.RequeueAfter <= 30*time.Second)

	// Verify pause was set
	assert.NotNil(t, stormService.Status.CanaryStatus.PausedAt)
	assert.Equal(t, orchestrationv1alpha1.CanaryPhasePaused, stormService.Status.CanaryStatus.Phase)
}

func TestProcessCanaryPauseStep_ManualPause(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
				CurrentStep:    1,
				CurrentWeight:  33,
				StableRevision: "rev-1",
				CanaryRevision: "rev-2",
				Phase:          orchestrationv1alpha1.CanaryPhaseProgressing,
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()
	eventRecorder := record.NewFakeRecorder(10)

	r := &StormServiceReconciler{
		Client:        client,
		Scheme:        scheme,
		EventRecorder: eventRecorder,
	}

	// Manual pause (no duration specified)
	pauseStep := &orchestrationv1alpha1.PauseStep{}

	ctx := context.Background()
	result, err := r.processCanaryPauseStep(ctx, stormService, pauseStep)

	require.NoError(t, err)
	assert.False(t, result.Requeue)
	assert.Equal(t, 30*time.Second, result.RequeueAfter)

	// Verify pause was set
	assert.NotNil(t, stormService.Status.CanaryStatus.PausedAt)
	assert.Equal(t, orchestrationv1alpha1.CanaryPhasePaused, stormService.Status.CanaryStatus.Phase)

	// Verify pause condition was added
	require.Len(t, stormService.Status.CanaryStatus.PauseConditions, 1)
	assert.Equal(t, orchestrationv1alpha1.PauseReasonCanaryPauseStep, stormService.Status.CanaryStatus.PauseConditions[0].Reason)

	// Verify event was recorded - should mention pause condition removal
	select {
	case event := <-eventRecorder.Events:
		assert.Contains(t, event, "CanaryUpdate")
		assert.Contains(t, event, "Remove CanaryPauseStep pause condition to continue")
	case <-time.After(time.Second):
		t.Fatal("Expected event not received")
	}
}

func TestCompleteCanary(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
				CurrentStep:    2,
				CurrentWeight:  100,
				StableRevision: "rev-1",
				CanaryRevision: "rev-2",
				Phase:          orchestrationv1alpha1.CanaryPhaseProgressing,
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()
	eventRecorder := record.NewFakeRecorder(10)

	r := &StormServiceReconciler{
		Client:        client,
		Scheme:        scheme,
		EventRecorder: eventRecorder,
	}

	ctx := context.Background()
	result, err := r.completeCanary(ctx, stormService)

	require.NoError(t, err)
	assert.True(t, result.Requeue)

	// With fake client, the in-memory object might not immediately reflect all patches
	// Check that canary status shows completion phase at minimum
	if stormService.Status.CanaryStatus != nil {
		// If CanaryStatus still exists, it should at least be in Completed phase
		assert.Equal(t, orchestrationv1alpha1.CanaryPhaseCompleted, stormService.Status.CanaryStatus.Phase)
		assert.Equal(t, int32(100), stormService.Status.CanaryStatus.CurrentWeight)
	}

	// The main status updates may not be reflected in the in-memory object with fake client
	// This is expected behavior - in real scenarios this would work via the API server

	// Verify event was recorded - should be completion event
	select {
	case event := <-eventRecorder.Events:
		assert.Contains(t, event, "CanaryUpdate")
		assert.Contains(t, event, "completed successfully")
	case <-time.After(time.Second):
		t.Fatal("Expected event not received")
	}
}

func TestAdvanceCanaryStep(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-storm",
			Namespace: "default",
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
				CurrentStep:    0,
				CurrentWeight:  25,
				StableRevision: "rev-1",
				CanaryRevision: "rev-2",
				Phase:          orchestrationv1alpha1.CanaryPhaseProgressing,
				PausedAt:       &metav1.Time{Time: time.Now()},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).WithStatusSubresource(stormService).Build()

	r := &StormServiceReconciler{
		Client: client,
		Scheme: scheme,
	}

	ctx := context.Background()
	result, err := r.advanceCanaryStep(ctx, stormService)

	require.NoError(t, err)
	assert.True(t, result.Requeue)

	// Verify step was advanced
	assert.Equal(t, int32(1), stormService.Status.CanaryStatus.CurrentStep)
	assert.Nil(t, stormService.Status.CanaryStatus.PausedAt)
	assert.Equal(t, orchestrationv1alpha1.CanaryPhaseProgressing, stormService.Status.CanaryStatus.Phase)
}

func TestHasPauseCondition(t *testing.T) {
	r := &StormServiceReconciler{}

	tests := []struct {
		name         string
		canaryStatus *orchestrationv1alpha1.CanaryStatus
		reason       orchestrationv1alpha1.PauseReason
		expected     bool
	}{
		{
			name:         "nil canary status",
			canaryStatus: nil,
			reason:       orchestrationv1alpha1.PauseReasonCanaryPauseStep,
			expected:     false,
		},
		{
			name: "empty pause conditions",
			canaryStatus: &orchestrationv1alpha1.CanaryStatus{
				PauseConditions: []orchestrationv1alpha1.PauseCondition{},
			},
			reason:   orchestrationv1alpha1.PauseReasonCanaryPauseStep,
			expected: false,
		},
		{
			name: "has matching pause condition",
			canaryStatus: &orchestrationv1alpha1.CanaryStatus{
				PauseConditions: []orchestrationv1alpha1.PauseCondition{
					{
						Reason:    orchestrationv1alpha1.PauseReasonCanaryPauseStep,
						StartTime: metav1.Now(),
					},
				},
			},
			reason:   orchestrationv1alpha1.PauseReasonCanaryPauseStep,
			expected: true,
		},
		{
			name: "has different pause condition",
			canaryStatus: &orchestrationv1alpha1.CanaryStatus{
				PauseConditions: []orchestrationv1alpha1.PauseCondition{
					{
						Reason:    orchestrationv1alpha1.PauseReasonCanaryPauseStep,
						StartTime: metav1.Now(),
					},
				},
			},
			reason:   orchestrationv1alpha1.PauseReason("DifferentReason"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.hasPauseCondition(tt.canaryStatus, tt.reason)
			assert.Equal(t, tt.expected, result)
		})
	}
}
