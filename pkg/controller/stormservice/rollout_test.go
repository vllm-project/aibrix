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
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestRolloutManager_ProcessCanaryRollout(t *testing.T) {
	tests := []struct {
		name          string
		stormService  *orchestrationv1alpha1.StormService
		expectedPhase orchestrationv1alpha1.RolloutPhase
		expectedStep  int32
	}{
		{
			name: "canary rollout with basic steps",
			stormService: &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-app",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					RolloutStrategy: &orchestrationv1alpha1.RolloutStrategy{
						Type: orchestrationv1alpha1.CanaryRolloutStrategyType,
						Canary: &orchestrationv1alpha1.CanaryStrategy{
							Steps: []orchestrationv1alpha1.CanaryStep{
								{
									Weight: int32Ptr(25),
									Pause: &orchestrationv1alpha1.PauseConfig{
										Duration: &metav1.Duration{Duration: 2 * time.Minute},
									},
								},
								{
									Weight: int32Ptr(50),
									Pause: &orchestrationv1alpha1.PauseConfig{
										UntilApproval: true,
									},
								},
								{
									Weight: int32Ptr(100),
								},
							},
							StableService: "test-app-stable",
							CanaryService: "test-app-canary",
						},
					},
				},
				Status: orchestrationv1alpha1.StormServiceStatus{
					RolloutStatus: &orchestrationv1alpha1.RolloutStatus{
						Phase: orchestrationv1alpha1.RolloutPhaseHealthy,
					},
				},
			},
			expectedPhase: orchestrationv1alpha1.RolloutPhaseProgressing,
			expectedStep:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := &StormServiceReconciler{}
			rm := NewRolloutManager(reconciler)

			ctx := context.Background()
			err := rm.processCanaryRollout(ctx, tt.stormService, "rev-1", "rev-2")

			// Since we're using mock implementations, we expect no error
			assert.NoError(t, err)

			// Verify rollout status was updated
			assert.NotNil(t, tt.stormService.Status.RolloutStatus)
			assert.Equal(t, tt.expectedPhase, tt.stormService.Status.RolloutStatus.Phase)

			if tt.stormService.Status.RolloutStatus.CurrentStepIndex != nil {
				assert.Equal(t, tt.expectedStep, *tt.stormService.Status.RolloutStatus.CurrentStepIndex)
			}
		})
	}
}

func TestRolloutManager_ManualControls(t *testing.T) {
	tests := []struct {
		name              string
		annotation        string
		initialPhase      orchestrationv1alpha1.RolloutPhase
		expectedPhase     orchestrationv1alpha1.RolloutPhase
		expectedCondition orchestrationv1alpha1.RolloutConditionType
	}{
		{
			name:              "manual approval",
			annotation:        RolloutApprovalAnnotation,
			initialPhase:      orchestrationv1alpha1.RolloutPhasePaused,
			expectedPhase:     orchestrationv1alpha1.RolloutPhaseProgressing,
			expectedCondition: orchestrationv1alpha1.RolloutProgressing,
		},
		{
			name:              "manual pause",
			annotation:        RolloutPauseAnnotation,
			initialPhase:      orchestrationv1alpha1.RolloutPhaseProgressing,
			expectedPhase:     orchestrationv1alpha1.RolloutPhasePaused,
			expectedCondition: orchestrationv1alpha1.RolloutPaused,
		},
		{
			name:              "manual abort",
			annotation:        RolloutAbortAnnotation,
			initialPhase:      orchestrationv1alpha1.RolloutPhaseProgressing,
			expectedPhase:     orchestrationv1alpha1.RolloutPhaseAborted,
			expectedCondition: orchestrationv1alpha1.RolloutDegraded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stormService := &orchestrationv1alpha1.StormService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-app",
					Namespace: "default",
					Annotations: map[string]string{
						tt.annotation: "true",
					},
				},
				Status: orchestrationv1alpha1.StormServiceStatus{
					RolloutStatus: &orchestrationv1alpha1.RolloutStatus{
						Phase: tt.initialPhase,
					},
				},
			}

			reconciler := &StormServiceReconciler{}
			rm := NewRolloutManager(reconciler)

			ctx := context.Background()
			err := rm.handleManualControls(ctx, stormService)

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedPhase, stormService.Status.RolloutStatus.Phase)

			// Verify annotation was removed
			_, exists := stormService.GetAnnotations()[tt.annotation]
			assert.False(t, exists)

			// Verify condition was set
			assert.NotEmpty(t, stormService.Status.RolloutStatus.Conditions)
			foundCondition := false
			for _, condition := range stormService.Status.RolloutStatus.Conditions {
				if condition.Type == tt.expectedCondition {
					foundCondition = true
					assert.Equal(t, corev1.ConditionTrue, condition.Status)
					break
				}
			}
			assert.True(t, foundCondition, "Expected condition %s not found", tt.expectedCondition)
		})
	}
}

func TestRolloutManager_BlueGreenRollout(t *testing.T) {
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			RolloutStrategy: &orchestrationv1alpha1.RolloutStrategy{
				Type: orchestrationv1alpha1.BlueGreenRolloutStrategyType,
				BlueGreen: &orchestrationv1alpha1.BlueGreenStrategy{
					ActiveService:         "test-app-active",
					PreviewService:        "test-app-preview",
					AutoPromotionEnabled:  false,
					ScaleDownDelaySeconds: int32Ptr(600),
				},
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			RolloutStatus: &orchestrationv1alpha1.RolloutStatus{
				Phase: orchestrationv1alpha1.RolloutPhaseHealthy,
			},
		},
	}

	reconciler := &StormServiceReconciler{}
	rm := NewRolloutManager(reconciler)

	ctx := context.Background()
	err := rm.processBlueGreenRollout(ctx, stormService, "rev-1", "rev-2")

	assert.NoError(t, err)
	assert.NotNil(t, stormService.Status.RolloutStatus.BlueGreenStatus)
	assert.Equal(t, "rev-1", stormService.Status.RolloutStatus.BlueGreenStatus.ActiveSelector)
	assert.Equal(t, "rev-2", stormService.Status.RolloutStatus.BlueGreenStatus.PreviewSelector)
}

func TestRolloutManager_AutoRollback(t *testing.T) {
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			RolloutStrategy: &orchestrationv1alpha1.RolloutStrategy{
				AutoRollback: &orchestrationv1alpha1.AutoRollbackConfig{
					Enabled: true,
					OnFailure: &orchestrationv1alpha1.RollbackTrigger{
						AnalysisFailure:    true,
						HealthCheckFailure: true,
						ErrorThreshold:     &intstr.IntOrString{Type: intstr.String, StrVal: "5%"},
					},
				},
			},
		},
	}

	reconciler := &StormServiceReconciler{}
	rm := NewRolloutManager(reconciler)

	// Test analysis failure trigger
	assert.True(t, rm.shouldAutoRollback(stormService, "analysis_failure"))

	// Test health check failure trigger
	assert.True(t, rm.shouldAutoRollback(stormService, "health_check_failure"))

	// Test unknown trigger
	assert.False(t, rm.shouldAutoRollback(stormService, "unknown_trigger"))
}

func TestRolloutManager_StepCompletion(t *testing.T) {
	tests := []struct {
		name          string
		step          orchestrationv1alpha1.CanaryStep
		rolloutStatus *orchestrationv1alpha1.RolloutStatus
		expected      bool
	}{
		{
			name: "weight step completed",
			step: orchestrationv1alpha1.CanaryStep{
				Weight: int32Ptr(25),
			},
			rolloutStatus: &orchestrationv1alpha1.RolloutStatus{
				CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
					CurrentWeight: 25,
				},
			},
			expected: true,
		},
		{
			name: "weight step not completed",
			step: orchestrationv1alpha1.CanaryStep{
				Weight: int32Ptr(50),
			},
			rolloutStatus: &orchestrationv1alpha1.RolloutStatus{
				CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
					CurrentWeight: 25,
				},
			},
			expected: false,
		},
		{
			name: "paused step waiting for approval",
			step: orchestrationv1alpha1.CanaryStep{
				Weight: int32Ptr(25),
				Pause: &orchestrationv1alpha1.PauseConfig{
					UntilApproval: true,
				},
			},
			rolloutStatus: &orchestrationv1alpha1.RolloutStatus{
				Phase: orchestrationv1alpha1.RolloutPhasePaused,
				CanaryStatus: &orchestrationv1alpha1.CanaryStatus{
					CurrentWeight: 25,
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stormService := &orchestrationv1alpha1.StormService{
				Status: orchestrationv1alpha1.StormServiceStatus{
					RolloutStatus: tt.rolloutStatus,
				},
			}

			reconciler := &StormServiceReconciler{}
			rm := NewRolloutManager(reconciler)

			ctx := context.Background()
			result := rm.isStepComplete(ctx, stormService, tt.step)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCalculateStepHash(t *testing.T) {
	rm := &RolloutManager{}

	step1 := orchestrationv1alpha1.CanaryStep{
		Weight: int32Ptr(25),
	}

	step2 := orchestrationv1alpha1.CanaryStep{
		Weight: int32Ptr(25),
	}

	step3 := orchestrationv1alpha1.CanaryStep{
		Weight: int32Ptr(50),
	}

	hash1 := rm.calculateStepHash(step1)
	hash2 := rm.calculateStepHash(step2)
	hash3 := rm.calculateStepHash(step3)

	// Same step should produce same hash
	assert.Equal(t, hash1, hash2)

	// Different step should produce different hash
	assert.NotEqual(t, hash1, hash3)

	// Hash should be 8 characters long
	assert.Len(t, hash1, 8)
}

// Helper function for tests
func int32Ptr(i int32) *int32 {
	return &i
}
