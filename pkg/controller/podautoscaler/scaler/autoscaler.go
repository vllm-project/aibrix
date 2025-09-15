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
	"strconv"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/adapters"
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

	// Create metrics client for aggregator
	var metricsClient aggregation.MetricsClient
	if strategy == autoscalingv1alpha1.KPA {
		kpaClient := metrics.NewKPAMetricsClient(fetcher, config.AggregationConfig.StableWindow, config.AggregationConfig.PanicWindow)
		metricsClient = adapters.NewKPAMetricsClientAdapter(kpaClient)
	} else {
		apaClient := metrics.NewAPAMetricsClient(fetcher, config.AggregationConfig.Window)
		metricsClient = adapters.NewAPAMetricsClientAdapter(apaClient)
	}

	aggregator := aggregation.NewMetricAggregator(strategy, metricsClient, config.AggregationConfig)
	engine := algorithm.NewScalingDecisionEngine(strategy, config.EngineConfig)

	return &PipelineAutoScaler{
		strategy:   strategy,
		collector:  collector,
		aggregator: aggregator,
		engine:     engine,
	}, nil
}

func (s *PipelineAutoScaler) Scale(ctx context.Context, request types.ScaleRequest) (*types.ScaleResult, error) {
	logger := klog.FromContext(ctx)
	startTime := time.Now()

	// Log pipeline start
	logger.V(2).Info("Starting scaling pipeline",
		"strategy", s.strategy,
		"target", request.PodAutoscaler.Spec.ScaleTargetRef.Name,
		"currentReplicas", request.CurrentReplicas)

	// Ensure configuration is up to date
	if err := s.UpdateConfiguration(request.PodAutoscaler); err != nil {
		logger.Error(err, "Failed to update configuration")
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

	logger.V(3).Info("Collecting metrics",
		"collector", s.collector.GetCollectorType(),
		"pods", len(request.Pods))

	snapshot, err := s.collector.CollectMetrics(ctx, collectionSpec)
	if err != nil {
		logger.Error(err, "Failed to collect metrics")
		return &types.ScaleResult{ScaleValid: false}, err
	}

	logger.V(3).Info("Metrics collected",
		"values", len(snapshot.Values),
		"source", snapshot.Source)

	// Step 2: Process and aggregate metrics
	logger.V(3).Info("Processing metrics snapshot")
	if err := s.aggregator.ProcessSnapshot(snapshot); err != nil {
		logger.Error(err, "Failed to process snapshot")
		return &types.ScaleResult{ScaleValid: false}, err
	}

	// Convert to types.MetricKey
	metricKey := types.MetricKey{
		Namespace:  s.metricKey.Namespace,
		Name:       s.metricKey.Name,
		MetricName: s.metricKey.MetricName,
	}
	aggregatedMetrics, err := s.aggregator.GetAggregatedMetrics(metricKey, request.Timestamp)
	if err != nil {
		logger.Error(err, "Failed to get aggregated metrics")
		return &types.ScaleResult{ScaleValid: false}, err
	}

	logger.V(3).Info("Metrics aggregated",
		"currentValue", aggregatedMetrics.CurrentValue,
		"trend", aggregatedMetrics.Trend,
		"confidence", aggregatedMetrics.Confidence)

	// Step 3: Make scaling decision
	scalingRequest := algorithm.ScalingRequest{
		Target: types.ScaleTarget{
			Namespace:  request.PodAutoscaler.Namespace,
			Name:       request.PodAutoscaler.Spec.ScaleTargetRef.Name,
			Kind:       request.PodAutoscaler.Spec.ScaleTargetRef.Kind,
			APIVersion: request.PodAutoscaler.Spec.ScaleTargetRef.APIVersion,
			MetricKey: types.MetricKey{
				Namespace:  s.metricKey.Namespace,
				Name:       s.metricKey.Name,
				MetricName: s.metricKey.MetricName,
			},
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    s.createScalingContext(request.PodAutoscaler),
		Constraints:       s.createConstraints(request.PodAutoscaler),
		Timestamp:         request.Timestamp,
	}

	logger.V(3).Info("Computing scaling recommendation",
		"engine", s.engine.GetEngineType())

	recommendation, err := s.engine.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		logger.Error(err, "Failed to compute recommendation")
		return &types.ScaleResult{ScaleValid: false}, err
	}

	// Log pipeline completion
	duration := time.Since(startTime)
	logger.V(2).Info("Scaling pipeline completed",
		"strategy", s.strategy,
		"desired", recommendation.DesiredReplicas,
		"valid", recommendation.ScaleValid,
		"reason", recommendation.Reason,
		"duration", duration)

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
	config := aggregation.AggregationConfig{
		StableWindow: 60 * time.Second,
		PanicWindow:  6 * time.Second,
		Window:       60 * time.Second,
		Granularity:  time.Second,
	}

	if pa.Annotations != nil {
		// Parse stable window duration
		if stableWindowStr, ok := pa.Annotations["autoscaling.aibrix.ai/stable-window"]; ok {
			if duration, err := time.ParseDuration(stableWindowStr); err == nil {
				config.StableWindow = duration
			} else {
				klog.V(4).InfoS("Failed to parse stable window duration", "value", stableWindowStr, "error", err)
			}
		}

		// Parse panic window duration
		if panicWindowStr, ok := pa.Annotations["autoscaling.aibrix.ai/panic-window"]; ok {
			if duration, err := time.ParseDuration(panicWindowStr); err == nil {
				config.PanicWindow = duration
			} else {
				klog.V(4).InfoS("Failed to parse panic window duration", "value", panicWindowStr, "error", err)
			}
		}

		// Parse window duration (for APA)
		if windowStr, ok := pa.Annotations["autoscaling.aibrix.ai/window"]; ok {
			if duration, err := time.ParseDuration(windowStr); err == nil {
				config.Window = duration
			} else {
				klog.V(4).InfoS("Failed to parse window duration", "value", windowStr, "error", err)
			}
		}

		// Parse granularity
		if granularityStr, ok := pa.Annotations["autoscaling.aibrix.ai/granularity"]; ok {
			if duration, err := time.ParseDuration(granularityStr); err == nil {
				config.Granularity = duration
			} else {
				klog.V(4).InfoS("Failed to parse granularity duration", "value", granularityStr, "error", err)
			}
		}
	}

	return config
}

func (s *PipelineAutoScaler) extractEngineConfig(pa autoscalingv1alpha1.PodAutoscaler) algorithm.EngineConfig {
	config := algorithm.EngineConfig{
		Strategy:       pa.Spec.ScalingStrategy,
		PanicThreshold: 2.0,
		StableWindow:   60 * time.Second,
		PanicWindow:    6 * time.Second,
		ScaleToZero:    false,
	}

	if pa.Annotations != nil {
		// Parse panic threshold
		if thresholdStr, ok := pa.Annotations["autoscaling.aibrix.ai/panic-threshold"]; ok {
			if threshold, err := strconv.ParseFloat(thresholdStr, 64); err == nil {
				config.PanicThreshold = threshold
			} else {
				klog.V(4).InfoS("Failed to parse panic threshold", "value", thresholdStr, "error", err)
			}
		}

		// Parse scale to zero flag
		if scaleToZeroStr, ok := pa.Annotations["autoscaling.aibrix.ai/scale-to-zero"]; ok {
			if scaleToZero, err := strconv.ParseBool(scaleToZeroStr); err == nil {
				config.ScaleToZero = scaleToZero
			} else {
				klog.V(4).InfoS("Failed to parse scale-to-zero flag", "value", scaleToZeroStr, "error", err)
			}
		}

		// Re-use window configurations from aggregation config
		aggConfig := s.extractAggregationConfig(pa)
		config.StableWindow = aggConfig.StableWindow
		config.PanicWindow = aggConfig.PanicWindow
	}

	return config
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
