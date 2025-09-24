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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
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
			fn(ss.Status.CanaryStatus)
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
		// For manual pause steps, check if we should resume
		if currentStep.Pause != nil && currentStep.Pause.IsManualPause() {
			// Check if the pause condition exists
			hasPauseCondition := r.hasPauseCondition(canaryStatus, orchestrationv1alpha1.PauseReasonCanaryPauseStep)

			// Check if we're in a state where we should have a pause condition but don't
			// This indicates the user has removed the pause condition to resume
			if canaryStatus.Phase == orchestrationv1alpha1.CanaryPhasePaused && !hasPauseCondition {
				klog.Infof("Manual pause condition removed for StormService %s/%s, checking if ready to advance",
					stormService.Namespace, stormService.Name)

				// Before advancing, check if the previous weight step's target has been achieved
				// Look for the last setWeight step before this pause
				var lastWeightStep *orchestrationv1alpha1.CanaryStep
				for i := currentStepIndex - 1; i >= 0; i-- {
					if steps[i].SetWeight != nil {
						lastWeightStep = &steps[i]
						break
					}
				}

				if lastWeightStep != nil {
					// Check if the weight target from the previous step has been achieved
					achieved, requeueAfter := r.isCanaryTargetAchieved(ctx, stormService, updateRevision)
					if !achieved {
						klog.Infof("Manual pause removed but previous weight step (%d%%) not yet achieved, waiting before advancing",
							*lastWeightStep.SetWeight)
						return ctrl.Result{RequeueAfter: requeueAfter}, nil
					}
				}

				return r.advanceCanaryStep(ctx, stormService)
			}
			// If we don't have the pause condition yet, we need to process this pause step first
			// This will be handled by processCanaryPauseStep below
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
	// Reuse existing revisions if they exist and are different, otherwise set them
	before := stormService.DeepCopy()

	// Only update revision fields if they are not already set or if they differ from the calculated ones
	needRevisionUpdate := false
	if stormService.Status.CurrentRevision == "" || stormService.Status.CurrentRevision != currentRevision {
		stormService.Status.CurrentRevision = currentRevision
		needRevisionUpdate = true
	}
	if stormService.Status.UpdateRevision == "" || stormService.Status.UpdateRevision != updateRevision {
		stormService.Status.UpdateRevision = updateRevision
		needRevisionUpdate = true
	}

	if needRevisionUpdate {
		if err := r.Status().Patch(ctx, stormService, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set revision fields: %w", err)
		}
	}

	// Then update the canary status
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.CurrentStep = 0
			status.Phase = orchestrationv1alpha1.CanaryPhaseInitializing
			// NOTE: Revisions are stored in main status fields (currentRevision, updateRevision)
		}).
		addEvent(fmt.Sprintf("Canary deployment initialized with %d steps", len(stormService.Spec.UpdateStrategy.Canary.Steps)))

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to initialize canary status: %w", err)
	}

	klog.Infof("Initialized canary deployment for StormService %s/%s from %s to %s",
		stormService.Namespace, stormService.Name, currentRevision, updateRevision)
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

	// Check if this is the first time processing this pause step
	pausedAt := r.getPausedAt(canaryStatus)
	if pausedAt == nil {
		klog.Infof("Setting pause condition for StormService %s/%s step %d",
			stormService.Namespace, stormService.Name, canaryStatus.CurrentStep)

		now := metav1.Now()
		update := newCanaryStatusUpdate().
			addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
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
		klog.Infof("Successfully set pause condition for StormService %s/%s",
			stormService.Namespace, stormService.Name)

		// Refresh pausedAt after update
		pausedAt = r.getPausedAt(stormService.Status.CanaryStatus)
	} else {
		klog.V(2).Infof("Pause condition already set for StormService %s/%s: %v",
			stormService.Namespace, stormService.Name, pausedAt.Time)
	}

	// Handle automatic pause with duration
	if !pauseStep.IsManualPause() {
		durationSeconds := pauseStep.DurationSeconds()
		if durationSeconds < 0 {
			// Invalid duration string - treat as error
			r.EventRecorder.Eventf(stormService, "Warning", "CanaryPauseInvalidDuration",
				"Invalid pause duration: %v, treating as manual pause", pauseStep.Duration.StrVal)
			// Treat invalid duration as manual pause to avoid infinite loop
			return r.processManualPause(ctx, stormService, canaryStatus)
		}

		pauseDuration := time.Duration(durationSeconds) * time.Second
		timeSincePause := time.Since(pausedAt.Time)

		klog.Infof("Pause timing for StormService %s/%s: pauseDuration=%v, timeSincePause=%v, pausedAt=%v",
			stormService.Namespace, stormService.Name, pauseDuration, timeSincePause, pausedAt.Time)

		if timeSincePause >= pauseDuration {
			// Duration has elapsed, but check if previous weight step target was achieved before advancing
			klog.Infof("Pause duration elapsed for StormService %s/%s, checking if ready to advance",
				stormService.Namespace, stormService.Name)

			// Before advancing, check if the previous weight step's target has been achieved
			// Look for the last setWeight step before this pause
			currentStepIndex := canaryStatus.CurrentStep
			steps := stormService.Spec.UpdateStrategy.Canary.Steps
			var lastWeightStep *orchestrationv1alpha1.CanaryStep
			for i := currentStepIndex - 1; i >= 0; i-- {
				if steps[i].SetWeight != nil {
					lastWeightStep = &steps[i]
					break
				}
			}

			if lastWeightStep != nil {
				// Check if the weight target from the previous step has been achieved
				// Use the canary revision from main status
				updateRevision := stormService.Status.UpdateRevision
				achieved, requeueAfter := r.isCanaryTargetAchieved(ctx, stormService, updateRevision)
				if !achieved {
					klog.Infof("Pause duration elapsed but previous weight step (%d%%) not yet achieved, waiting before advancing",
						*lastWeightStep.SetWeight)
					return ctrl.Result{RequeueAfter: requeueAfter}, nil
				}
				klog.Infof("Previous weight step (%d%%) achieved, ready to advance",
					*lastWeightStep.SetWeight)
			}

			return r.advanceCanaryStep(ctx, stormService)
		}

		// Still within pause duration, requeue after remaining time
		remainingTime := pauseDuration - timeSincePause
		klog.Infof("Still within pause duration for StormService %s/%s, requeue after %v",
			stormService.Namespace, stormService.Name, remainingTime)

		// Handle edge cases for very small remaining time
		if remainingTime <= 0 {
			// Time already elapsed, advance immediately
			klog.Infof("Pause duration already elapsed for StormService %s/%s, advancing immediately",
				stormService.Namespace, stormService.Name)
			return r.advanceCanaryStep(ctx, stormService)
		}

		// For small remaining times, use a minimum requeue interval to avoid rapid reconciliation
		if remainingTime < 5*time.Second {
			remainingTime = 5 * time.Second
		}

		// Only emit countdown event at certain intervals to reduce spam
		// Emit at: start, 75%, 50%, 25%, and <5 seconds
		percentRemaining := float64(remainingTime) / float64(pauseDuration) * 100
		shouldEmitEvent := timeSincePause < 1*time.Second || // Just started
			(percentRemaining <= 75 && percentRemaining > 74) ||
			(percentRemaining <= 50 && percentRemaining > 49) ||
			(percentRemaining <= 25 && percentRemaining > 24) ||
			remainingTime < 5*time.Second

		if shouldEmitEvent {
			r.EventRecorder.Eventf(stormService, "Normal", "CanaryPauseAutomatic",
				"Canary paused for %v, %v remaining", pauseDuration, remainingTime)
		}
		return ctrl.Result{RequeueAfter: remainingTime}, nil
	}

	// Manual pause - delegate to separate function for clarity
	return r.processManualPause(ctx, stormService, canaryStatus)
}

// processManualPause handles manual pause steps that require user intervention
func (r *StormServiceReconciler) processManualPause(ctx context.Context, stormService *orchestrationv1alpha1.StormService, canaryStatus *orchestrationv1alpha1.CanaryStatus) (ctrl.Result, error) {
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
	}
	// Don't emit duplicate events - the pause condition is already set

	// Requeue periodically to check if pause condition was removed
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// processCanaryWeightStep applies the weight setting and advances to next step
func (r *StormServiceReconciler) processCanaryWeightStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight int32, currentRevision, updateRevision string) (ctrl.Result, error) {
	// Only emit event if phase is changing or this is first time applying this weight
	lastWeight := r.getCurrentWeight(stormService)
	needsEvent := lastWeight != weight

	// Update phase if needed
	if needsEvent {
		update := newCanaryStatusUpdate().
			addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
				status.Phase = orchestrationv1alpha1.CanaryPhaseProgressing
			}).
			addEvent(fmt.Sprintf("Canary weight set to %d%%", weight))

		if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update canary weight status: %w", err)
		}
	}

	// Apply the weight-based replica distribution
	if err := r.applyCanaryWeight(ctx, stormService, weight, currentRevision, updateRevision); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply canary weight %d: %w", weight, err)
	}

	// Check if the canary target has been achieved before advancing
	if achieved, requeue := r.isCanaryTargetAchieved(ctx, stormService, updateRevision); achieved {
		// Target achieved, advance to next step
		klog.Infof("Canary target achieved for weight %d%% at step %d, advancing to next step",
			weight, stormService.Status.CanaryStatus.CurrentStep)
		return r.advanceCanaryStep(ctx, stormService)
	} else {
		// Target not yet achieved, requeue to wait for rollout completion
		klog.Infof("Canary target not yet achieved for weight %d%% at step %d, waiting for rollout completion (requeue after %v)",
			weight, stormService.Status.CanaryStatus.CurrentStep, requeue)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
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
	// Calculate desired canary replicas based on weight
	desiredCanaryReplicas := int32(math.Ceil(float64(totalReplicas) * float64(weight) / 100.0))

	// Get current RoleSets to check rollout constraints
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return fmt.Errorf("failed to get RoleSets for constraint calculation: %w", err)
	}

	// Calculate achievable canary replicas considering rollout constraints
	achievableCanaryReplicas := r.calculateAchievableCanaryReplicas(stormService, allRoleSets, updateRevision, desiredCanaryReplicas)

	// Get actual current canary replicas (for logging and event reporting)
	activeRoleSets, _ := filterTerminatingRoleSets(allRoleSets)
	updated, _ := filterRoleSetByRevision(activeRoleSets, updateRevision)
	actualCanaryReplicas := int32(len(updated))

	stableReplicas := totalReplicas - actualCanaryReplicas

	klog.Infof("Replica mode canary: total=%d, desired_canary=%d, achievable_canary=%d, actual_canary=%d (%d%%), stable=%d",
		totalReplicas, desiredCanaryReplicas, achievableCanaryReplicas, actualCanaryReplicas, weight, stableReplicas)

	// Status fields (Replicas, UpdatedReplicas, UpdatedReadyReplicas, CurrentReplicas)
	// are calculated and updated by the main sync logic in updateStatus() function.
	// We don't update them here to avoid conflicts.
	klog.Infof("CanaryReplicaMode: %d/%d updated (%d%%), target=%d",
		actualCanaryReplicas, totalReplicas, weight, achievableCanaryReplicas)

	// The sync.go rollout logic will use these counts to control RoleSet updates
	return nil
}

// applyPooledModeCanaryWeight distributes new version across pods within roles based on weight
func (r *StormServiceReconciler) applyPooledModeCanaryWeight(ctx context.Context, stormService *orchestrationv1alpha1.StormService, weight, totalReplicas int32, currentRevision, updateRevision string) error {
	klog.Infof("Pooled mode canary: applying %d%% weight to roles", weight)

	// In pooled mode, calculate canary pod count for each role
	roleCanaryCounts := make(map[string]int32)
	totalCanaryPods := int32(0)

	// Calculate per-role canary pod counts based on weight
	for _, role := range stormService.Spec.Template.Spec.Roles {
		roleReplicas := role.Replicas
		if roleReplicas == nil {
			roleReplicas = ptr.To(int32(1)) // Default to 1 if not specified
		}

		// Calculate how many pods in this role should be on canary
		canaryPods := int32(math.Ceil(float64(*roleReplicas) * float64(weight) / 100.0))
		roleCanaryCounts[role.Name] = canaryPods
		totalCanaryPods += canaryPods

		klog.Infof("Role %s: %d/%d pods on canary version (%d%%)",
			role.Name, canaryPods, *roleReplicas, weight)
	}

	// Update status with per-role canary counts
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.RoleCanaryCounts = roleCanaryCounts
			status.TotalCanaryPods = totalCanaryPods
		})

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return fmt.Errorf("failed to update pooled mode canary counts: %w", err)
	}

	r.EventRecorder.Eventf(stormService, "Normal", "CanaryPooledMode",
		"Applying pooled mode canary: %d%% of pods per role on new version (%d total canary pods)",
		weight, totalCanaryPods)

	// The sync.go rollout logic will use these per-role counts to control pod updates
	return nil
}

// advanceCanaryStep moves to the next step in canary deployment
func (r *StormServiceReconciler) advanceCanaryStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (ctrl.Result, error) {
	nextStep := stormService.Status.CanaryStatus.CurrentStep + 1

	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.CurrentStep = nextStep
			// Remove only CanaryPauseStep conditions, keep other pause reasons
			status.PauseConditions = r.removePauseCondition(status.PauseConditions, orchestrationv1alpha1.PauseReasonCanaryPauseStep)
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

	// Verify that ALL replicas are updated before completing canary
	var totalReplicas int32 = 1
	if stormService.Spec.Replicas != nil {
		totalReplicas = *stormService.Spec.Replicas
	}

	// If revision fields are missing (due to patch issues), use the current updateRevision
	canaryRevision := stormService.Status.UpdateRevision

	// Check if all replicas are actually on the canary revision
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get RoleSets for completion check: %w", err)
	}

	activeRoleSets, _ := filterTerminatingRoleSets(allRoleSets)
	updated, _ := filterRoleSetByRevision(activeRoleSets, canaryRevision)
	readyUpdated, _ := filterReadyRoleSets(updated)

	if int32(len(updated)) < totalReplicas || int32(len(readyUpdated)) < totalReplicas {
		klog.Infof("Canary at 100%% but not all replicas updated yet: %d/%d updated, %d/%d ready. Waiting for full rollout.",
			len(updated), totalReplicas, len(readyUpdated), totalReplicas)
		// Requeue to wait for all replicas to be updated
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	klog.Infof("Completing canary deployment for StormService %s/%s, all %d replicas updated, promoting %s as stable",
		stormService.Namespace, stormService.Name, totalReplicas, canaryRevision)

	// Step 1: Mark canary as completed with 100% weight
	update := newCanaryStatusUpdate().
		addStatusUpdate(func(status *orchestrationv1alpha1.CanaryStatus) {
			status.Phase = orchestrationv1alpha1.CanaryPhaseCompleted
		}).
		addEvent("Canary deployment completed successfully")

	if err := r.applyCanaryStatusUpdate(ctx, stormService, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to complete canary deployment: %w", err)
	}

	// Step 2: Promote canary revision as stable
	// The promoted revision becomes the new current revision
	promotedRevision := canaryRevision

	// Use single patch operation for the final status update
	// Capture the original object BEFORE mutating status to ensure the patch contains the changes
	original := stormService.DeepCopy()

	// Step 3: Update main status to reflect the promotion
	// CurrentRevision becomes the promoted canary revision
	// UpdateRevision stays the same (it's already the canary revision)
	// This indicates the update is complete
	stormService.Status.CurrentRevision = promotedRevision
	// Keep UpdateRevision as-is since it's already set to the canary revision

	// Step 4: Clear canary status - deployment is complete
	stormService.Status.CanaryStatus = nil

	if err := r.Status().Patch(ctx, stormService, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to complete canary and promote revision: %w", err)
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

// removePauseCondition removes a specific pause condition from the list
func (r *StormServiceReconciler) removePauseCondition(conditions []orchestrationv1alpha1.PauseCondition, reason orchestrationv1alpha1.PauseReason) []orchestrationv1alpha1.PauseCondition {
	if len(conditions) == 0 {
		return nil
	}

	var filtered []orchestrationv1alpha1.PauseCondition
	for _, condition := range conditions {
		if condition.Reason != reason {
			filtered = append(filtered, condition)
		}
	}
	return filtered
}

// getPausedAt returns when the canary was paused based on the first pause condition
func (r *StormServiceReconciler) getPausedAt(canaryStatus *orchestrationv1alpha1.CanaryStatus) *metav1.Time {
	if canaryStatus == nil || len(canaryStatus.PauseConditions) == 0 {
		return nil
	}
	// Return the start time of the first pause condition
	return &canaryStatus.PauseConditions[0].StartTime
}

// getCurrentWeight calculates the current weight from the canary steps
func (r *StormServiceReconciler) getCurrentWeight(stormService *orchestrationv1alpha1.StormService) int32 {
	if stormService.Status.CanaryStatus == nil {
		return 0
	}

	canaryStatus := stormService.Status.CanaryStatus
	if stormService.Spec.UpdateStrategy.Canary == nil {
		return 0
	}

	steps := stormService.Spec.UpdateStrategy.Canary.Steps

	// Find the last setWeight step before or at the current step
	var currentWeight int32 = 0
	for i := int32(0); i <= canaryStatus.CurrentStep && i < int32(len(steps)); i++ {
		if steps[i].SetWeight != nil {
			currentWeight = *steps[i].SetWeight
		}
	}

	return currentWeight
}

// calculateAchievableCanaryReplicas calculates how many canary replicas can actually be achieved
// considering rollout constraints (maxUnavailable/maxSurge)
// This uses READY replicas for constraint calculation, not just created replicas
func (r *StormServiceReconciler) calculateAchievableCanaryReplicas(
	ss *orchestrationv1alpha1.StormService,
	allRoleSets []*orchestrationv1alpha1.RoleSet,
	updateRevision string,
	desired int32,
) int32 {
	var total int32 = 1
	if ss.Spec.Replicas != nil {
		total = *ss.Spec.Replicas
	}

	active, _ := filterTerminatingRoleSets(allRoleSets)
	updated, _ := filterRoleSetByRevision(active, updateRevision)
	currentUpdated := int32(len(updated))

	// maxUnavailable
	maxUnavail := int32(1)
	if ss.Spec.UpdateStrategy.MaxUnavailable != nil {
		v, _ := intstr.GetScaledValueFromIntOrPercent(ss.Spec.UpdateStrategy.MaxUnavailable, int(total), false)
		maxUnavail = int32(v)
	}
	if maxUnavail < 0 {
		maxUnavail = 0
	}

	// maxSurge
	maxSurge := int32(0)
	if ss.Spec.UpdateStrategy.MaxSurge != nil {
		v, _ := intstr.GetScaledValueFromIntOrPercent(ss.Spec.UpdateStrategy.MaxSurge, int(total), true)
		maxSurge = int32(v)
	}
	if maxSurge < 0 {
		maxSurge = 0
	}

	var achievable int32
	if desired >= currentUpdated {
		// going UP: bounded by surge
		upper := currentUpdated + maxSurge
		if upper > total {
			upper = total
		}
		if desired < upper {
			achievable = desired
		} else {
			achievable = upper
		}
	} else {
		// going DOWN: bounded by unavailability (how many updated we can delete safely in this tick)
		lower := currentUpdated - maxUnavail
		if lower < 0 {
			lower = 0
		}
		if desired > lower {
			achievable = desired
		} else {
			achievable = lower
		}
	}

	if achievable < 0 {
		achievable = 0
	}
	if achievable > total {
		achievable = total
	}

	klog.Infof("Canary constraint calc: desired=%d currentUpdated=%d total=%d maxSurge=%d maxUnavail=%d => achievable=%d",
		desired, currentUpdated, total, maxSurge, maxUnavail, achievable)
	return achievable
}

// isCanaryTargetAchieved checks if the current canary target has been achieved
// Returns (achieved, requeue_interval)
func (r *StormServiceReconciler) isCanaryTargetAchieved(
	ctx context.Context, ss *orchestrationv1alpha1.StormService, updateRevision string,
) (bool, time.Duration) {
	cs := ss.Status.CanaryStatus
	if cs == nil {
		return false, 5 * time.Second
	}

	all, err := r.getRoleSetList(ctx, ss.Spec.Selector)
	if err != nil {
		klog.Errorf("getRoleSetList failed: %v", err)
		return false, 10 * time.Second
	}
	active, _ := filterTerminatingRoleSets(all)

	updated, _ := filterRoleSetByRevision(active, updateRevision)
	currentUpdated := int32(len(updated))
	readyUpdatedList, _ := filterReadyRoleSets(updated)
	readyUpdated := int32(len(readyUpdatedList))

	var total int32 = 1
	if ss.Spec.Replicas != nil {
		total = *ss.Spec.Replicas
	}
	weight := r.getCurrentWeight(ss)
	desired := int32(math.Ceil(float64(total) * float64(weight) / 100.0))

	klog.Infof("Canary target check: desired=%d currentUpdated=%d readyUpdated=%d (weight=%d%%, total=%d)",
		desired, currentUpdated, readyUpdated, weight, total)

	achieved := (currentUpdated >= desired) && (readyUpdated >= desired)
	if achieved {
		return true, 0
	}
	return false, 10 * time.Second
}
