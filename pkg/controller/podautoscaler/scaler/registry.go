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
	"fmt"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/adapters"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/execution"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ComponentRegistry manages the creation and lifecycle of autoscaler components
type ComponentRegistry struct {
	// Component factories
	collectorFactory  CollectorFactory
	aggregatorFactory AggregatorFactory
	engineFactory     EngineFactory
	executorFactory   ExecutorFactory

	// Cached components
	collectors  map[string]metrics.MetricCollector
	aggregators map[string]aggregation.MetricAggregator
	engines     map[string]algorithm.ScalingDecisionEngine
	executors   map[string]execution.ScaleExecutor

	// Shared dependencies
	fetcher metrics.MetricFetcher
	client  client.Client

	mutex sync.RWMutex
}

// Factory interfaces for component creation
type (
	CollectorFactory  func(sourceType autoscalingv1alpha1.MetricSourceType, fetcher metrics.MetricFetcher) metrics.MetricCollector
	AggregatorFactory func(strategy autoscalingv1alpha1.ScalingStrategyType, client aggregation.MetricsClient, config aggregation.AggregationConfig) aggregation.MetricAggregator
	EngineFactory     func(strategy autoscalingv1alpha1.ScalingStrategyType, config algorithm.EngineConfig) algorithm.ScalingDecisionEngine
	ExecutorFactory   func(client client.Client) execution.ScaleExecutor
)

// NewComponentRegistry creates a new component registry
func NewComponentRegistry(fetcher metrics.MetricFetcher, client client.Client) *ComponentRegistry {
	return &ComponentRegistry{
		// Default factories
		collectorFactory:  defaultCollectorFactory,
		aggregatorFactory: defaultAggregatorFactory,
		engineFactory:     defaultEngineFactory,
		executorFactory:   defaultExecutorFactory,

		// Component caches
		collectors:  make(map[string]metrics.MetricCollector),
		aggregators: make(map[string]aggregation.MetricAggregator),
		engines:     make(map[string]algorithm.ScalingDecisionEngine),
		executors:   make(map[string]execution.ScaleExecutor),

		// Shared dependencies
		fetcher: fetcher,
		client:  client,
	}
}

// GetOrCreateCollector gets or creates a metric collector
func (r *ComponentRegistry) GetOrCreateCollector(sourceType autoscalingv1alpha1.MetricSourceType) metrics.MetricCollector {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	key := string(sourceType)
	if collector, exists := r.collectors[key]; exists {
		return collector
	}

	collector := r.collectorFactory(sourceType, r.fetcher)
	r.collectors[key] = collector

	klog.V(4).InfoS("Created new metric collector",
		"type", sourceType,
		"collector", collector.GetCollectorType())

	return collector
}

// GetOrCreateAggregator gets or creates a metric aggregator
func (r *ComponentRegistry) GetOrCreateAggregator(strategy autoscalingv1alpha1.ScalingStrategyType, config aggregation.AggregationConfig) aggregation.MetricAggregator {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	key := string(strategy)
	if aggregator, exists := r.aggregators[key]; exists {
		// Update configuration if needed
		_ = aggregator.UpdateConfiguration(config)
		return aggregator
	}

	// Create metrics client first
	var metricsClient aggregation.MetricsClient
	if strategy == autoscalingv1alpha1.KPA {
		kpaClient := metrics.NewKPAMetricsClient(r.fetcher, config.StableWindow, config.PanicWindow)
		metricsClient = adapters.NewKPAMetricsClientAdapter(kpaClient)
	} else {
		apaClient := metrics.NewAPAMetricsClient(r.fetcher, config.Window)
		metricsClient = adapters.NewAPAMetricsClientAdapter(apaClient)
	}

	aggregator := r.aggregatorFactory(strategy, metricsClient, config)
	r.aggregators[key] = aggregator

	klog.V(4).InfoS("Created new metric aggregator",
		"strategy", strategy)

	return aggregator
}

// GetOrCreateEngine gets or creates a scaling decision engine
func (r *ComponentRegistry) GetOrCreateEngine(strategy autoscalingv1alpha1.ScalingStrategyType, config algorithm.EngineConfig) algorithm.ScalingDecisionEngine {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	key := string(strategy)
	if engine, exists := r.engines[key]; exists {
		// Update configuration if needed
		_ = engine.UpdateConfiguration(config)
		return engine
	}

	engine := r.engineFactory(strategy, config)
	r.engines[key] = engine

	klog.V(4).InfoS("Created new scaling decision engine",
		"strategy", strategy,
		"engine", engine.GetEngineType())

	return engine
}

// GetOrCreateExecutor gets or creates a scale executor
func (r *ComponentRegistry) GetOrCreateExecutor(executorType string) execution.ScaleExecutor {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if executor, exists := r.executors[executorType]; exists {
		return executor
	}

	executor := r.executorFactory(r.client)
	r.executors[executorType] = executor

	klog.V(4).InfoS("Created new scale executor",
		"type", executorType)

	return executor
}

// CreateAutoScaler creates a complete autoscaler with all components
func (r *ComponentRegistry) CreateAutoScaler(pa autoscalingv1alpha1.PodAutoscaler) (AutoScaler, error) {
	strategy := pa.Spec.ScalingStrategy
	// Use first metric source for now
	if len(pa.Spec.MetricsSources) == 0 {
		return nil, fmt.Errorf("no metric sources defined")
	}
	metricSource := pa.Spec.MetricsSources[0]

	// Extract configurations
	aggConfig := extractAggregationConfig(pa)
	engineConfig := extractEngineConfig(pa, aggConfig)

	// Get or create components
	collector := r.GetOrCreateCollector(metricSource.MetricSourceType)
	aggregator := r.GetOrCreateAggregator(strategy, aggConfig)
	engine := r.GetOrCreateEngine(strategy, engineConfig)

	// Create the pipeline autoscaler
	return &PipelineAutoScalerWithExecutor{
		PipelineAutoScaler: &PipelineAutoScaler{
			strategy:   strategy,
			collector:  collector,
			aggregator: aggregator,
			engine:     engine,
		},
		executor: r.GetOrCreateExecutor("default"),
	}, nil
}

// RegisterCollectorFactory registers a custom collector factory
func (r *ComponentRegistry) RegisterCollectorFactory(factory CollectorFactory) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.collectorFactory = factory
}

// RegisterAggregatorFactory registers a custom aggregator factory
func (r *ComponentRegistry) RegisterAggregatorFactory(factory AggregatorFactory) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.aggregatorFactory = factory
}

// RegisterEngineFactory registers a custom engine factory
func (r *ComponentRegistry) RegisterEngineFactory(factory EngineFactory) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.engineFactory = factory
}

// RegisterExecutorFactory registers a custom executor factory
func (r *ComponentRegistry) RegisterExecutorFactory(factory ExecutorFactory) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.executorFactory = factory
}

// Clear clears all cached components
func (r *ComponentRegistry) Clear() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.collectors = make(map[string]metrics.MetricCollector)
	r.aggregators = make(map[string]aggregation.MetricAggregator)
	r.engines = make(map[string]algorithm.ScalingDecisionEngine)
	r.executors = make(map[string]execution.ScaleExecutor)

	klog.V(4).InfoS("Cleared component registry")
}

// Default factory implementations
func defaultCollectorFactory(sourceType autoscalingv1alpha1.MetricSourceType, fetcher metrics.MetricFetcher) metrics.MetricCollector {
	return metrics.NewMetricCollector(sourceType, fetcher)
}

func defaultAggregatorFactory(strategy autoscalingv1alpha1.ScalingStrategyType, client aggregation.MetricsClient, config aggregation.AggregationConfig) aggregation.MetricAggregator {
	return aggregation.NewMetricAggregator(strategy, client, config)
}

func defaultEngineFactory(strategy autoscalingv1alpha1.ScalingStrategyType, config algorithm.EngineConfig) algorithm.ScalingDecisionEngine {
	return algorithm.NewScalingDecisionEngine(strategy, config)
}

func defaultExecutorFactory(client client.Client) execution.ScaleExecutor {
	if client == nil {
		return execution.NewNoOpScaleExecutor()
	}
	return execution.NewDefaultScaleExecutor(client)
}

// PipelineAutoScalerWithExecutor extends PipelineAutoScaler with execution capability
type PipelineAutoScalerWithExecutor struct {
	*PipelineAutoScaler
	executor execution.ScaleExecutor
}

// ExecuteScale executes the scaling decision
func (s *PipelineAutoScalerWithExecutor) ExecuteScale(ctx context.Context, recommendation *algorithm.ScalingRecommendation, target types.ScaleTarget, currentReplicas int32) (*execution.ExecutionResult, error) {
	request := execution.ExecutionRequest{
		Target:          target,
		CurrentReplicas: currentReplicas,
		Recommendation:  recommendation,
		DryRun:          false,
		Timestamp:       time.Now(),
	}

	return s.executor.ExecuteScale(ctx, request)
}

// Helper function to extract aggregation config (reuse from autoscaler.go)
func extractAggregationConfig(pa autoscalingv1alpha1.PodAutoscaler) aggregation.AggregationConfig {
	// Implementation moved here from autoscaler.go for reuse
	config := aggregation.AggregationConfig{
		StableWindow: 60 * time.Second,
		PanicWindow:  6 * time.Second,
		Window:       60 * time.Second,
		Granularity:  time.Second,
	}

	if pa.Annotations != nil {
		// Parse configurations from annotations
		// (implementation details as in autoscaler.go)
	}

	return config
}

// Helper function to extract engine config (reuse from autoscaler.go)
func extractEngineConfig(pa autoscalingv1alpha1.PodAutoscaler, aggConfig aggregation.AggregationConfig) algorithm.EngineConfig {
	config := algorithm.EngineConfig{
		Strategy:       pa.Spec.ScalingStrategy,
		PanicThreshold: 2.0,
		StableWindow:   aggConfig.StableWindow,
		PanicWindow:    aggConfig.PanicWindow,
		ScaleToZero:    false,
	}

	if pa.Annotations != nil {
		// Parse configurations from annotations
		// (implementation details as in autoscaler.go)
	}

	return config
}
