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

package podautoscaler

import (
	"context"
	"fmt"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/config"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AutoScaler orchestrates the complete scaling pipeline
type AutoScaler interface {
	// Scale executes the complete scaling pipeline
	Scale(ctx context.Context, request types.ScaleRequest) (*types.ScaleResult, error)

	// ComputeDesiredReplicas performs metric-based scaling calculation only
	// This method focuses on business logic without K8s resource management
	ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*ReplicaComputeResult, error)

	// UpdateConfiguration updates scaler configuration
	UpdateConfiguration(pa autoscalingv1alpha1.PodAutoscaler) error

	// GetStrategy returns the scaling strategy
	GetStrategy() autoscalingv1alpha1.ScalingStrategyType
}

// ReplicaComputeRequest represents a request for replica calculation
type ReplicaComputeRequest struct {
	PodAutoscaler   autoscalingv1alpha1.PodAutoscaler
	CurrentReplicas int32
	Pods            []corev1.Pod
	Timestamp       time.Time
}

// ReplicaComputeResult represents the result of replica calculation
type ReplicaComputeResult struct {
	DesiredReplicas int32
	Algorithm       string
	Reason          string
	Valid           bool
}

// DefaultAutoScaler implements the complete scaling pipeline
type DefaultAutoScaler struct {
	// Core components
	factory         metrics.MetricFetcherFactory
	client          client.Client
	configExtractor *config.ConfigExtractor

	// Current configuration (updated per request)
	strategy     autoscalingv1alpha1.ScalingStrategyType
	aggregator   aggregation.MetricAggregator
	algorithm    algorithm.ScalingAlgorithm
	metricKey    metrics.NamespaceNameMetric
	metricSource autoscalingv1alpha1.MetricSource
}

// NewDefaultAutoScaler creates a new default autoscaler
func NewDefaultAutoScaler(
	factory metrics.MetricFetcherFactory,
	client client.Client,
) *DefaultAutoScaler {
	return &DefaultAutoScaler{
		factory:         factory,
		client:          client,
		configExtractor: config.NewConfigExtractor(),
	}
}

// configureForStrategy configures the autoscaler for a specific strategy
func (a *DefaultAutoScaler) configureForStrategy(strategy autoscalingv1alpha1.ScalingStrategyType, source autoscalingv1alpha1.MetricSource) {
	// Create metrics client for this specific source
	metricsClient := metrics.NewMetricsClient(a.factory.For(source), time.Second)

	// Configure client and create aggregator based on strategy
	if strategy == autoscalingv1alpha1.KPA {
		metricsClient.ConfigureForStrategy(strategy, metrics.MetricsConfig{
			StableWindow: 60 * time.Second,
			PanicWindow:  6 * time.Second,
		})
		a.aggregator = aggregation.NewKPAMetricAggregator(metricsClient, aggregation.AggregationConfig{
			StableWindow: 60 * time.Second,
			PanicWindow:  6 * time.Second,
			Granularity:  time.Second,
		})
	} else {
		metricsClient.ConfigureForStrategy(strategy, metrics.MetricsConfig{
			Window: 60 * time.Second,
		})
		a.aggregator = aggregation.NewAPAMetricAggregator(metricsClient, aggregation.AggregationConfig{
			Window:      60 * time.Second,
			Granularity: time.Second,
		})
	}

	// Create algorithm for this strategy
	a.algorithm = algorithm.NewScalingAlgorithm(strategy, algorithm.AlgorithmConfig{
		Strategy:       strategy,
		PanicThreshold: 2.0,
		StableWindow:   60 * time.Second,
		PanicWindow:    6 * time.Second,
		ScaleToZero:    false,
	})

	// Update current strategy
	a.strategy = strategy
}

func (a *DefaultAutoScaler) Scale(ctx context.Context, request types.ScaleRequest) (*types.ScaleResult, error) {
	logger := klog.FromContext(ctx)
	startTime := time.Now()

	// Log pipeline start
	logger.V(2).Info("Starting scaling pipeline",
		"strategy", a.strategy,
		"target", request.PodAutoscaler.Spec.ScaleTargetRef.Name,
		"currentReplicas", request.CurrentReplicas)

	// Ensure configuration is up to date
	if err := a.UpdateConfiguration(request.PodAutoscaler); err != nil {
		logger.Error(err, "Failed to update configuration")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to update configuration for %s/%s: %w",
			request.PodAutoscaler.Namespace, request.PodAutoscaler.Name, err)
	}

	// Step 1: Collect metrics
	collectionSpec := types.CollectionSpec{
		Namespace:    a.metricKey.Namespace,
		TargetName:   a.metricKey.Name,
		MetricName:   a.metricKey.MetricName,
		MetricSource: a.metricSource,
		Pods:         request.Pods,
		Timestamp:    request.Timestamp,
	}

	logger.V(3).Info("Collecting metrics",
		"source", a.metricSource.MetricSourceType,
		"pods", len(request.Pods))

	snapshot, err := metrics.CollectMetrics(ctx, collectionSpec, a.factory)
	if err != nil {
		logger.Error(err, "Failed to collect metrics")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to collect metrics for %s/%s: %w",
			request.PodAutoscaler.Namespace, request.PodAutoscaler.Name, err)
	}

	logger.V(3).Info("Metrics collected",
		"values", len(snapshot.Values),
		"source", snapshot.Source)

	// Step 2: Process and aggregate metrics
	logger.V(3).Info("Processing metrics snapshot")
	if err := a.aggregator.ProcessSnapshot(snapshot); err != nil {
		logger.Error(err, "Failed to process snapshot")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to process metrics snapshot: %w", err)
	}

	// Convert to types.MetricKey
	metricKey := types.MetricKey{
		Namespace:  a.metricKey.Namespace,
		Name:       a.metricKey.Name,
		MetricName: a.metricKey.MetricName,
	}
	aggregatedMetrics, err := a.aggregator.GetAggregatedMetrics(metricKey, request.Timestamp)
	if err != nil {
		logger.Error(err, "Failed to get aggregated metrics")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to get aggregated metrics for %s: %w",
			metricKey, err)
	}

	// Enhance confidence with pod count awareness
	podCount := len(request.Pods)
	if podCount > 0 {
		// Simple pod-aware confidence enhancement
		// More pods = higher confidence (diminishing returns)
		podConfidenceFactor := minFloat64(1.0, float64(podCount)/5.0)
		aggregatedMetrics.Confidence = minFloat64(1.0, aggregatedMetrics.Confidence*0.7+podConfidenceFactor*0.3)
	}

	logger.V(3).Info("Metrics aggregated",
		"currentValue", aggregatedMetrics.CurrentValue,
		"trend", aggregatedMetrics.Trend,
		"confidence", aggregatedMetrics.Confidence,
		"podCount", len(request.Pods))

	// Step 3: Make scaling decision
	scalingRequest := algorithm.ScalingRequest{
		Target: types.ScaleTarget{
			Namespace:  request.PodAutoscaler.Namespace,
			Name:       request.PodAutoscaler.Spec.ScaleTargetRef.Name,
			Kind:       request.PodAutoscaler.Spec.ScaleTargetRef.Kind,
			APIVersion: request.PodAutoscaler.Spec.ScaleTargetRef.APIVersion,
			MetricKey:  metricKey,
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    a.createScalingContext(request.PodAutoscaler),
		Constraints:       a.createConstraints(request.PodAutoscaler),
		Timestamp:         request.Timestamp,
	}

	logger.V(3).Info("Computing scaling recommendation",
		"algorithm", a.algorithm.GetAlgorithmType())

	recommendation, err := a.algorithm.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		logger.Error(err, "Failed to compute recommendation")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to compute scaling recommendation: %w", err)
	}

	// Step 4: Execute scaling (embedded - no separate executor)
	result, err := a.executeScale(ctx, recommendation, scalingRequest.Target, request.CurrentReplicas)
	if err != nil {
		logger.Error(err, "Failed to execute scaling")
		return &types.ScaleResult{ScaleValid: false}, fmt.Errorf("failed to execute scaling: %w", err)
	}

	// Log pipeline completion
	duration := time.Since(startTime)
	logger.V(2).Info("Scaling pipeline completed",
		"strategy", a.strategy,
		"desired", result.DesiredPodCount,
		"valid", result.ScaleValid,
		"reason", result.Reason,
		"duration", duration)

	return result, nil
}

func (a *DefaultAutoScaler) ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*ReplicaComputeResult, error) {
	logger := klog.FromContext(ctx)

	// Ensure configuration is up to date
	if err := a.UpdateConfiguration(request.PodAutoscaler); err != nil {
		logger.Error(err, "Failed to update configuration")
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to update configuration: %w", err)
	}

	// Step 1: Collect metrics
	collectionSpec := types.CollectionSpec{
		Namespace:    a.metricKey.Namespace,
		TargetName:   a.metricKey.Name,
		MetricName:   a.metricKey.MetricName,
		MetricSource: a.metricSource,
		Pods:         request.Pods,
		Timestamp:    request.Timestamp,
	}

	logger.V(3).Info("Collecting metrics for replica computation",
		"source", a.metricSource.MetricSourceType,
		"pods", len(request.Pods))

	snapshot, err := metrics.CollectMetrics(ctx, collectionSpec, a.factory)
	if err != nil {
		logger.Error(err, "Failed to collect metrics")
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to collect metrics: %w", err)
	}

	// Step 2: Process and aggregate metrics
	if err := a.aggregator.ProcessSnapshot(snapshot); err != nil {
		logger.Error(err, "Failed to process snapshot")
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to process metrics snapshot: %w", err)
	}

	metricKey := types.MetricKey{
		Namespace:  a.metricKey.Namespace,
		Name:       a.metricKey.Name,
		MetricName: a.metricKey.MetricName,
	}
	aggregatedMetrics, err := a.aggregator.GetAggregatedMetrics(metricKey, request.Timestamp)
	if err != nil {
		logger.Error(err, "Failed to get aggregated metrics")
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to get aggregated metrics: %w", err)
	}

	// Enhance confidence with pod count awareness
	podCount := len(request.Pods)
	if podCount > 0 {
		podConfidenceFactor := minFloat64(1.0, float64(podCount)/5.0)
		aggregatedMetrics.Confidence = minFloat64(1.0, aggregatedMetrics.Confidence*0.7+podConfidenceFactor*0.3)
	}

	// Step 3: Make scaling decision
	scalingRequest := algorithm.ScalingRequest{
		Target: types.ScaleTarget{
			Namespace:  request.PodAutoscaler.Namespace,
			Name:       request.PodAutoscaler.Spec.ScaleTargetRef.Name,
			Kind:       request.PodAutoscaler.Spec.ScaleTargetRef.Kind,
			APIVersion: request.PodAutoscaler.Spec.ScaleTargetRef.APIVersion,
			MetricKey:  metricKey,
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    a.createScalingContext(request.PodAutoscaler),
		Constraints:       a.createConstraints(request.PodAutoscaler),
		Timestamp:         request.Timestamp,
	}

	recommendation, err := a.algorithm.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		logger.Error(err, "Failed to compute recommendation")
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to compute scaling recommendation: %w", err)
	}

	logger.V(3).Info("Computed scaling recommendation",
		"currentReplicas", request.CurrentReplicas,
		"desiredReplicas", recommendation.DesiredReplicas,
		"algorithm", recommendation.Algorithm)

	return &ReplicaComputeResult{
		DesiredReplicas: recommendation.DesiredReplicas,
		Algorithm:       recommendation.Algorithm,
		Reason:          recommendation.Reason,
		Valid:           recommendation.ScaleValid,
	}, nil
}

func (a *DefaultAutoScaler) UpdateConfiguration(pa autoscalingv1alpha1.PodAutoscaler) error {
	// Extract metric key and source
	metricKey, metricSource, err := metrics.NewNamespaceNameMetric(&pa)
	if err != nil {
		return fmt.Errorf("failed to create metric key: %w", err)
	}

	a.metricKey = metricKey
	a.metricSource = metricSource

	// Configure strategy-specific components (this replaces on-demand creation)
	a.configureForStrategy(pa.Spec.ScalingStrategy, metricSource)

	// Update component configurations using ConfigExtractor if components exist
	if a.aggregator != nil {
		aggregationConfig, err := a.configExtractor.ExtractAggregationConfig(pa)
		if err != nil {
			return fmt.Errorf("failed to extract aggregation configuration: %w", err)
		}
		if err := a.aggregator.UpdateConfiguration(aggregationConfig); err != nil {
			return fmt.Errorf("failed to update aggregator configuration: %w", err)
		}
	}

	if a.algorithm != nil {
		algorithmConfig, err := a.configExtractor.ExtractAlgorithmConfig(pa)
		if err != nil {
			return fmt.Errorf("failed to extract algorithm configuration: %w", err)
		}
		if err := a.algorithm.UpdateConfiguration(algorithmConfig); err != nil {
			return fmt.Errorf("failed to update algorithm configuration: %w", err)
		}
	}

	return nil
}

func (a *DefaultAutoScaler) GetStrategy() autoscalingv1alpha1.ScalingStrategyType {
	return a.strategy
}

// executeScale performs the actual scaling operation
func (a *DefaultAutoScaler) executeScale(ctx context.Context, recommendation *algorithm.ScalingRecommendation, target types.ScaleTarget, currentReplicas int32) (*types.ScaleResult, error) {
	logger := klog.FromContext(ctx)

	// For now, we just return the recommendation without actually scaling the resource
	// In a full implementation, this would update the target resource's replica count
	logger.V(3).Info("Scaling recommendation ready",
		"target", target.Name,
		"currentReplicas", currentReplicas,
		"desiredReplicas", recommendation.DesiredReplicas,
		"algorithm", recommendation.Algorithm)

	return &types.ScaleResult{
		DesiredPodCount: recommendation.DesiredReplicas,
		ScaleValid:      recommendation.ScaleValid,
		Reason:          recommendation.Reason,
		Algorithm:       recommendation.Algorithm,
		Metadata:        recommendation.Metadata,
	}, nil
}

// Helper methods
func (a *DefaultAutoScaler) createScalingContext(pa autoscalingv1alpha1.PodAutoscaler) scalingctx.ScalingContext {
	// Create base context with defaults
	ctx := scalingctx.NewBaseScalingContext()

	// Update with PA-specific values
	if err := ctx.UpdateByPaTypes(&pa); err != nil {
		// Log error but continue with defaults
		klog.ErrorS(err, "Failed to update scaling context from PodAutoscaler, using defaults")
	}

	return ctx
}

func (a *DefaultAutoScaler) createConstraints(pa autoscalingv1alpha1.PodAutoscaler) types.ScalingConstraints {
	minReplicas := int32(1)
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	}

	return types.ScalingConstraints{
		MinReplicas: minReplicas,
		MaxReplicas: pa.Spec.MaxReplicas,
	}
}
