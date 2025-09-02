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
	"fmt"
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// canaryStatusUpdate represents a canary status update operation
type canaryStatusUpdate struct {
	statusUpdates []func(*orchestrationv1alpha1.CanaryStatus)
	specUpdates   []func(*orchestrationv1alpha1.StormService)
	eventMessages []string
}

// newCanaryStatusUpdate creates a new update operation
func newCanaryStatusUpdate() *canaryStatusUpdate {
	return &canaryStatusUpdate{
		statusUpdates: make([]func(*orchestrationv1alpha1.CanaryStatus), 0),
		specUpdates:   make([]func(*orchestrationv1alpha1.StormService), 0),
		eventMessages: make([]string, 0),
	}
}

// addStatusUpdate adds a status modification function
func (u *canaryStatusUpdate) addStatusUpdate(fn func(*orchestrationv1alpha1.CanaryStatus)) *canaryStatusUpdate {
	u.statusUpdates = append(u.statusUpdates, fn)
	return u
}

// addSpecUpdate adds a spec modification function
func (u *canaryStatusUpdate) addSpecUpdate(fn func(*orchestrationv1alpha1.StormService)) *canaryStatusUpdate {
	u.specUpdates = append(u.specUpdates, fn)
	return u
}

// addEvent adds an event message
func (u *canaryStatusUpdate) addEvent(message string) *canaryStatusUpdate {
	u.eventMessages = append(u.eventMessages, message)
	return u
}

// apply applies all updates and patches the object
func (r *StormServiceReconciler) applyCanaryStatusUpdate(
	ctx context.Context,
	ss *orchestrationv1alpha1.StormService,
	upd *canaryStatusUpdate,
) error {
	if len(upd.specUpdates) > 0 {
		specObj := ss.DeepCopy()
		for _, fn := range upd.specUpdates {
			fn(specObj)
		}
		if err := r.Patch(ctx, specObj, client.MergeFrom(ss)); err != nil {
			return fmt.Errorf("patch spec: %w", err)
		}
		*ss = *specObj
	}

	if len(upd.statusUpdates) > 0 {
		before := ss.DeepCopy()
		if ss.Status.CanaryStatus == nil {
			ss.Status.CanaryStatus = &orchestrationv1alpha1.CanaryStatus{}
		}
		for _, fn := range upd.statusUpdates {
			fn(ss.Status.CanaryStatus) // 只改 status
		}
		if err := r.Status().Patch(ctx, ss, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch status: %w", err)
		}
	}

	for _, msg := range upd.eventMessages {
		r.EventRecorder.Event(ss, "Normal", "CanaryUpdate", msg)
	}
	return nil
}

// isCanaryEnabled checks if canary deployment is configured for the StormService
func (r *StormServiceReconciler) isCanaryEnabled(stormService *orchestrationv1alpha1.StormService) bool {
	return stormService.Spec.UpdateStrategy.Canary != nil &&
		len(stormService.Spec.UpdateStrategy.Canary.Steps) > 0
}

// isReplicaMode determines if the StormService is in replica mode (multiple RoleSets)
func (r *StormServiceReconciler) isReplicaMode(stormService *orchestrationv1alpha1.StormService) bool {
	replicas := int32(1)
	if stormService.Spec.Replicas != nil {
		replicas = *stormService.Spec.Replicas
	}
	return replicas > 1
}

// processCanaryUpdate handles canary deployment progression
func (r *StormServiceReconciler) processCanaryUpdate(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) (ctrl.Result, error) {
	// Initialize canary status if not exists
	if stormService.Status.CanaryStatus == nil {
		return r.initializeCanaryStatus(ctx, stormService, currentRevision, updateRevision)
	}

	canaryStatus := stormService.Status.CanaryStatus
	steps := stormService.Spec.UpdateStrategy.Canary.Steps

	// Check if canary is completed
	if canaryStatus.CurrentStep >= int32(len(steps)) {
		return r.completeCanary(ctx, stormService)
	}

	// Handle pause conditions first
	if stormService.Spec.Paused {
		return r.handleCanaryPause(ctx, stormService)
	}

	// Check if we're waiting for pause condition removal on a manual pause step
	currentStepIndex := canaryStatus.CurrentStep
	if currentStepIndex < int32(len(steps)) {
		currentStep := steps[currentStepIndex]
		// If current step is a manual pause step but no pause condition exists, advance
		if currentStep.Pause != nil && currentStep.Pause.IsManualPause() {
			if !r.hasPauseCondition(canaryStatus, orchestrationv1alpha1.PauseReasonCanaryPauseStep) {
				klog.Infof("Manual pause condition removed for StormService %s/%s, advancing to next step",
					stormService.Namespace, stormService.Name)
				return r.advanceCanaryStep(ctx, stormService)
			}
		}
	}

	// Process current step
	if currentStepIndex >= int32(len(steps)) {
		// Should not happen, but handle gracefully
		return r.completeCanary(ctx, stormService)
	}

	currentStep := steps[currentStepIndex]

	// Handle pause step
	if currentStep.Pause != nil {
		return r.processCanaryPauseStep(ctx, stormService, currentStep.Pause)
	}

	// Handle weight setting step
	if currentStep.SetWeight != nil {
		return r.processCanaryWeightStep(ctx, stormService, *currentStep.SetWeight, currentRevision, updateRevision)
	}

	// If step has neither pause nor setWeight, advance to next step
	return r.advanceCanaryStep(ctx, stormService)
}

// initializeCanaryStatus sets up initial canary deployment state
func (r *StormServiceReconciler) initializeCanaryStatus(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) (ctrl.Result, error) {
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.CurrentStep = 0
			status.CurrentWeight = 0
			status.StableRevision = currentRevision
			status.CanaryRevision = updateRevision
			status.Phase = orchestrationv1alpha1.CanaryPhaseInitializing
		}).
		addEvent(fmt.Sprintf("Canary deployment initialized with %d steps", len(stormService.Spec.UpdateStrategy.Canary.Steps)))

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to initialize canary status: %w", err)
	}

	klog.Infof("Initialized canary deployment for StormService %s/%s", stormService.Namespace, stormService.Name)
	return ctrl.Result{Requeue: true}, nil
}

// processCanaryPauseStep handles pause steps in canary deployment
func (r *StormServiceReconciler) processCanaryPauseStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, pauseStep *orchestrationv1alpha1.PauseStep) (ctrl.Result, error) {
	canaryStatus := stormService.Status.CanaryStatus

	// Check if this is a resume action (duration set to 0 or "0")
	if pauseStep.IsResume() {
		// User has approved the manual pause, advance to next step
		return r.advanceCanaryStep(ctx, stormService)
	}

	// If this is the first time processing this pause step
	if canaryStatus.PausedAt == nil {
		klog.Infof("Setting PausedAt timestamp for StormService %s/%s step %d",
			stormService.Namespace, stormService.Name, canaryStatus.CurrentStep)

		now := metav1.Now()
		update := newCanaryStatusUpdate().
			addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
				status.PausedAt = &now
				status.Phase = orchestrationv1alpha1.CanaryPhasePaused
				
				// Add pause condition to track why it's paused
				pauseCondition := orchestrationv1alpha1.PauseCondition{
					Reason:    orchestrationv1alpha1.PauseReasonCanaryPauseStep,
					StartTime: now,
				}
				status.PauseConditions = append(status.PauseConditions, pauseCondition)
			})

		if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
			klog.Errorf("Failed to update canary pause status for StormService %s/%s: %v",
				stormService.Namespace, stormService.Name, err)
			return ctrl.Result{}, fmt.Errorf("failed to update canary pause status: %w", err)
		}
		klog.Infof("Successfully updated PausedAt for StormService %s/%s",
			stormService.Namespace, stormService.Name)

		// Refresh canaryStatus after update
		canaryStatus = stormService.Status.CanaryStatus
	} else {
		klog.V(2).Infof("PausedAt already set for StormService %s/%s: %v",
			stormService.Namespace, stormService.Name, canaryStatus.PausedAt.Time)
	}

	// Handle automatic pause with duration
	if !pauseStep.IsManualPause() {
		durationSeconds := pauseStep.DurationSeconds()
		if durationSeconds < 0 {
			// Invalid duration string
			r.EventRecorder.Eventf(stormService, "Warning", "CanaryPauseInvalidDuration",
				"Invalid pause duration: %v", pauseStep.Duration.StrVal)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		pauseDuration := time.Duration(durationSeconds) * time.Second
		timeSincePause := time.Since(canaryStatus.PausedAt.Time)

		klog.Infof("Pause timing for StormService %s/%s: pauseDuration=%v, timeSincePause=%v, pausedAt=%v",
			stormService.Namespace, stormService.Name, pauseDuration, timeSincePause, canaryStatus.PausedAt.Time)

		if timeSincePause >= pauseDuration {
			// Duration has elapsed, advance to next step
			klog.Infof("Pause duration elapsed for StormService %s/%s, advancing to next step",
				stormService.Namespace, stormService.Name)
			return r.advanceCanaryStep(ctx, stormService)
		}

		// Still within pause duration, requeue after remaining time
		remainingTime := pauseDuration - timeSincePause
		klog.Infof("Still within pause duration for StormService %s/%s, requeue after %v",
			stormService.Namespace, stormService.Name, remainingTime)

		// Handle edge cases for very small remaining time
		if remainingTime <= time.Second {
			klog.Infof("Remaining time very small (%v), immediate requeue for StormService %s/%s",
				remainingTime, stormService.Namespace, stormService.Name)
			return ctrl.Result{Requeue: true}, nil
		}

		r.EventRecorder.Eventf(stormService, "Normal", "CanaryPauseAutomatic",
			"Canary paused for %v, %v remaining", pauseDuration, remainingTime)
		return ctrl.Result{RequeueAfter: remainingTime}, nil
	}

	// Manual pause - add pause condition instead of setting spec.paused=true
	hasCanaryPauseCondition := r.hasPauseCondition(canaryStatus, orchestrationv1alpha1.PauseReasonCanaryPauseStep)
	if !hasCanaryPauseCondition {
		klog.Infof("Manual pause step reached for StormService %s/%s, adding CanaryPauseStep condition",
			stormService.Namespace, stormService.Name)

		now := metav1.Now()
		pauseCondition := orchestrationv1alpha1.PauseCondition{
			Reason:    orchestrationv1alpha1.PauseReasonCanaryPauseStep,
			StartTime: now,
		}

		update := newCanaryStatusUpdate().
			addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
				status.PauseConditions = append(status.PauseConditions, pauseCondition)
			}).
			addEvent("Canary paused at manual pause step. Remove CanaryPauseStep pause condition to continue")

		if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add CanaryPauseStep pause condition: %w", err)
		}
	} else {
		r.EventRecorder.Eventf(stormService, "Normal", "CanaryPauseManual",
			"Canary paused at manual pause step. Remove CanaryPauseStep pause condition to continue")
	}

	// Requeue periodically to check if pause condition was removed
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// processCanaryWeightStep applies the weight setting and advances to next step
func (r *StormServiceReconciler) processCanaryWeightStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight int32, currentRevision, updateRevision string) (ctrl.Result, error) {
	// Update current weight in status
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.CurrentWeight = weight
			status.Phase = orchestrationv1alpha1.CanaryPhaseProgressing
		}).
		addEvent(fmt.Sprintf("Canary weight set to %d%%", weight))

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update canary weight status: %w", err)
	}

	// Apply the weight-based replica distribution
	if err := r.applyCanaryWeight(ctx, stormService, weight, currentRevision, updateRevision); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply canary weight %d: %w", weight, err)
	}

	// Advance to next step
	return r.advanceCanaryStep(ctx, stormService)
}

// applyCanaryWeight distributes replicas based on canary weight
// This function recalculates replica distribution whenever scaling changes totalReplicas
func (r *StormServiceReconciler) applyCanaryWeight(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight int32, currentRevision, updateRevision string) error {
	totalReplicas := int32(1)
	if stormService.Spec.Replicas != nil {
		totalReplicas = *stormService.Spec.Replicas
	}

	// Calculate new canary replicas based on current totalReplicas and weight
	canaryReplicas := int32(math.Ceil(float64(totalReplicas) * float64(weight) / 100.0))
	stableReplicas := totalReplicas - canaryReplicas

	// Check if this is a scaling event during canary (compare with current status replicas)
	currentStatusReplicas := stormService.Status.Replicas
	
	// If replicas changed during canary, log and notify but continue with recalculation
	if currentStatusReplicas > 0 && currentStatusReplicas != totalReplicas {
		klog.Infof("Scaling detected during canary for StormService %s/%s: %d -> %d replicas, recalculating distribution", 
			stormService.Namespace, stormService.Name, currentStatusReplicas, totalReplicas)
		
		// Log the recalculation event
		r.EventRecorder.Eventf(stormService, "Normal", "CanaryScaling",
			"Recalculated canary distribution due to scaling: %d%% weight with %d total replicas (%d canary, %d stable)", 
			weight, totalReplicas, canaryReplicas, stableReplicas)
	}

	klog.Infof("Applying canary weight %d%% for StormService %s/%s with totalReplicas=%d, canaryReplicas=%d, stableReplicas=%d",
		weight, stormService.Namespace, stormService.Name, totalReplicas, canaryReplicas, stableReplicas)

	if r.isReplicaMode(stormService) {
		return r.applyReplicaModeCanaryWeight(ctx, stormService, weight, totalReplicas, currentRevision, updateRevision)
	} else {
		return r.applyPooledModeCanaryWeight(ctx, stormService, weight, totalReplicas, currentRevision, updateRevision)
	}
}

// applyReplicaModeCanaryWeight distributes new version across RoleSets based on weight
func (r *StormServiceReconciler) applyReplicaModeCanaryWeight(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight, totalReplicas int32, currentRevision, updateRevision string) error {
	// Calculate how many RoleSets should be on new version
	canaryReplicas := int32(math.Ceil(float64(totalReplicas) * float64(weight) / 100.0))
	stableReplicas := totalReplicas - canaryReplicas

	klog.Infof("Replica mode canary: total=%d, canary=%d (%d%%), stable=%d",
		totalReplicas, canaryReplicas, weight, stableReplicas)

	// This will be implemented by calling the existing rollout logic with calculated limits
	// For now, log the calculation
	r.EventRecorder.Eventf(stormService, "Normal", "CanaryReplicaMode",
		"Applying replica mode canary: %d/%d RoleSets on new version (%d%%)",
		canaryReplicas, totalReplicas, weight)

	return nil
}

// applyPooledModeCanaryWeight distributes new version across pods within roles based on weight
func (r *StormServiceReconciler) applyPooledModeCanaryWeight(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight, totalReplicas int32, currentRevision, updateRevision string) error {
	klog.Infof("Pooled mode canary: applying %d%% weight to roles", weight)

	// This will be implemented by calculating per-role canary pod counts
	// For now, log the calculation
	r.EventRecorder.Eventf(stormService, "Normal", "CanaryPooledMode",
		"Applying pooled mode canary: %d%% of pods per role on new version", weight)

	return nil
}

// advanceCanaryStep moves to the next step in canary deployment
func (r *StormServiceReconciler) advanceCanaryStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (ctrl.Result, error) {
	nextStep := stormService.Status.CanaryStatus.CurrentStep + 1

	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.CurrentStep = nextStep
			status.PausedAt = nil // Clear pause timestamp
			status.PauseConditions = nil // Clear pause conditions when resuming
			status.Phase = orchestrationv1alpha1.CanaryPhaseProgressing
		})

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to advance canary step: %w", err)
	}

	klog.Infof("Advanced canary deployment to step %d for StormService %s/%s",
		nextStep, stormService.Namespace, stormService.Name)

	return ctrl.Result{Requeue: true}, nil
}

// handleCanaryPause handles the global pause state during canary
func (r *StormServiceReconciler) handleCanaryPause(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (ctrl.Result, error) {
	canaryStatus := stormService.Status.CanaryStatus
	if canaryStatus.Phase != orchestrationv1alpha1.CanaryPhasePaused {
		update := newCanaryStatusUpdate().
			addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
				status.Phase = orchestrationv1alpha1.CanaryPhasePaused
			}).
			addEvent(fmt.Sprintf("Canary deployment paused at step %d", canaryStatus.CurrentStep))

		if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update canary pause status: %w", err)
		}
	} else {
		r.EventRecorder.Eventf(stormService, "Normal", "CanaryPaused",
			"Canary deployment paused at step %d", canaryStatus.CurrentStep)
	}

	// Requeue to check for resume
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// completeCanary finalizes the canary deployment and promotes stable revision
func (r *StormServiceReconciler) completeCanary(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (ctrl.Result, error) {
	canaryStatus := stormService.Status.CanaryStatus
	if canaryStatus == nil {
		return ctrl.Result{Requeue: true}, nil
	}

	klog.Infof("Completing canary deployment for StormService %s/%s, promoting %s as stable",
		stormService.Namespace, stormService.Name, canaryStatus.CanaryRevision)

	// Step 1: Mark canary as completed with 100% weight
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.Phase = orchestrationv1alpha1.CanaryPhaseCompleted
			status.CurrentWeight = 100
			status.PausedAt = nil
		}).
		addEvent("Canary deployment completed successfully")

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to complete canary deployment: %w", err)
	}

	// Step 2: Promote canary revision as stable and update current revision pointers
	// This ensures subsequent normal rollouts use the new stable version
	promotedRevision := canaryStatus.CanaryRevision
	finalUpdate := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			// Store promoted info temporarily
			status.StableRevision = promotedRevision
		})

	if err := r.applyCanaryStatusUpdate(ctx, stormService, finalUpdate); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to promote canary revision: %w", err)
	}

	// Step 3: Update main status to reflect the promotion
	stormService.Status.CurrentRevision = promotedRevision
	stormService.Status.UpdateRevision = promotedRevision

	// Step 4: Clear canary status - this triggers normal rollout logic to take over
	stormService.Status.CanaryStatus = nil

	// Use single patch operation for the final status update
	original := stormService.DeepCopy()
	if err := r.Status().Patch(ctx, stormService, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to clear canary status and promote revision: %w", err)
	}

	klog.Infof("Successfully completed canary deployment for StormService %s/%s, promoted revision %s",
		stormService.Namespace, stormService.Name, promotedRevision)

	// Step 5: Trigger cleanup of old revisions by returning with requeue
	// The normal scaling/rollout logic will handle cleaning up old RoleSets
	return ctrl.Result{Requeue: true}, nil
}

// hasPauseCondition checks if the canary status has a specific pause condition
func (r *StormServiceReconciler) hasPauseCondition(canaryStatus *orchestrationv1alpha1.CanaryStatus, reason orchestrationv1alpha1.PauseReason) bool {
	if canaryStatus == nil || len(canaryStatus.PauseConditions) == 0 {
		return false
	}

	for _, condition := range canaryStatus.PauseConditions {
		if condition.Reason == reason {
			return true
		}
	}
	return false
}
