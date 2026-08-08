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

package circuitbreaker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEvaluateClosedOpenTransitions(t *testing.T) {
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	previousTransition := metav1.NewTime(now.Add(-time.Hour))

	tests := []struct {
		name                 string
		in                   Input
		wantStatus           Status
		wantOverride         bool
		wantDesiredReplicas  int32
		wantBlockScaleDown   bool
		wantTransition       Transition
		wantReason           string
		wantTransitionUpdate bool
	}{
		{
			name: "closed healthy clears failure count",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 2, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllHealthy,
				CurrentReplicas: 5,
				DesiredReplicas: 6,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:           Status{State: v1alpha1.CircuitBreakerStateClosed, LastTransitionTime: &previousTransition, Reason: ReasonMetricsHealthy},
			wantDesiredReplicas:  6,
			wantReason:           ReasonMetricsHealthy,
			wantTransitionUpdate: false,
		},
		{
			name: "closed degraded blocks scale down without opening",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 1, LastTransitionTime: &previousTransition},
				Round:           RoundHealthDegraded,
				CurrentReplicas: 5,
				DesiredReplicas: 3,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:          Status{State: v1alpha1.CircuitBreakerStateClosed, LastTransitionTime: &previousTransition, Reason: ReasonDegraded},
			wantDesiredReplicas: 3,
			wantBlockScaleDown:  true,
			wantReason:          ReasonDegraded,
		},
		{
			name: "closed all failed below threshold keeps current replicas",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 1, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:          Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 2, LastTransitionTime: &previousTransition, Reason: ReasonAllFailed},
			wantOverride:        true,
			wantDesiredReplicas: 5,
			wantReason:          ReasonAllFailed,
		},
		{
			name: "closed all failed reaches threshold and opens",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 2, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:           Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, FailureCount: 3, ProtectedReplicas: int32Ptr(5), LastTransitionTime: ptrTime(now), Reason: ReasonOpened},
			wantOverride:         true,
			wantDesiredReplicas:  5,
			wantTransition:       TransitionOpened,
			wantReason:           ReasonOpened,
			wantTransitionUpdate: true,
		},
		{
			name: "closed threshold one opens on first all failed round",
			in: Input{
				Config:          enabledConfig(1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 4,
				DesiredReplicas: 6,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:           Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, FailureCount: 1, ProtectedReplicas: int32Ptr(4), LastTransitionTime: ptrTime(now), Reason: ReasonOpened},
			wantOverride:         true,
			wantDesiredReplicas:  4,
			wantTransition:       TransitionOpened,
			wantReason:           ReasonOpened,
			wantTransitionUpdate: true,
		},
		{
			name: "open healthy below recovery threshold keeps protecting",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 1, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllHealthy,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:          Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 2, ProtectedReplicas: int32Ptr(5), LastTransitionTime: &previousTransition, Reason: ReasonRecovering},
			wantOverride:        true,
			wantDesiredReplicas: 5,
			wantReason:          ReasonRecovering,
		},
		{
			name: "open healthy reaches recovery threshold and closes",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 2, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllHealthy,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:           Status{State: v1alpha1.CircuitBreakerStateClosed, LastTransitionTime: ptrTime(now), Reason: ReasonClosed},
			wantDesiredReplicas:  8,
			wantTransition:       TransitionClosed,
			wantReason:           ReasonClosed,
			wantTransitionUpdate: true,
		},
		{
			name: "open degraded resets recovery and keeps protecting",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 2, LastTransitionTime: &previousTransition},
				Round:           RoundHealthDegraded,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:          Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: int32Ptr(5), LastTransitionTime: &previousTransition, Reason: ReasonDegraded},
			wantOverride:        true,
			wantDesiredReplicas: 5,
			wantReason:          ReasonDegraded,
		},
		{
			name: "open all failed resets recovery and keeps protecting",
			in: Input{
				Config:          enabledConfig(3, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 2, LastTransitionTime: &previousTransition},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 8,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantStatus:          Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: int32Ptr(5), LastTransitionTime: &previousTransition, Reason: ReasonAllFailed},
			wantOverride:        true,
			wantDesiredReplicas: 5,
			wantReason:          ReasonAllFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.in

			got := Evaluate(tt.in)

			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, tt.wantOverride, got.Override)
			assert.Equal(t, tt.wantDesiredReplicas, got.DesiredReplicas)
			assert.Equal(t, tt.wantBlockScaleDown, got.BlockScaleDown)
			assert.Equal(t, tt.wantTransition, got.Transition)
			assert.Equal(t, tt.wantReason, got.Reason)
			assert.Equal(t, before, tt.in)
			if tt.wantTransitionUpdate {
				assert.Equal(t, now, got.Status.LastTransitionTime.Time)
			} else if tt.in.Previous.LastTransitionTime != nil {
				assert.Equal(t, tt.in.Previous.LastTransitionTime, got.Status.LastTransitionTime)
			}
			assert.GreaterOrEqual(t, got.DesiredReplicas, tt.in.MinReplicas)
			assert.LessOrEqual(t, got.DesiredReplicas, tt.in.MaxReplicas)
			assert.GreaterOrEqual(t, got.Status.FailureCount, int32(0))
			assert.GreaterOrEqual(t, got.Status.RecoveryCount, int32(0))
		})
	}
}

func TestEvaluateProtectiveActions(t *testing.T) {
	now := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	frozenReplicas := int32(5)

	tests := []struct {
		name                string
		in                  Input
		wantDesiredReplicas int32
		wantProtected       *int32
		wantAction          v1alpha1.CircuitBreakerAction
	}{
		{
			name: "freeze locks current replicas when opening",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 9,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantDesiredReplicas: 5,
			wantProtected:       int32Ptr(5),
			wantAction:          v1alpha1.CircuitBreakerActionFreeze,
		},
		{
			name: "freeze corrects external scale up drift back to protected replicas",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: &frozenReplicas},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 8,
				DesiredReplicas: 9,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantDesiredReplicas: 5,
			wantProtected:       int32Ptr(5),
			wantAction:          v1alpha1.CircuitBreakerActionFreeze,
		},
		{
			name: "freeze corrects external scale down drift back to protected replicas",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: &frozenReplicas},
				Round:           RoundHealthDegraded,
				CurrentReplicas: 2,
				DesiredReplicas: 9,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantDesiredReplicas: 5,
			wantProtected:       int32Ptr(5),
			wantAction:          v1alpha1.CircuitBreakerActionFreeze,
		},
		{
			name: "freeze clamps protected replicas when bounds change",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: &frozenReplicas},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 9,
				MinReplicas:     6,
				MaxReplicas:     7,
				Now:             now,
			},
			wantDesiredReplicas: 6,
			wantProtected:       int32Ptr(6),
			wantAction:          v1alpha1.CircuitBreakerActionFreeze,
		},
		{
			name: "max action uses current max replicas and clears protected replicas",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionMax, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionMax, ProtectedReplicas: &frozenReplicas},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 5,
				DesiredReplicas: 3,
				MinReplicas:     1,
				MaxReplicas:     12,
				Now:             now,
			},
			wantDesiredReplicas: 12,
			wantAction:          v1alpha1.CircuitBreakerActionMax,
		},
		{
			name: "freeze to max hot switch clears protected replicas",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionMax, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, ProtectedReplicas: &frozenReplicas},
				Round:           RoundHealthDegraded,
				CurrentReplicas: 5,
				DesiredReplicas: 9,
				MinReplicas:     1,
				MaxReplicas:     11,
				Now:             now,
			},
			wantDesiredReplicas: 11,
			wantAction:          v1alpha1.CircuitBreakerActionMax,
		},
		{
			name: "max to freeze hot switch locks current replicas",
			in: Input{
				Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
				Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionMax},
				Round:           RoundHealthAllFailed,
				CurrentReplicas: 7,
				DesiredReplicas: 9,
				MinReplicas:     1,
				MaxReplicas:     10,
				Now:             now,
			},
			wantDesiredReplicas: 7,
			wantProtected:       int32Ptr(7),
			wantAction:          v1alpha1.CircuitBreakerActionFreeze,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.in)

			assert.True(t, got.Override)
			assert.Equal(t, v1alpha1.CircuitBreakerStateOpen, got.Status.State)
			assert.Equal(t, tt.wantAction, got.Status.Action)
			assert.Equal(t, tt.wantDesiredReplicas, got.DesiredReplicas)
			assert.Equal(t, tt.wantProtected, got.Status.ProtectedReplicas)
		})
	}
}

func TestEvaluateConfigChange(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	t.Run("failure threshold hot decrease opens on next round", func(t *testing.T) {
		got := Evaluate(Input{
			Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 2, 3),
			Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed, FailureCount: 1},
			Round:           RoundHealthAllFailed,
			CurrentReplicas: 4,
			DesiredReplicas: 8,
			MinReplicas:     1,
			MaxReplicas:     10,
			Now:             now,
		})

		assert.Equal(t, TransitionOpened, got.Transition)
		assert.Equal(t, v1alpha1.CircuitBreakerStateOpen, got.Status.State)
		assert.Equal(t, int32(2), got.Status.FailureCount)
	})

	t.Run("recovery threshold hot decrease closes on next healthy round", func(t *testing.T) {
		got := Evaluate(Input{
			Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 3, 2),
			Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, RecoveryCount: 1},
			Round:           RoundHealthAllHealthy,
			CurrentReplicas: 4,
			DesiredReplicas: 8,
			MinReplicas:     1,
			MaxReplicas:     10,
			Now:             now,
		})

		assert.Equal(t, TransitionClosed, got.Transition)
		assert.Equal(t, v1alpha1.CircuitBreakerStateClosed, got.Status.State)
		assert.False(t, got.Override)
		assert.Equal(t, int32(8), got.DesiredReplicas)
	})

	t.Run("disabled clears runtime state", func(t *testing.T) {
		protected := int32(4)

		got := Evaluate(Input{
			Config:          config.Config{Enabled: false},
			Previous:        Status{State: v1alpha1.CircuitBreakerStateOpen, Action: v1alpha1.CircuitBreakerActionFreeze, FailureCount: 3, RecoveryCount: 1, ProtectedReplicas: &protected},
			Round:           RoundHealthAllFailed,
			CurrentReplicas: 4,
			DesiredReplicas: 8,
			MinReplicas:     1,
			MaxReplicas:     10,
			Now:             now,
		})

		assert.Equal(t, Status{}, got.Status)
		assert.False(t, got.Override)
		assert.Equal(t, int32(8), got.DesiredReplicas)
	})
}

func TestEvaluateRestartRecovery(t *testing.T) {
	now := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC)

	opened := Evaluate(Input{
		Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
		Previous:        Status{State: v1alpha1.CircuitBreakerStateClosed},
		Round:           RoundHealthAllFailed,
		CurrentReplicas: 6,
		DesiredReplicas: 9,
		MinReplicas:     1,
		MaxReplicas:     10,
		Now:             now,
	})
	apiStatus := toAPIStatus(opened.Status)
	restored := statusFromAPI(apiStatus)

	got := Evaluate(Input{
		Config:          enabledActionConfig(v1alpha1.CircuitBreakerActionFreeze, 1, 3),
		Previous:        restored,
		Round:           RoundHealthAllFailed,
		CurrentReplicas: 3,
		DesiredReplicas: 9,
		MinReplicas:     1,
		MaxReplicas:     10,
		Now:             now.Add(time.Minute),
	})

	assert.Equal(t, v1alpha1.CircuitBreakerStateOpen, got.Status.State)
	assert.Equal(t, v1alpha1.CircuitBreakerActionFreeze, got.Status.Action)
	assert.Equal(t, int32(1), got.Status.FailureCount)
	assert.Equal(t, int32(6), *got.Status.ProtectedReplicas)
	assert.Equal(t, int32(6), got.DesiredReplicas)
}

func enabledConfig(failureThreshold, recoveryThreshold int32) config.Config {
	return config.Config{
		Enabled:           true,
		Action:            v1alpha1.CircuitBreakerActionFreeze,
		FailureThreshold:  failureThreshold,
		RecoveryThreshold: recoveryThreshold,
	}
}

func enabledActionConfig(action v1alpha1.CircuitBreakerAction, failureThreshold, recoveryThreshold int32) config.Config {
	return config.Config{
		Enabled:           true,
		Action:            action,
		FailureThreshold:  failureThreshold,
		RecoveryThreshold: recoveryThreshold,
	}
}

func int32Ptr(v int32) *int32 {
	return &v
}

func toAPIStatus(status Status) v1alpha1.CircuitBreakerStatus {
	return v1alpha1.CircuitBreakerStatus{
		State:              status.State,
		Action:             status.Action,
		FailureCount:       status.FailureCount,
		RecoveryCount:      status.RecoveryCount,
		ProtectedReplicas:  status.ProtectedReplicas,
		Reason:             status.Reason,
		LastTransitionTime: status.LastTransitionTime,
	}
}

func statusFromAPI(status v1alpha1.CircuitBreakerStatus) Status {
	return Status{
		State:              status.State,
		Action:             status.Action,
		FailureCount:       status.FailureCount,
		RecoveryCount:      status.RecoveryCount,
		ProtectedReplicas:  status.ProtectedReplicas,
		Reason:             status.Reason,
		LastTransitionTime: status.LastTransitionTime,
	}
}

func ptrTime(t time.Time) *metav1.Time {
	out := metav1.NewTime(t)
	return &out
}
