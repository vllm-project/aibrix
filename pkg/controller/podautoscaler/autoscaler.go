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
	"math"
	"sync"
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
// All implementations are stateless and thread-safe, supporting concurrent reconciliation.
type AutoScaler interface {
	// ComputeDesiredReplicas performs metric-based scaling calculation.
	// This is the primary method for scaling decisions and returns only the recommendation.
	// It does NOT perform any actual scaling operations.
	// All per-PA configuration is extracted from the PodAutoscaler spec on each call.
	ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*MetricRoundResult, error)
}

// ReplicaComputeRequest represents a request for replica calculation.
// This type is used both for the public interface and internal pipeline processing.
type ReplicaComputeRequest struct {
	PodAutoscaler   autoscalingv1alpha1.PodAutoscaler
	ScalingContext  scalingctx.ScalingContext // Single source of truth for PA-level configuration
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

// MetricSourceResult contains one metric source's health and optional recommendation.
type MetricSourceResult struct {
	MetricName     string
	MetricSource   autoscalingv1alpha1.MetricSourceType
	Health         types.MetricSourceHealth
	Recommendation *ReplicaComputeResult
}

// MetricRoundResult contains the selected recommendation plus per-source health.
type MetricRoundResult struct {
	DesiredReplicas     int32
	Algorithm           string
	Reason              string
	Valid               bool
	HasRecommendation   bool
	SourceResults       []MetricSourceResult
	TotalSourceCount    int
	HealthySourceCount  int
	DegradedSourceCount int
	FailedSourceCount   int
	HealthReason        string
}

// DefaultAutoScaler implements the complete scaling pipeline
// All components are stateless or thread-safe, allowing concurrent reconciliation
type DefaultAutoScaler struct {
	// Immutable shared components (all thread-safe)
	factory       metrics.MetricFetcherFactory
	client        client.Client
	metricsClient *metrics.MetricsClient
	aggregator    aggregation.MetricAggregator

	// Algorithm cache (algorithms are stateless structs, can be safely reused)
	mu             sync.RWMutex
	algorithmCache map[autoscalingv1alpha1.ScalingStrategyType]algorithm.ScalingAlgorithm
}

// NewDefaultAutoScaler creates a new default autoscaler
func NewDefaultAutoScaler(
	factory metrics.MetricFetcherFactory,
	client client.Client,
) *DefaultAutoScaler {
	// Create stable metrics client with default window durations
	// Windows auto-initialize on first use based on the PodAutoscaler config
	metricsClient := metrics.NewMetricsClient(time.Second)

	// Create single aggregator for all strategies (stateless, just delegates to client)
	aggregator := aggregation.NewMetricAggregator(metricsClient)

	return &DefaultAutoScaler{
		factory:        factory,
		client:         client,
		metricsClient:  metricsClient,
		aggregator:     aggregator,
		algorithmCache: make(map[autoscalingv1alpha1.ScalingStrategyType]algorithm.ScalingAlgorithm),
	}
}

// getOrCreateAlgorithm retrieves or creates a stateless algorithm for the given strategy
// The algorithm struct is stateless and can be safely cached and reused
func (a *DefaultAutoScaler) getOrCreateAlgorithm(strategy autoscalingv1alpha1.ScalingStrategyType) algorithm.ScalingAlgorithm {
	// Try read lock first (common case)
	a.mu.RLock()
	algo, exists := a.algorithmCache[strategy]
	a.mu.RUnlock()
	if exists {
		return algo
	}

	// Need to create, acquire write lock
	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-check after acquiring write lock
	if algo, exists := a.algorithmCache[strategy]; exists {
		return algo
	}

	// Create new stateless algorithm instance
	algo = algorithm.NewScalingAlgorithm(strategy)
	a.algorithmCache[strategy] = algo
	return algo
}

// ComputeDesiredReplicas computes desired replicas based on all metrics in MetricsSources.
// It returns the maximum recommended replicas across all valid metrics.
func (a *DefaultAutoScaler) ComputeDesiredReplicas(ctx context.Context, request ReplicaComputeRequest) (*MetricRoundResult, error) {
	pa := request.PodAutoscaler
	metricsSources := pa.Spec.MetricsSources

	if len(metricsSources) == 0 {
		return &MetricRoundResult{Valid: false}, fmt.Errorf(
			"no metricsSources defined in PodAutoscaler %s/%s", pa.Namespace, pa.Name)
	}
	circuitBreakerEnabled := config.NormalizeCircuitBreaker(pa.Spec.CircuitBreaker).Enabled
	var validResults []*ReplicaComputeResult // to record the recommended result calculated in the current round
	round := &MetricRoundResult{
		TotalSourceCount: len(metricsSources),
		SourceResults:    make([]MetricSourceResult, 0, len(metricsSources)),
	}

	for _, metricSource := range metricsSources {
		// Extract per-request configuration
		metricKey := types.MetricKey{
			Namespace:   pa.Namespace,
			Name:        pa.Spec.ScaleTargetRef.Name,
			MetricName:  metricSource.TargetMetric,
			PaNamespace: pa.Namespace,
			PaName:      pa.Name,
		}

		sourceResult, err := a.computeReplicasForSingleMetric(ctx, request, metricKey, metricSource, circuitBreakerEnabled)
		if err != nil {
			klog.ErrorS(err, "Failed to compute replicas for metric",
				"PodAutoscaler", klog.KObj(&pa),
				"metric", metricSource.TargetMetric,
				"metricSourceType", metricSource.MetricSourceType)
			if circuitBreakerEnabled {
				if sourceResult.Health.State == "" || sourceResult.Health.State == types.MetricHealthStateHealthy {
					sourceResult.Health.State = types.MetricHealthStateFailed
					sourceResult.Health.Reason = types.MetricReasonAllMetricSamplesUnavailable
					sourceResult.Health.ValidCount = 0
					if sourceResult.Health.TotalCount == 0 {
						sourceResult.Health.TotalCount = 1
					}
					if sourceResult.Health.FailureCount == 0 {
						sourceResult.Health.FailureCount = sourceResult.Health.TotalCount
					}
				}
				round.addSourceResult(sourceResult)
				continue
			}
			// Continue processing other metrics — one failure shouldn't block all
			continue
		}

		round.addSourceResult(sourceResult)
		if sourceResult.Recommendation != nil && sourceResult.Recommendation.Valid {
			validResults = append(validResults, sourceResult.Recommendation)
			klog.V(4).InfoS("Computed replicas for metric",
				"PodAutoscaler", klog.KObj(&pa),
				"metricName", metricSource.TargetMetric,
				"desiredReplicas", sourceResult.Recommendation.DesiredReplicas,
				"algorithm", sourceResult.Recommendation.Algorithm,
				"reason", sourceResult.Recommendation.Reason,
				"currentReplicas", request.CurrentReplicas,
			)
		}
	}

	if len(validResults) == 0 {
		round.Valid = false
		round.HasRecommendation = false
		round.finalizeHealthReason()
		if circuitBreakerEnabled {
			return round, nil
		}
		return nil, fmt.Errorf("all %d metric sources failed for "+
			"PodAutoscaler %s/%s", len(metricsSources), pa.Namespace, pa.Name)
	}

	// compare the recommended values of the current round and use the maximum value
	bestResult := validResults[0]
	for _, r := range validResults[1:] {
		if r.DesiredReplicas > bestResult.DesiredReplicas {
			bestResult = r
		}
	}

	allValidReplicas := make([]int32, len(validResults))
	for i, res := range validResults {
		allValidReplicas[i] = res.DesiredReplicas
	}

	klog.V(2).InfoS("Multi metric autoscaling computed result",
		"podAutoscaler", klog.KObj(&pa),
		"metricCount", len(allValidReplicas),
		"desiredReplicas", bestResult.DesiredReplicas,
		"validDesiredReplicas", allValidReplicas,
		"currentReplicas", request.CurrentReplicas,
	)

	round.DesiredReplicas = bestResult.DesiredReplicas
	round.Algorithm = bestResult.Algorithm
	round.Reason = bestResult.Reason
	round.Valid = true
	round.HasRecommendation = true
	round.finalizeHealthReason()
	return round, nil
}

func (r *MetricRoundResult) addSourceResult(source MetricSourceResult) {
	r.SourceResults = append(r.SourceResults, source)
	switch source.Health.State {
	case types.MetricHealthStateHealthy:
		r.HealthySourceCount++
	case types.MetricHealthStateDegraded:
		r.DegradedSourceCount++
	case types.MetricHealthStateFailed:
		r.FailedSourceCount++
	}
}

func (r *MetricRoundResult) finalizeHealthReason() {
	switch {
	case r.FailedSourceCount == r.TotalSourceCount && r.TotalSourceCount > 0:
		if firstUnhealthyReason(r.SourceResults) == types.MetricReasonAggregatedMetricInvalid {
			r.HealthReason = types.MetricReasonAggregatedMetricInvalid
			return
		}
		r.HealthReason = types.MetricReasonAllMetricSourcesFailed
	case r.FailedSourceCount > 0:
		r.HealthReason = firstUnhealthyReason(r.SourceResults)
	case r.DegradedSourceCount > 0:
		r.HealthReason = firstUnhealthyReason(r.SourceResults)
	default:
		r.HealthReason = types.MetricReasonMetricsHealthy
	}
}

func firstUnhealthyReason(results []MetricSourceResult) string {
	for _, result := range results {
		if result.Health.Reason != "" && result.Health.Reason != types.MetricReasonMetricsHealthy {
			return result.Health.Reason
		}
	}
	return types.MetricReasonMetricsHealthy
}

func successfulSourceResult(
	metricSource autoscalingv1alpha1.MetricSource,
	health types.MetricSourceHealth,
	recommendation *algorithm.ScalingRecommendation,
) MetricSourceResult {
	return MetricSourceResult{
		MetricName:   metricSource.TargetMetric,
		MetricSource: metricSource.MetricSourceType,
		Health:       health,
		Recommendation: &ReplicaComputeResult{
			DesiredReplicas: recommendation.DesiredReplicas,
			Algorithm:       recommendation.Algorithm,
			Reason:          recommendation.Reason,
			Valid:           true,
		},
	}
}

func failedSourceResult(metricSource autoscalingv1alpha1.MetricSource, health types.MetricSourceHealth) MetricSourceResult {
	return MetricSourceResult{
		MetricName:   metricSource.TargetMetric,
		MetricSource: metricSource.MetricSourceType,
		Health:       health,
	}
}

// computeReplicasForSingleMetric computes desired replicas for a single MetricSource.
// It wraps executeScalingPipeline and formats the result.
func (a *DefaultAutoScaler) computeReplicasForSingleMetric(
	ctx context.Context,
	request ReplicaComputeRequest,
	metricKey types.MetricKey,
	metricSource autoscalingv1alpha1.MetricSource,
	circuitBreakerEnabled bool,
) (MetricSourceResult, error) {

	// Get algorithm based on the PA-level scaling strategy (shared across all metrics)
	algo := a.getOrCreateAlgorithm(request.PodAutoscaler.Spec.ScalingStrategy)

	// Execute the full pipeline for this single metric
	recommendation, health, err := a.executeScalingPipeline(ctx, request, metricKey, metricSource, algo, circuitBreakerEnabled)
	if err != nil {
		return failedSourceResult(metricSource, health), fmt.Errorf("failed to compute recommendation for metric %q: %w", metricSource.TargetMetric, err)
	}

	if recommendation == nil {
		return failedSourceResult(metricSource, health), nil
	}

	if !recommendation.ScaleValid {
		return failedSourceResult(metricSource, health), fmt.Errorf("scaling recommendation invalid for metric %q", metricSource.TargetMetric)
	}

	return successfulSourceResult(metricSource, health, recommendation), nil
}

// executeScalingPipeline contains the common scaling logic for replica computation
func (a *DefaultAutoScaler) executeScalingPipeline(
	ctx context.Context,
	request ReplicaComputeRequest,
	metricKey types.MetricKey,
	metricSource autoscalingv1alpha1.MetricSource,
	algo algorithm.ScalingAlgorithm,
	circuitBreakerEnabled bool,
) (*algorithm.ScalingRecommendation, types.MetricSourceHealth, error) {
	workloadKey := fmt.Sprintf("%s/%s", request.PodAutoscaler.Namespace, request.PodAutoscaler.Name)

	// Step 1: Collect metrics
	collectionSpec := types.CollectionSpec{
		Namespace:    metricKey.Namespace,
		TargetName:   metricKey.Name,
		MetricName:   metricKey.MetricName,
		MetricSource: metricSource,
		Pods:         request.Pods,
		Timestamp:    request.Timestamp,
	}

	klog.InfoS("Collecting metrics", "source", workloadKey, "pods", len(request.Pods))
	snapshot, err := metrics.CollectMetrics(ctx, collectionSpec, a.factory)
	if err != nil {
		return nil, types.MetricSourceHealth{}, fmt.Errorf("failed to collect metrics for %s: %w", workloadKey, err)
	}
	sourceHealth := types.MetricSourceHealth{
		TotalCount:   snapshot.CollectionStats.ExpectedCount,
		ValidCount:   len(snapshot.Values),
		FailureCount: snapshot.CollectionStats.FetchFailureCount,
		State:        types.MetricHealthStateHealthy,
		Reason:       types.MetricReasonMetricsHealthy,
	}
	if circuitBreakerEnabled {
		sanitized, health := metrics.EvaluateSnapshot(snapshot)
		sourceHealth = health
		if health.State == types.MetricHealthStateFailed {
			return nil, health, nil
		}
		snapshot = sanitized
	}

	// Use scaling context from request (single source of truth)
	scalingContext := request.ScalingContext

	// Step 2: Process and aggregate metrics
	klog.InfoS("Processing metrics snapshot", "source", workloadKey, "healthy metrics pods", len(snapshot.Values), "values", snapshot.Values)
	if err := a.aggregator.ProcessSnapshot(metricKey, snapshot); err != nil {
		return nil, sourceHealth, fmt.Errorf("failed to process metrics snapshot for %s: %w", workloadKey, err)
	}
	// Use the full metricKey with PaNamespace and PaName for proper multi-tenancy
	aggregatedMetrics, err := a.aggregator.GetAggregatedMetrics(metricKey, request.Timestamp)
	if err != nil {
		return nil, sourceHealth, fmt.Errorf("failed to get aggregated metrics for %s %s: %w", workloadKey, metricKey, err)
	}
	if circuitBreakerEnabled && !isAggregatedMetricValid(request.PodAutoscaler.Spec.ScalingStrategy, aggregatedMetrics) {
		sourceHealth.State = types.MetricHealthStateFailed
		sourceHealth.Reason = types.MetricReasonAggregatedMetricInvalid
		sourceHealth.ValidCount = 0
		sourceHealth.FailureCount = 0
		sourceHealth.InvalidCount = sourceHealth.TotalCount
		return nil, sourceHealth, nil
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
			MetricKey:  metricKey,
		},
		CurrentReplicas:   request.CurrentReplicas,
		AggregatedMetrics: aggregatedMetrics,
		ScalingContext:    scalingContext,
		Timestamp:         request.Timestamp,
	}

	klog.InfoS("Computing scaling recommendation", "source", workloadKey,
		"algorithm", algo.GetAlgorithmType())
	recommendation, err := algo.ComputeRecommendation(ctx, scalingRequest)
	if err != nil {
		return nil, sourceHealth, fmt.Errorf("failed to compute scaling recommendation for %s: %w", workloadKey, err)
	}
	klog.InfoS("Scaling recommendation computed",
		"source", workloadKey,
		"algorithm", algo.GetAlgorithmType(),
		"recommendation", recommendation)

	return recommendation, sourceHealth, nil
}

func isAggregatedMetricValid(strategy autoscalingv1alpha1.ScalingStrategyType, aggregatedMetrics *types.AggregatedMetrics) bool {
	if aggregatedMetrics == nil {
		return false
	}

	switch strategy {
	case autoscalingv1alpha1.KPA:
		return isFiniteNonNegative(aggregatedMetrics.StableValue) && isFiniteNonNegative(aggregatedMetrics.PanicValue)
	default:
		return isFiniteNonNegative(aggregatedMetrics.StableValue)
	}
}

func isFiniteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// Helper methods
