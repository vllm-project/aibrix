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
	"crypto/sha256"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

const (
	// RolloutApprovalAnnotation is used to manually approve a paused rollout
	RolloutApprovalAnnotation = "stormservice.aibrix.ai/rollout-approval"

	// RolloutAbortAnnotation is used to manually abort a rollout
	RolloutAbortAnnotation = "stormservice.aibrix.ai/rollout-abort"

	// RolloutRetryAnnotation is used to retry a failed rollout
	RolloutRetryAnnotation = "stormservice.aibrix.ai/rollout-retry"

	// RolloutPauseAnnotation is used to pause a rollout at current step
	RolloutPauseAnnotation = "stormservice.aibrix.ai/rollout-pause"

	// RolloutResumeAnnotation is used to resume a paused rollout
	RolloutResumeAnnotation = "stormservice.aibrix.ai/rollout-resume"
)

// RolloutManager manages advanced rollout strategies for StormService
type RolloutManager struct {
	reconciler *StormServiceReconciler
}

// NewRolloutManager creates a new RolloutManager
func NewRolloutManager(reconciler *StormServiceReconciler) *RolloutManager {
	return &RolloutManager{
		reconciler: reconciler,
	}
}

// ProcessRollout handles the rollout process based on the configured strategy
func (rm *RolloutManager) ProcessRollout(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	// Initialize rollout status if not present
	if stormService.Status.RolloutStatus == nil {
		stormService.Status.RolloutStatus = &orchestrationv1alpha1.RolloutStatus{
			Phase: orchestrationv1alpha1.RolloutPhaseHealthy,
		}
	}

	// Check if rollout strategy is configured
	if stormService.Spec.RolloutStrategy == nil {
		// No advanced rollout strategy, use standard rolling update
		return rm.reconciler.rollout(ctx, stormService, currentRevision, updateRevision)
	}

	// Handle manual controls first
	if err := rm.handleManualControls(ctx, stormService); err != nil {
		return err
	}

	// Process based on rollout strategy type
	switch stormService.Spec.RolloutStrategy.Type {
	case orchestrationv1alpha1.CanaryRolloutStrategyType:
		return rm.processCanaryRollout(ctx, stormService, currentRevision, updateRevision)
	case orchestrationv1alpha1.BlueGreenRolloutStrategyType:
		return rm.processBlueGreenRollout(ctx, stormService, currentRevision, updateRevision)
	default:
		// Default to standard rolling update if strategy type not recognized
		return rm.reconciler.rollout(ctx, stormService, currentRevision, updateRevision)
	}
}

// handleManualControls processes manual rollout controls via annotations
func (rm *RolloutManager) handleManualControls(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	annotations := stormService.GetAnnotations()
	if annotations == nil {
		return nil
	}

	// Handle approval annotation
	if _, exists := annotations[RolloutApprovalAnnotation]; exists {
		if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhasePaused {
			klog.Infof("Manual approval received for StormService %s/%s, resuming rollout", stormService.Namespace, stormService.Name)
			rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutProgressing, corev1.ConditionTrue, "ManualApproval", "Rollout resumed via manual approval")
			stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseProgressing
			stormService.Status.RolloutStatus.PauseStartTime = nil
		}
		// Remove the annotation after processing
		delete(annotations, RolloutApprovalAnnotation)
		stormService.SetAnnotations(annotations)
	}

	// Handle abort annotation
	if _, exists := annotations[RolloutAbortAnnotation]; exists {
		klog.Infof("Manual abort received for StormService %s/%s, aborting rollout", stormService.Namespace, stormService.Name)
		rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutDegraded, corev1.ConditionTrue, "ManualAbort", "Rollout aborted via manual intervention")
		stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseAborted
		// Remove the annotation after processing
		delete(annotations, RolloutAbortAnnotation)
		stormService.SetAnnotations(annotations)
		return rm.initiateRollback(ctx, stormService, "Manual abort")
	}

	// Handle pause annotation
	if _, exists := annotations[RolloutPauseAnnotation]; exists {
		if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhaseProgressing {
			klog.Infof("Manual pause received for StormService %s/%s, pausing rollout", stormService.Namespace, stormService.Name)
			rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutPaused, corev1.ConditionTrue, "ManualPause", "Rollout paused via manual intervention")
			stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhasePaused
			now := metav1.Now()
			stormService.Status.RolloutStatus.PauseStartTime = &now
		}
		// Remove the annotation after processing
		delete(annotations, RolloutPauseAnnotation)
		stormService.SetAnnotations(annotations)
	}

	// Handle resume annotation
	if _, exists := annotations[RolloutResumeAnnotation]; exists {
		if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhasePaused {
			klog.Infof("Manual resume received for StormService %s/%s, resuming rollout", stormService.Namespace, stormService.Name)
			rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutProgressing, corev1.ConditionTrue, "ManualResume", "Rollout resumed via manual intervention")
			stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseProgressing
			stormService.Status.RolloutStatus.PauseStartTime = nil
		}
		// Remove the annotation after processing
		delete(annotations, RolloutResumeAnnotation)
		stormService.SetAnnotations(annotations)
	}

	return nil
}

// processCanaryRollout handles canary deployment strategy
func (rm *RolloutManager) processCanaryRollout(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	canaryConfig := stormService.Spec.RolloutStrategy.Canary
	if canaryConfig == nil {
		return fmt.Errorf("canary configuration is missing")
	}

	// Initialize canary status if not present
	if stormService.Status.RolloutStatus.CanaryStatus == nil {
		stormService.Status.RolloutStatus.CanaryStatus = &orchestrationv1alpha1.CanaryStatus{
			StableRS: currentRevision,
			CanaryRS: updateRevision,
		}
	}

	// Check if rollout is paused
	if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhasePaused {
		return rm.handlePausedState(ctx, stormService)
	}

	// Check if rollout is completed
	if currentRevision == updateRevision {
		return rm.completeRollout(ctx, stormService)
	}

	// Process canary steps
	if len(canaryConfig.Steps) == 0 {
		// No steps defined, proceed with standard rolling update
		return rm.reconciler.rollout(ctx, stormService, currentRevision, updateRevision)
	}

	return rm.executeCanaryStep(ctx, stormService, currentRevision, updateRevision)
}

// executeCanaryStep executes the current canary step
func (rm *RolloutManager) executeCanaryStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	canaryConfig := stormService.Spec.RolloutStrategy.Canary
	currentStepIndex := rm.getCurrentStepIndex(stormService)

	if currentStepIndex >= int32(len(canaryConfig.Steps)) {
		// All steps completed, promote canary
		return rm.promoteCanary(ctx, stormService, currentRevision, updateRevision)
	}

	currentStep := canaryConfig.Steps[currentStepIndex]

	// Check if step changed (step hash comparison)
	stepHash := rm.calculateStepHash(currentStep)
	if stormService.Status.RolloutStatus.CurrentStepHash != stepHash {
		klog.Infof("Executing canary step %d for StormService %s/%s", currentStepIndex, stormService.Namespace, stormService.Name)
		stormService.Status.RolloutStatus.CurrentStepIndex = &currentStepIndex
		stormService.Status.RolloutStatus.CurrentStepHash = stepHash
		stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseProgressing
	}

	// Handle step pause
	if currentStep.Pause != nil {
		return rm.handleStepPause(ctx, stormService, currentStep.Pause)
	}

	// Execute step weight or replica change
	if err := rm.applyCanaryStep(ctx, stormService, currentStep, currentRevision, updateRevision); err != nil {
		return err
	}

	// Run analysis if configured
	if currentStep.Analysis != nil {
		if err := rm.runAnalysis(ctx, stormService, currentStep.Analysis); err != nil {
			return err
		}
	}

	// Move to next step if current step is complete
	if rm.isStepComplete(ctx, stormService, currentStep) {
		nextStepIndex := currentStepIndex + 1
		stormService.Status.RolloutStatus.CurrentStepIndex = &nextStepIndex
		rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutProgressing, corev1.ConditionTrue, "StepComplete", fmt.Sprintf("Step %d completed successfully", currentStepIndex))
	}

	return nil
}

// processBlueGreenRollout handles blue-green deployment strategy
func (rm *RolloutManager) processBlueGreenRollout(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	blueGreenConfig := stormService.Spec.RolloutStrategy.BlueGreen
	if blueGreenConfig == nil {
		return fmt.Errorf("blue-green configuration is missing")
	}

	// Initialize blue-green status if not present
	if stormService.Status.RolloutStatus.BlueGreenStatus == nil {
		stormService.Status.RolloutStatus.BlueGreenStatus = &orchestrationv1alpha1.BlueGreenStatus{
			ActiveSelector:  currentRevision,
			PreviewSelector: updateRevision,
		}
	}

	// Check if rollout is paused
	if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhasePaused {
		return rm.handlePausedState(ctx, stormService)
	}

	// Check if rollout is completed
	if currentRevision == updateRevision {
		return rm.completeRollout(ctx, stormService)
	}

	return rm.executeBlueGreenStep(ctx, stormService, currentRevision, updateRevision)
}

// executeBlueGreenStep executes blue-green deployment logic
func (rm *RolloutManager) executeBlueGreenStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	blueGreenConfig := stormService.Spec.RolloutStrategy.BlueGreen

	// Create preview environment (green)
	if err := rm.createPreviewEnvironment(ctx, stormService, updateRevision); err != nil {
		return err
	}

	// Run pre-promotion analysis if configured
	if blueGreenConfig.PrePromotionAnalysis != nil {
		if err := rm.runAnalysis(ctx, stormService, blueGreenConfig.PrePromotionAnalysis); err != nil {
			return err
		}
	}

	// Check if preview environment is ready for promotion
	if rm.isPreviewEnvironmentReady(ctx, stormService, updateRevision) {
		if blueGreenConfig.AutoPromotionEnabled {
			return rm.promoteBlueGreen(ctx, stormService, currentRevision, updateRevision)
		} else {
			// Pause for manual promotion
			return rm.pauseForManualPromotion(ctx, stormService)
		}
	}

	return nil
}

// Helper methods for rollout management

func (rm *RolloutManager) getCurrentStepIndex(stormService *orchestrationv1alpha1.StormService) int32 {
	if stormService.Status.RolloutStatus.CurrentStepIndex == nil {
		return 0
	}
	return *stormService.Status.RolloutStatus.CurrentStepIndex
}

func (rm *RolloutManager) calculateStepHash(step orchestrationv1alpha1.CanaryStep) string {
	hash := sha256.New()
	if step.Weight != nil {
		hash.Write([]byte(fmt.Sprintf("weight:%d", *step.Weight)))
	}
	if step.Replicas != nil {
		hash.Write([]byte(fmt.Sprintf("replicas:%d", *step.Replicas)))
	}
	if step.Pause != nil {
		if step.Pause.Duration != nil {
			hash.Write([]byte(fmt.Sprintf("pause-duration:%s", step.Pause.Duration.String())))
		}
		hash.Write([]byte(fmt.Sprintf("pause-approval:%t", step.Pause.UntilApproval)))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))[:8]
}

func (rm *RolloutManager) setRolloutCondition(stormService *orchestrationv1alpha1.StormService, condType orchestrationv1alpha1.RolloutConditionType, status corev1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	condition := orchestrationv1alpha1.RolloutCondition{
		Type:               condType,
		Status:             status,
		LastUpdateTime:     now,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}

	// Update existing condition or add new one
	conditions := stormService.Status.RolloutStatus.Conditions
	for i, existing := range conditions {
		if existing.Type == condType {
			if existing.Status != status {
				condition.LastTransitionTime = now
			} else {
				condition.LastTransitionTime = existing.LastTransitionTime
			}
			conditions[i] = condition
			return
		}
	}
	conditions = append(conditions, condition)
	stormService.Status.RolloutStatus.Conditions = conditions
}

func (rm *RolloutManager) handlePausedState(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	// Check for timeout if pause duration is set
	if stormService.Status.RolloutStatus.PauseStartTime != nil {
		// Implementation for pause timeout handling
		// This would check if the pause duration has expired
	}
	return nil
}

func (rm *RolloutManager) completeRollout(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutCompleted, corev1.ConditionTrue, "RolloutComplete", "Rollout completed successfully")
	stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseCompleted
	stormService.Status.RolloutStatus.CurrentStepIndex = nil
	stormService.Status.RolloutStatus.CurrentStepHash = ""
	stormService.Status.RolloutStatus.PauseStartTime = nil
	return nil
}

func (rm *RolloutManager) initiateRollback(ctx context.Context, stormService *orchestrationv1alpha1.StormService, reason string) error {
	klog.Infof("Initiating rollback for StormService %s/%s, reason: %s", stormService.Namespace, stormService.Name, reason)

	// Set rollback condition
	rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutDegraded, corev1.ConditionTrue, "Rollback", fmt.Sprintf("Rolling back: %s", reason))

	// Reset rollout status
	stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhaseAborted
	stormService.Status.RolloutStatus.CurrentStepIndex = nil
	stormService.Status.RolloutStatus.CurrentStepHash = ""
	stormService.Status.RolloutStatus.PauseStartTime = nil

	// Implement actual rollback logic here
	// This would involve scaling down the new revision and scaling up the stable revision

	return nil
}

// Placeholder methods for complex operations that would need full implementation

func (rm *RolloutManager) handleStepPause(ctx context.Context, stormService *orchestrationv1alpha1.StormService, pauseConfig *orchestrationv1alpha1.PauseConfig) error {
	// Implementation for handling step pause
	if pauseConfig.UntilApproval {
		rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutPaused, corev1.ConditionTrue, "PauseForApproval", "Rollout paused waiting for manual approval")
		stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhasePaused
		if stormService.Status.RolloutStatus.PauseStartTime == nil {
			now := metav1.Now()
			stormService.Status.RolloutStatus.PauseStartTime = &now
		}
	}
	return nil
}

func (rm *RolloutManager) applyCanaryStep(ctx context.Context, stormService *orchestrationv1alpha1.StormService, step orchestrationv1alpha1.CanaryStep, currentRevision, updateRevision string) error {
	// Implementation for applying canary step (traffic weight, replica changes)
	if step.Weight != nil {
		// Update traffic routing weight
		stormService.Status.RolloutStatus.CanaryStatus.CurrentWeight = *step.Weight

		// Apply traffic routing
		trafficRouter := NewTrafficRouter(rm.reconciler)
		if err := trafficRouter.UpdateTrafficWeights(ctx, stormService, *step.Weight); err != nil {
			return fmt.Errorf("failed to update traffic weights: %w", err)
		}

		klog.Infof("Updated traffic weight to %d%% for canary StormService %s/%s", *step.Weight, stormService.Namespace, stormService.Name)
	}

	// Apply replica changes if specified
	if step.Replicas != nil {
		// This would involve scaling the canary replica set to the specified number
		klog.Infof("Scaling canary replicas to %d for StormService %s/%s", *step.Replicas, stormService.Namespace, stormService.Name)
		// Implementation would call the scaling logic here
	}

	// Run health checks after applying changes
	return rm.checkHealthAndRollback(ctx, stormService)
}

func (rm *RolloutManager) shouldAutoRollback(stormService *orchestrationv1alpha1.StormService, triggerType string) bool {
	if stormService.Spec.RolloutStrategy == nil || stormService.Spec.RolloutStrategy.AutoRollback == nil {
		return false
	}

	autoRollback := stormService.Spec.RolloutStrategy.AutoRollback
	if !autoRollback.Enabled || autoRollback.OnFailure == nil {
		return false
	}

	switch triggerType {
	case "analysis_failure":
		return autoRollback.OnFailure.AnalysisFailure
	case "health_check_failure":
		return autoRollback.OnFailure.HealthCheckFailure
	default:
		return false
	}
}

func (rm *RolloutManager) checkHealthAndRollback(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	// Perform health check
	healthChecker := NewHealthChecker(rm.reconciler)
	health, err := healthChecker.CheckRolloutHealth(ctx, stormService)
	if err != nil {
		klog.Errorf("Health check failed for StormService %s/%s: %v", stormService.Namespace, stormService.Name, err)
		return err
	}

	// Update health status in rollout status
	if stormService.Status.RolloutStatus != nil {
		if health.Healthy {
			rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutHealthy, corev1.ConditionTrue, "HealthCheckPassed", health.Message)
		} else {
			rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutHealthy, corev1.ConditionFalse, "HealthCheckFailed", health.Message)
		}
	}

	// Check if rollback should be triggered
	if health.ShouldRollback {
		if rm.shouldAutoRollback(stormService, "health_check_failure") {
			return rm.initiateRollback(ctx, stormService, health.RollbackReason)
		}
	}

	return nil
}

func (rm *RolloutManager) runAnalysis(ctx context.Context, stormService *orchestrationv1alpha1.StormService, analysis *orchestrationv1alpha1.AnalysisStep) error {
	// Implementation for running analysis templates
	analysisRunner := NewAnalysisRunner(rm.reconciler)
	result, err := analysisRunner.RunAnalysis(ctx, stormService, analysis)
	if err != nil {
		klog.Errorf("Analysis failed for StormService %s/%s: %v", stormService.Namespace, stormService.Name, err)

		// Check if auto-rollback is enabled for analysis failure
		if rm.shouldAutoRollback(stormService, "analysis_failure") {
			return rm.initiateRollback(ctx, stormService, fmt.Sprintf("Analysis failed: %v", err))
		}
		return err
	}

	if !result.Successful {
		klog.Warningf("Analysis unsuccessful for StormService %s/%s: %s", stormService.Namespace, stormService.Name, result.Message)

		// Check if auto-rollback is enabled for analysis failure
		if rm.shouldAutoRollback(stormService, "analysis_failure") {
			return rm.initiateRollback(ctx, stormService, fmt.Sprintf("Analysis unsuccessful: %s", result.Message))
		}
		return fmt.Errorf("analysis unsuccessful: %s", result.Message)
	}

	klog.Infof("Analysis completed successfully for StormService %s/%s", stormService.Namespace, stormService.Name)
	return nil
}

func (rm *RolloutManager) isStepComplete(ctx context.Context, stormService *orchestrationv1alpha1.StormService, step orchestrationv1alpha1.CanaryStep) bool {
	// Check if traffic routing is properly applied
	if step.Weight != nil {
		if stormService.Status.RolloutStatus.CanaryStatus == nil {
			return false
		}
		currentWeight := stormService.Status.RolloutStatus.CanaryStatus.CurrentWeight
		if currentWeight != *step.Weight {
			return false
		}
	}

	// Check if replicas are properly scaled
	if step.Replicas != nil {
		// This would check if the canary replica set has the desired number of ready replicas
		// For now, we'll assume it's ready after traffic routing is applied
	}

	// Check if pause duration has elapsed (if applicable)
	if step.Pause != nil && step.Pause.Duration != nil {
		if stormService.Status.RolloutStatus.PauseStartTime != nil {
			elapsed := time.Since(stormService.Status.RolloutStatus.PauseStartTime.Time)
			if elapsed < step.Pause.Duration.Duration {
				return false // Still in pause period
			}
		}
	}

	// If waiting for approval, step is not complete until approval is given
	if step.Pause != nil && step.Pause.UntilApproval {
		if stormService.Status.RolloutStatus.Phase == orchestrationv1alpha1.RolloutPhasePaused {
			return false // Still waiting for approval
		}
	}

	// Step is complete if all conditions are met
	return true
}

func (rm *RolloutManager) promoteCanary(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	// Implementation for promoting canary to stable
	klog.Infof("Promoting canary for StormService %s/%s", stormService.Namespace, stormService.Name)
	return rm.completeRollout(ctx, stormService)
}

func (rm *RolloutManager) createPreviewEnvironment(ctx context.Context, stormService *orchestrationv1alpha1.StormService, updateRevision string) error {
	// Implementation for creating blue-green preview environment
	return nil
}

func (rm *RolloutManager) isPreviewEnvironmentReady(ctx context.Context, stormService *orchestrationv1alpha1.StormService, updateRevision string) bool {
	// Implementation for checking if preview environment is ready
	return true
}

func (rm *RolloutManager) promoteBlueGreen(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision string) error {
	// Implementation for blue-green promotion
	klog.Infof("Promoting blue-green deployment for StormService %s/%s", stormService.Namespace, stormService.Name)
	return rm.completeRollout(ctx, stormService)
}

func (rm *RolloutManager) pauseForManualPromotion(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	// Implementation for pausing for manual promotion
	rm.setRolloutCondition(stormService, orchestrationv1alpha1.RolloutPaused, corev1.ConditionTrue, "PauseForPromotion", "Rollout paused waiting for manual promotion")
	stormService.Status.RolloutStatus.Phase = orchestrationv1alpha1.RolloutPhasePaused
	if stormService.Status.RolloutStatus.PauseStartTime == nil {
		now := metav1.Now()
		stormService.Status.RolloutStatus.PauseStartTime = &now
	}
	return nil
}
