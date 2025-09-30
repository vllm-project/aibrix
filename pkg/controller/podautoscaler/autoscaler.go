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
	"hash/fnv"
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

// AutoScaler provides scaling decision capabilities based on metrics and algorithms.
// This interface focuses purely on scaling logic without actual resource manipulation.
type AutoScaler interface {
	// ComputeDesiredReplicas performs metric-based scaling calculation.
	// This is the primary method for scaling decisions and returns only the recommendation.
	// It does NOT perform any actual scaling operations.
	ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*ReplicaComputeResult, error)

	// UpdateConfiguration updates the autoscaler's strategy and parameters.
	// This method optimizes performance by only recreating components when strategy changes.
	UpdateConfiguration(pa autoscalingv1alpha1.PodAutoscaler) error

	// GetStrategy returns the currently configured scaling strategy.
	GetStrategy() autoscalingv1alpha1.ScalingStrategyType
}

// ReplicaComputeRequest represents a request for replica calculation.
// This type is used both for the public interface and internal pipeline processing.
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
	factory         metrics.MetricFetcherFactory
	client          client.Client
	configExtractor *config.ConfigExtractor
	metricsClient   *metrics.MetricsClient

	// Current configuration (updated per request)
	strategy               autoscalingv1alpha1.ScalingStrategyType
	lastConfiguredStrategy autoscalingv1alpha1.ScalingStrategyType
	aggregator             aggregation.MetricAggregator
	algorithm              algorithm.ScalingAlgorithm
	metricKey              types.MetricKey
	metricSource           autoscalingv1alpha1.MetricSource
	lastConfigHash         uint64 // Track configuration changes
}

// NewDefaultAutoScaler creates a new default autoscaler
func NewDefaultAutoScaler(
	factory metrics.MetricFetcherFactory,
	client client.Client,
) *DefaultAutoScaler {
	// Create stable metrics client with default window durations
	// Windows auto-initialize on first use based on the PodAutoscaler config
	metricsClient := metrics.NewMetricsClient(time.Second)

	// Create single aggregator for all strategies (doesn't depend on strategy)
	aggregator := aggregation.NewMetricAggregator(metricsClient, aggregation.AggregationConfig{
		StableWindow: 120 * time.Second,
		PanicWindow:  15 * time.Second,
		Window:       120 * time.Second,
		Granularity:  time.Second,
	})

	return &DefaultAutoScaler{
		factory:         factory,
		client:          client,
		configExtractor: config.NewConfigExtractor(),
		metricsClient:   metricsClient,
		aggregator:      aggregator,
	}
}

// configureForStrategy configures the autoscaler for a specific strategy
func (a *DefaultAutoScaler) configureForStrategy(strategy autoscalingv1alpha1.ScalingStrategyType, source autoscalingv1alpha1.MetricSource) {
	// MetricsClient and aggregator are already created and stable
	// Windows are auto-initialized on first metric update with default durations

	// Only create algorithm based on strategy (this is the only strategy-specific component)
	a.algorithm = algorithm.NewScalingAlgorithm(strategy, algorithm.AlgorithmConfig{
		Strategy:       strategy,
		StableWindow:   120 * time.Second,
		PanicWindow:    15 * time.Second,
		PanicThreshold: 2.0,
		ScaleToZero:    false,
	})

	// Update current strategy
	a.strategy = strategy
}

func (a *DefaultAutoScaler) ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*ReplicaComputeResult, error) {
	// Ensure configuration is up to date
	if err := a.UpdateConfiguration(request.PodAutoscaler); err != nil {
		return &ReplicaComputeResult{Valid: false}, fmt.Errorf("failed to update configuration: %w", err)
	}

	// Execute the common scaling pipeline
	recommendation, err := a.executeScalingPipeline(ctx, request)
	if err != nil {
		return &ReplicaComputeResult{Valid: false}, err
	}

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

	// Calculate configuration hash for change detection
	configHash := a.calculateConfigHash(pa)

	// Check if we need to reconfigure components
	// TODO: address the locking issue.
	// TODO: lastConfiguredStrategy could be empty, in this case, we do not need to print logs etc.
	strategyChanged := a.lastConfiguredStrategy != pa.Spec.ScalingStrategy
	configChanged := a.lastConfigHash != configHash

	a.metricKey = metricKey
	a.metricSource = metricSource

	// Only recreate components if strategy changed
	if strategyChanged {
		klog.V(4).InfoS("Strategy changed, reconfiguring components",
			"oldStrategy", a.lastConfiguredStrategy,
			"newStrategy", pa.Spec.ScalingStrategy)
		a.configureForStrategy(pa.Spec.ScalingStrategy, metricSource)
		a.lastConfiguredStrategy = pa.Spec.ScalingStrategy
	}

	// Update component configurations if config changed or components were recreated
	if configChanged || strategyChanged {
		if err := a.updateComponentConfigurations(pa); err != nil {
			return fmt.Errorf("failed to update component configurations: %w", err)
		}
		a.lastConfigHash = configHash
	}

	return nil
}

// calculateConfigHash creates a hash of the configuration to detect changes
func (a *DefaultAutoScaler) calculateConfigHash(pa autoscalingv1alpha1.PodAutoscaler) uint64 {
	// Use a more robust hashing mechanism to reduce collision probability.
	h := fnv.New64a()

	// Strategy
	_, _ = fmt.Fprint(h, string(pa.Spec.ScalingStrategy))

	// Min/Max replicas
	if pa.Spec.MinReplicas != nil {
		_, _ = fmt.Fprintf(h, "%d", *pa.Spec.MinReplicas)
	}
	_, _ = fmt.Fprintf(h, "%d", pa.Spec.MaxReplicas)

	// Metric sources
	for _, source := range pa.Spec.MetricsSources {
		_, _ = fmt.Fprint(h, string(source.MetricSourceType))
		_, _ = fmt.Fprint(h, source.TargetMetric)
		_, _ = fmt.Fprint(h, source.TargetValue)
		_, _ = fmt.Fprint(h, source.Path)
	}

	return h.Sum64()
}

// updateComponentConfigurations updates existing component configurations
func (a *DefaultAutoScaler) updateComponentConfigurations(pa autoscalingv1alpha1.PodAutoscaler) error {
	// Update aggregator configuration if it exists
	if a.aggregator != nil {
		aggregationConfig, err := a.configExtractor.ExtractAggregationConfig(pa)
		if err != nil {
			return fmt.Errorf("failed to extract aggregation configuration: %w", err)
		}
		if err := a.aggregator.UpdateConfiguration(aggregationConfig); err != nil {
			return fmt.Errorf("failed to update aggregator configuration: %w", err)
		}
	}

	// Update algorithm configuration if it exists
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

// executeScalingPipeline contains the common scaling logic for replica computation
func (a *DefaultAutoScaler) executeScalingPipeline(ctx context.Context, request ReplicaComputeRequest) (*algorithm.ScalingRecommendation, error) {
	workloadKey := fmt.Sprintf("%s/%s", request.PodAutoscaler.Namespace, request.PodAutoscaler.Name)
	// Step 1: Collect metrics
	collectionSpec := types.CollectionSpec{
		Namespace:    a.metricKey.Namespace,
		TargetName:   a.metricKey.Name,
		MetricName:   a.metricKey.MetricName,
		MetricSource: a.metricSource,
		Pods:         request.Pods,
		Timestamp:    request.Timestamp,
	}

	klog.InfoS("Collecting metrics", "source", workloadKey, "pods", len(request.Pods))
	snapshot, err := metrics.CollectMetrics(ctx, collectionSpec, a.factory)
	if err != nil {
		return nil, fmt.Errorf("failed to collect metrics for %s: %w", workloadKey, err)
	}

	// Step 2: Process and aggregate metrics
	klog.InfoS("Processing metrics snapshot", "source", workloadKey, "healthy metrics pods", len(snapshot.Values), "values", snapshot.Values)
	if err := a.aggregator.ProcessSnapshot(a.metricKey, snapshot); err != nil {
		return nil, fmt.Errorf("failed to process metrics snapshot for %s: %w", workloadKey, err)
	}
	// Use the full metricKey with PaNamespace and PaName for proper multi-tenancy
	aggregatedMetrics, err := a.aggregator.GetAggregatedMetrics(a.metricKey, request.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregated metrics for %s %s: %w", workloadKey, a.metricKey, err)
	}

	// Step 3: Enhance confidence with pod count awareness
	// TODO: enable confidence and pod count later
	//podCount := len(request.Pods)
	//if podCount > 0 {
	//	// Simple pod-aware confidence enhancement
	//	// More pods = higher confidence (diminishing returns)
	//	podConfidenceFactor := minFloat64(1.0, float64(podCount)/5.0)
	//	aggregatedMetrics.Confidence = minFloat64(1.0, aggregatedMetrics.Confidence*0.7+podConfidenceFactor*0.3)
	//}

	klog.InfoS("Metrics aggregated",
		"averageValue", aggregatedMetrics.StableValue,
		"trend", aggregatedMetrics.Trend,
		"confidence", aggregatedMetrics.Confidence,
		"podCount", len(request.Pods))

	// Step 4: Make scaling decision
	scalingRequest := algorithm.ScalingRequest{
		Target: types.ScaleTarget{
			Namespace:  request.PodAutoscaler.Namespace,
			Name:       request.PodAutoscaler.Spec.ScaleTargetRef.Name,
			Kind:       request.PodAutoscaler.Spec.ScaleTargetRef.Kind,
			APIVersion: request.PodAutoscaler.Spec.ScaleTargetRef.APIVersion,
			MetricKey:  a.metricKey,
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    a.createScalingContext(request.PodAutoscaler),
		Constraints:       a.createConstraints(request.PodAutoscaler),
		Timestamp:         request.Timestamp,
	}

	klog.InfoS("Computing scaling recommendation", "source", workloadKey,
		"algorithm", a.algorithm.GetAlgorithmType())
	recommendation, err := a.algorithm.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to compute scaling recommendation for %s: %w", workloadKey, err)
	}
	klog.InfoS("Scaling recommendation computed",
		"source", workloadKey,
		"algorithm", a.algorithm.GetAlgorithmType(),
		"recommendation", recommendation)

	return recommendation, nil
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
