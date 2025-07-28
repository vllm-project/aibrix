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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// HealthChecker provides health checking capabilities for rollouts
type HealthChecker struct {
	reconciler *StormServiceReconciler
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(reconciler *StormServiceReconciler) *HealthChecker {
	return &HealthChecker{
		reconciler: reconciler,
	}
}

// CheckRolloutHealth evaluates the health of the current rollout
func (hc *HealthChecker) CheckRolloutHealth(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (*HealthStatus, error) {
	// Get all role sets for the storm service
	allRoleSets, err := hc.reconciler.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("failed to get role sets: %w", err)
	}

	// Analyze health based on ready replicas and error rates
	health := &HealthStatus{
		Healthy:     true,
		Message:     "All replicas are healthy",
		CheckTime:   metav1.Now(),
		ErrorRate:   0.0,
		SuccessRate: 100.0,
	}

	// Check replica health
	ready, notReady := filterReadyRoleSets(allRoleSets)
	totalReplicas := len(allRoleSets)
	readyReplicas := len(ready)

	if totalReplicas > 0 {
		health.SuccessRate = float64(readyReplicas) / float64(totalReplicas) * 100
		health.ErrorRate = float64(len(notReady)) / float64(totalReplicas) * 100

		// Define health thresholds
		if health.SuccessRate < 90.0 {
			health.Healthy = false
			health.Message = fmt.Sprintf("Health degraded: only %d/%d replicas ready (%.1f%% success rate)", readyReplicas, totalReplicas, health.SuccessRate)
		} else if health.SuccessRate < 95.0 {
			health.Message = fmt.Sprintf("Health warning: %d/%d replicas ready (%.1f%% success rate)", readyReplicas, totalReplicas, health.SuccessRate)
		}
	}

	// Check for rollout-specific health issues
	if stormService.Status.RolloutStatus != nil {
		if err := hc.checkRolloutSpecificHealth(ctx, stormService, health); err != nil {
			return nil, err
		}
	}

	return health, nil
}

// checkRolloutSpecificHealth checks health specific to rollout type
func (hc *HealthChecker) checkRolloutSpecificHealth(ctx context.Context, stormService *orchestrationv1alpha1.StormService, health *HealthStatus) error {
	rolloutStatus := stormService.Status.RolloutStatus

	// Check for timeout conditions
	if rolloutStatus.PauseStartTime != nil {
		pauseDuration := time.Since(rolloutStatus.PauseStartTime.Time)

		// Default timeout of 30 minutes for paused rollouts
		timeoutDuration := 30 * time.Minute
		if stormService.Spec.RolloutStrategy != nil && stormService.Spec.RolloutStrategy.AutoRollback != nil && stormService.Spec.RolloutStrategy.AutoRollback.OnTimeout != nil {
			timeoutDuration = stormService.Spec.RolloutStrategy.AutoRollback.OnTimeout.Duration
		}

		if pauseDuration > timeoutDuration {
			health.Healthy = false
			health.Message = fmt.Sprintf("Rollout timed out after %v in paused state", pauseDuration)
		}
	}

	// Check auto-rollback conditions
	if stormService.Spec.RolloutStrategy != nil && stormService.Spec.RolloutStrategy.AutoRollback != nil {
		autoRollback := stormService.Spec.RolloutStrategy.AutoRollback

		if autoRollback.Enabled && autoRollback.OnFailure != nil {
			if autoRollback.OnFailure.HealthCheckFailure && !health.Healthy {
				health.ShouldRollback = true
				health.RollbackReason = "Health check failure detected"
			}

			// Check error threshold
			if autoRollback.OnFailure.ErrorThreshold != nil {
				// This would require integration with metrics systems
				// For now, we'll use the replica health as a proxy
				if health.ErrorRate > 10.0 { // 10% error threshold as default
					health.ShouldRollback = true
					health.RollbackReason = fmt.Sprintf("Error rate %.1f%% exceeds threshold", health.ErrorRate)
				}
			}
		}
	}

	return nil
}

// HealthStatus represents the health status of a rollout
type HealthStatus struct {
	Healthy        bool        `json:"healthy"`
	Message        string      `json:"message"`
	CheckTime      metav1.Time `json:"checkTime"`
	ErrorRate      float64     `json:"errorRate"`
	SuccessRate    float64     `json:"successRate"`
	ShouldRollback bool        `json:"shouldRollback"`
	RollbackReason string      `json:"rollbackReason,omitempty"`
}

// AnalysisRunner provides analysis execution capabilities
type AnalysisRunner struct {
	reconciler *StormServiceReconciler
}

// NewAnalysisRunner creates a new analysis runner
func NewAnalysisRunner(reconciler *StormServiceReconciler) *AnalysisRunner {
	return &AnalysisRunner{
		reconciler: reconciler,
	}
}

// RunAnalysis executes the specified analysis step
func (ar *AnalysisRunner) RunAnalysis(ctx context.Context, stormService *orchestrationv1alpha1.StormService, analysis *orchestrationv1alpha1.AnalysisStep) (*AnalysisResult, error) {
	klog.Infof("Running analysis for StormService %s/%s with %d templates", stormService.Namespace, stormService.Name, len(analysis.Templates))

	result := &AnalysisResult{
		Successful: true,
		Message:    "Analysis completed successfully",
		StartTime:  metav1.Now(),
		Phase:      "Running",
		Results:    make(map[string]interface{}),
	}

	// Execute each analysis template
	for _, template := range analysis.Templates {
		templateResult, err := ar.executeAnalysisTemplate(ctx, stormService, template, analysis.Args)
		if err != nil {
			result.Successful = false
			result.Message = fmt.Sprintf("Analysis template %s failed: %v", template.Name, err)
			result.Phase = "Failed"
			return result, err
		}

		result.Results[template.Name] = templateResult
	}

	result.Phase = "Successful"
	result.FinishTime = &metav1.Time{Time: time.Now()}

	return result, nil
}

// executeAnalysisTemplate executes a single analysis template
func (ar *AnalysisRunner) executeAnalysisTemplate(ctx context.Context, stormService *orchestrationv1alpha1.StormService, template orchestrationv1alpha1.AnalysisTemplateRef, args []orchestrationv1alpha1.AnalysisArg) (interface{}, error) {
	// This is a placeholder implementation
	// In a real implementation, this would:
	// 1. Fetch the AnalysisTemplate from the cluster
	// 2. Execute the defined metrics queries (Prometheus, Datadog, etc.)
	// 3. Evaluate success criteria
	// 4. Return results

	klog.Infof("Executing analysis template %s for StormService %s/%s", template.Name, stormService.Namespace, stormService.Name)

	// Simulate analysis execution
	time.Sleep(100 * time.Millisecond)

	// Mock successful result
	return map[string]interface{}{
		"success_rate": 99.5,
		"error_rate":   0.5,
		"latency_p95":  150.0,
		"status":       "pass",
	}, nil
}

// AnalysisResult represents the result of an analysis execution
type AnalysisResult struct {
	Successful bool                   `json:"successful"`
	Message    string                 `json:"message"`
	StartTime  metav1.Time            `json:"startTime"`
	FinishTime *metav1.Time           `json:"finishTime,omitempty"`
	Phase      string                 `json:"phase"`
	Results    map[string]interface{} `json:"results"`
}

// TrafficRouter provides traffic routing capabilities for canary deployments
type TrafficRouter struct {
	reconciler *StormServiceReconciler
}

// NewTrafficRouter creates a new traffic router
func NewTrafficRouter(reconciler *StormServiceReconciler) *TrafficRouter {
	return &TrafficRouter{
		reconciler: reconciler,
	}
}

// UpdateTrafficWeights updates traffic routing weights for canary deployment
func (tr *TrafficRouter) UpdateTrafficWeights(ctx context.Context, stormService *orchestrationv1alpha1.StormService, canaryWeight int32) error {
	if stormService.Spec.RolloutStrategy == nil || stormService.Spec.RolloutStrategy.Canary == nil {
		return fmt.Errorf("canary configuration not found")
	}

	canaryConfig := stormService.Spec.RolloutStrategy.Canary

	klog.Infof("Updating traffic weights for StormService %s/%s: canary=%d%%, stable=%d%%",
		stormService.Namespace, stormService.Name, canaryWeight, 100-canaryWeight)

	// Route traffic based on configured traffic routing
	if canaryConfig.TrafficRouting != nil {
		if canaryConfig.TrafficRouting.Nginx != nil {
			return tr.updateNginxTrafficWeights(ctx, stormService, canaryConfig.TrafficRouting.Nginx, canaryWeight)
		}
		if canaryConfig.TrafficRouting.Istio != nil {
			return tr.updateIstioTrafficWeights(ctx, stormService, canaryConfig.TrafficRouting.Istio, canaryWeight)
		}
		if canaryConfig.TrafficRouting.SMI != nil {
			return tr.updateSMITrafficWeights(ctx, stormService, canaryConfig.TrafficRouting.SMI, canaryWeight)
		}
	}

	// Default service-based routing
	return tr.updateServiceTrafficWeights(ctx, stormService, canaryConfig, canaryWeight)
}

// updateNginxTrafficWeights updates nginx ingress for traffic splitting
func (tr *TrafficRouter) updateNginxTrafficWeights(ctx context.Context, stormService *orchestrationv1alpha1.StormService, nginxConfig *orchestrationv1alpha1.NginxTrafficRouting, canaryWeight int32) error {
	// This would update nginx ingress annotations for traffic splitting
	// nginx.ingress.kubernetes.io/canary: "true"
	// nginx.ingress.kubernetes.io/canary-weight: "10"
	klog.Infof("Updating nginx traffic weights for ingress %s: %d%%", nginxConfig.Ingress, canaryWeight)
	return nil
}

// updateIstioTrafficWeights updates Istio VirtualService for traffic splitting
func (tr *TrafficRouter) updateIstioTrafficWeights(ctx context.Context, stormService *orchestrationv1alpha1.StormService, istioConfig *orchestrationv1alpha1.IstioTrafficRouting, canaryWeight int32) error {
	// This would update Istio VirtualService resources
	klog.Infof("Updating Istio traffic weights for VirtualService %s: %d%%", istioConfig.VirtualService, canaryWeight)
	return nil
}

// updateSMITrafficWeights updates SMI TrafficSplit for traffic splitting
func (tr *TrafficRouter) updateSMITrafficWeights(ctx context.Context, stormService *orchestrationv1alpha1.StormService, smiConfig *orchestrationv1alpha1.SMITrafficRouting, canaryWeight int32) error {
	// This would update SMI TrafficSplit resources
	klog.Infof("Updating SMI traffic weights for TrafficSplit %s: %d%%", smiConfig.TrafficSplit, canaryWeight)
	return nil
}

// updateServiceTrafficWeights updates service selector for basic traffic routing
func (tr *TrafficRouter) updateServiceTrafficWeights(ctx context.Context, stormService *orchestrationv1alpha1.StormService, canaryConfig *orchestrationv1alpha1.CanaryStrategy, canaryWeight int32) error {
	// This would update service selectors to route traffic between stable and canary
	klog.Infof("Updating service traffic weights: canary=%d%%, stable=%d%%", canaryWeight, 100-canaryWeight)
	return nil
}
