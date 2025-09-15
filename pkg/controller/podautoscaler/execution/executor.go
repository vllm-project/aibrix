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

package execution

import (
	"context"
	"fmt"
	"time"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ScaleExecutor handles the actual scaling execution
type ScaleExecutor interface {
	// ExecuteScale applies the scaling recommendation to the target resource
	ExecuteScale(ctx context.Context, request ExecutionRequest) (*ExecutionResult, error)

	// ValidateScale checks if scaling is valid before execution
	ValidateScale(ctx context.Context, request ExecutionRequest) error

	// GetExecutorType returns the executor type
	GetExecutorType() string
}

// ExecutionRequest contains the scaling decision to execute
type ExecutionRequest struct {
	Target          types.ScaleTarget
	CurrentReplicas int32
	Recommendation  *algorithm.ScalingRecommendation
	DryRun          bool
	Timestamp       time.Time
}

// ExecutionResult contains the outcome of the scaling execution
type ExecutionResult struct {
	Success          bool
	ActualReplicas   int32
	PreviousReplicas int32
	Error            error
	ExecutionTime    time.Duration
	Metadata         map[string]interface{}
}

// DefaultScaleExecutor implements standard Kubernetes scaling
type DefaultScaleExecutor struct {
	client client.Client
}

// NewDefaultScaleExecutor creates a new default executor
func NewDefaultScaleExecutor(client client.Client) *DefaultScaleExecutor {
	return &DefaultScaleExecutor{
		client: client,
	}
}

// ValidateScale validates the scaling request before execution
func (e *DefaultScaleExecutor) ValidateScale(ctx context.Context, request ExecutionRequest) error {
	// Check if recommendation is valid
	if request.Recommendation == nil {
		return fmt.Errorf("no scaling recommendation provided")
	}

	if !request.Recommendation.ScaleValid {
		return fmt.Errorf("scaling recommendation is not valid: %s", request.Recommendation.Reason)
	}

	// Check if target exists
	target := &unstructured.Unstructured{}
	target.SetAPIVersion(request.Target.APIVersion)
	target.SetKind(request.Target.Kind)

	key := client.ObjectKey{
		Namespace: request.Target.Namespace,
		Name:      request.Target.Name,
	}

	if err := e.client.Get(ctx, key, target); err != nil {
		return fmt.Errorf("failed to get target resource: %w", err)
	}

	// Validate current replicas match
	currentReplicasInt64, found, err := unstructured.NestedInt64(target.Object, "spec", "replicas")
	if !found || err != nil {
		return fmt.Errorf("failed to get current replicas from target: %w", err)
	}

	if int32(currentReplicasInt64) != request.CurrentReplicas {
		klog.V(4).InfoS("Current replicas mismatch",
			"expected", request.CurrentReplicas,
			"actual", currentReplicasInt64)
	}

	return nil
}

// ExecuteScale performs the actual scaling operation
func (e *DefaultScaleExecutor) ExecuteScale(ctx context.Context, request ExecutionRequest) (*ExecutionResult, error) {
	startTime := time.Now()

	result := &ExecutionResult{
		PreviousReplicas: request.CurrentReplicas,
		Metadata:         make(map[string]interface{}),
	}

	// Validate before execution
	if err := e.ValidateScale(ctx, request); err != nil {
		result.Success = false
		result.Error = err
		return result, err
	}

	// Add recommendation metadata
	result.Metadata["algorithm"] = request.Recommendation.Algorithm
	result.Metadata["confidence"] = request.Recommendation.Confidence
	result.Metadata["reason"] = request.Recommendation.Reason

	// Skip actual scaling if dry run
	if request.DryRun {
		result.Success = true
		result.ActualReplicas = request.Recommendation.DesiredReplicas
		result.ExecutionTime = time.Since(startTime)
		result.Metadata["dry_run"] = true
		klog.V(2).InfoS("Dry run scaling execution",
			"target", fmt.Sprintf("%s/%s", request.Target.Namespace, request.Target.Name),
			"current", request.CurrentReplicas,
			"desired", request.Recommendation.DesiredReplicas)
		return result, nil
	}

	// Get the target resource
	target := &unstructured.Unstructured{}
	target.SetAPIVersion(request.Target.APIVersion)
	target.SetKind(request.Target.Kind)

	key := client.ObjectKey{
		Namespace: request.Target.Namespace,
		Name:      request.Target.Name,
	}

	if err := e.client.Get(ctx, key, target); err != nil {
		result.Success = false
		result.Error = fmt.Errorf("failed to get target for scaling: %w", err)
		return result, result.Error
	}

	// Update replicas
	if err := unstructured.SetNestedField(target.Object, int64(request.Recommendation.DesiredReplicas), "spec", "replicas"); err != nil {
		result.Success = false
		result.Error = fmt.Errorf("failed to set replicas field: %w", err)
		return result, result.Error
	}

	// Apply the update
	if err := e.client.Update(ctx, target); err != nil {
		result.Success = false
		result.Error = fmt.Errorf("failed to update target resource: %w", err)
		return result, result.Error
	}

	result.Success = true
	result.ActualReplicas = request.Recommendation.DesiredReplicas
	result.ExecutionTime = time.Since(startTime)

	klog.InfoS("Successfully executed scaling",
		"target", fmt.Sprintf("%s/%s", request.Target.Namespace, request.Target.Name),
		"previous", request.CurrentReplicas,
		"actual", result.ActualReplicas,
		"duration", result.ExecutionTime)

	return result, nil
}

// GetExecutorType returns the executor type
func (e *DefaultScaleExecutor) GetExecutorType() string {
	return "default"
}

// NoOpScaleExecutor is a no-op executor for testing
type NoOpScaleExecutor struct{}

// NewNoOpScaleExecutor creates a new no-op executor
func NewNoOpScaleExecutor() *NoOpScaleExecutor {
	return &NoOpScaleExecutor{}
}

// ValidateScale always returns success
func (e *NoOpScaleExecutor) ValidateScale(ctx context.Context, request ExecutionRequest) error {
	return nil
}

// ExecuteScale simulates scaling without actual changes
func (e *NoOpScaleExecutor) ExecuteScale(ctx context.Context, request ExecutionRequest) (*ExecutionResult, error) {
	return &ExecutionResult{
		Success:          true,
		ActualReplicas:   request.Recommendation.DesiredReplicas,
		PreviousReplicas: request.CurrentReplicas,
		ExecutionTime:    time.Millisecond,
		Metadata: map[string]interface{}{
			"executor": "noop",
		},
	}, nil
}

// GetExecutorType returns the executor type
func (e *NoOpScaleExecutor) GetExecutorType() string {
	return "noop"
}
