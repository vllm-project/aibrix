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

package scaler

import (
	"context"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/klog/v2"
)

// AutoScaler orchestrates the complete scaling pipeline
type AutoScaler interface {
	// Scale executes the complete scaling pipeline
	Scale(ctx context.Context, request types.ScaleRequest) (*types.ScaleResult, error)

	// UpdateConfiguration updates scaler configuration
	UpdateConfiguration(pa autoscalingv1alpha1.PodAutoscaler) error

	// GetStrategy returns the scaling strategy
	GetStrategy() autoscalingv1alpha1.ScalingStrategyType
}

// PipelineAutoScaler implements the complete scaling pipeline
type PipelineAutoScaler struct {
	strategy   autoscalingv1alpha1.ScalingStrategyType
	collector  metrics.MetricCollector
	aggregator aggregation.MetricAggregator
	engine     algorithm.ScalingDecisionEngine
	// Metric tracking
	metricKey    metrics.NamespaceNameMetric
	metricSource autoscalingv1alpha1.MetricSource
}

// NewPipelineAutoScaler creates a new pipeline-based autoscaler
func NewPipelineAutoScaler(
	strategy autoscalingv1alpha1.ScalingStrategyType,
	fetcher metrics.MetricFetcher,
	config AutoScalerConfig,
) (*PipelineAutoScaler, error) {

	// Create pipeline components
	collector := metrics.NewMetricCollector(config.MetricSource.MetricSourceType, fetcher)
	aggregator := aggregation.NewMetricAggregator(strategy, fetcher, config.AggregationConfig)
	engine := algorithm.NewScalingDecisionEngine(strategy, config.EngineConfig)

	return &PipelineAutoScaler{
		strategy:   strategy,
		collector:  collector,
		aggregator: aggregator,
		engine:     engine,
	}, nil
}

func (s *PipelineAutoScaler) Scale(ctx context.Context, request types.ScaleRequest) (*types.ScaleResult, error) {
	// Ensure configuration is up to date
	if err := s.UpdateConfiguration(request.PodAutoscaler); err != nil {
		return &types.ScaleResult{ScaleValid: false}, err
	}

	// Step 1: Collect metrics
	collectionSpec := types.CollectionSpec{
		Namespace:    s.metricKey.Namespace,
		TargetName:   s.metricKey.Name,
		MetricName:   s.metricKey.MetricName,
		MetricSource: s.metricSource,
		Pods:         request.Pods,
		Timestamp:    request.Timestamp,
	}

	snapshot, err := s.collector.CollectMetrics(ctx, collectionSpec)
	if err != nil {
		return &types.ScaleResult{ScaleValid: false}, err
	}

	// Step 2: Process and aggregate metrics
	if err := s.aggregator.ProcessSnapshot(snapshot); err != nil {
		return &types.ScaleResult{ScaleValid: false}, err
	}

	aggregatedMetrics, err := s.aggregator.GetAggregatedMetrics(s.metricKey, request.Timestamp)
	if err != nil {
		return &types.ScaleResult{ScaleValid: false}, err
	}

	// Step 3: Make scaling decision
	scalingRequest := algorithm.ScalingRequest{
		Target: types.ScaleTarget{
			Namespace:  request.PodAutoscaler.Namespace,
			Name:       request.PodAutoscaler.Spec.ScaleTargetRef.Name,
			Kind:       request.PodAutoscaler.Spec.ScaleTargetRef.Kind,
			APIVersion: request.PodAutoscaler.Spec.ScaleTargetRef.APIVersion,
			MetricKey:  s.metricKey,
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    s.createScalingContext(request.PodAutoscaler),
		Constraints:       s.createConstraints(request.PodAutoscaler),
		Timestamp:         request.Timestamp,
	}

	recommendation, err := s.engine.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		return &types.ScaleResult{ScaleValid: false}, err
	}

	return &types.ScaleResult{
		DesiredPodCount: recommendation.DesiredReplicas,
		ScaleValid:      recommendation.ScaleValid,
		Reason:          recommendation.Reason,
		Algorithm:       recommendation.Algorithm,
		Metadata:        recommendation.Metadata,
	}, nil
}

func (s *PipelineAutoScaler) UpdateConfiguration(pa autoscalingv1alpha1.PodAutoscaler) error {
	// Extract metric key and source
	metricKey, metricSource, err := metrics.NewNamespaceNameMetric(&pa)
	if err != nil {
		return err
	}

	s.metricKey = metricKey
	s.metricSource = metricSource

	// Update component configurations
	aggregationConfig := s.extractAggregationConfig(pa)
	if err := s.aggregator.UpdateConfiguration(aggregationConfig); err != nil {
		return err
	}

	engineConfig := s.extractEngineConfig(pa)
	if err := s.engine.UpdateConfiguration(engineConfig); err != nil {
		return err
	}

	return nil
}

func (s *PipelineAutoScaler) GetStrategy() autoscalingv1alpha1.ScalingStrategyType {
	return s.strategy
}

// Helper methods
func (s *PipelineAutoScaler) createScalingContext(pa autoscalingv1alpha1.PodAutoscaler) common.ScalingContext {
	// Create base context with defaults
	ctx := common.NewBaseScalingContext()

	// Update with PA-specific values
	if err := ctx.UpdateByPaTypes(&pa); err != nil {
		// Log error but continue with defaults
		klog.ErrorS(err, "Failed to update scaling context from PodAutoscaler, using defaults")
	}

	return ctx
}

func (s *PipelineAutoScaler) createConstraints(pa autoscalingv1alpha1.PodAutoscaler) types.ScalingConstraints {
	minReplicas := int32(1)
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	}

	return types.ScalingConstraints{
		MinReplicas: minReplicas,
		MaxReplicas: pa.Spec.MaxReplicas,
	}
}

func (s *PipelineAutoScaler) extractAggregationConfig(pa autoscalingv1alpha1.PodAutoscaler) aggregation.AggregationConfig {
	// Extract from PA annotations or use defaults
	return aggregation.AggregationConfig{
		StableWindow: 60 * time.Second,
		PanicWindow:  6 * time.Second,
		Window:       60 * time.Second,
		Granularity:  time.Second,
	}
}

func (s *PipelineAutoScaler) extractEngineConfig(pa autoscalingv1alpha1.PodAutoscaler) algorithm.EngineConfig {
	return algorithm.EngineConfig{
		Strategy:       pa.Spec.ScalingStrategy,
		PanicThreshold: 2.0, // Extract from annotations
		StableWindow:   60 * time.Second,
		PanicWindow:    6 * time.Second,
		ScaleToZero:    false,
	}
}

// AutoScalerConfig contains configuration for all components
type AutoScalerConfig struct {
	MetricSource      autoscalingv1alpha1.MetricSource
	AggregationConfig aggregation.AggregationConfig
	EngineConfig      algorithm.EngineConfig
}

// Factory function to create autoscalers
func NewAutoScaler(strategy autoscalingv1alpha1.ScalingStrategyType, fetcher metrics.MetricFetcher, config AutoScalerConfig) (AutoScaler, error) {
	return NewPipelineAutoScaler(strategy, fetcher, config)
}
