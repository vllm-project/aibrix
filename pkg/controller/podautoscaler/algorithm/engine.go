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

package algorithm

import (
	"context"
	"fmt"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/common"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// ScalingDecisionEngine makes scaling decisions based on aggregated metrics
type ScalingDecisionEngine interface {
	// ComputeRecommendation calculates desired replica count
	ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error)

	// GetEngineType returns the engine type
	GetEngineType() string

	// UpdateConfiguration changes engine parameters
	UpdateConfiguration(config EngineConfig) error
}

// ScalingRequest contains all data needed for scaling decision
type ScalingRequest struct {
	Target            types.ScaleTarget
	CurrentReplicas   int32
	AggregatedMetrics *aggregation.AggregatedMetrics
	ScalingContext    common.ScalingContext
	Constraints       types.ScalingConstraints
	Timestamp         time.Time
}

// ScalingRecommendation contains the scaling decision
type ScalingRecommendation struct {
	DesiredReplicas int32
	Confidence      float64
	Reason          string
	Algorithm       string
	ScaleValid      bool
	Metadata        map[string]interface{}
}

// EngineConfig defines engine parameters
type EngineConfig struct {
	Strategy       autoscalingv1alpha1.ScalingStrategyType
	PanicThreshold float64
	StableWindow   time.Duration
	PanicWindow    time.Duration
	ScaleToZero    bool
}

// KPADecisionEngine uses existing KPA algorithm logic
type KPADecisionEngine struct {
	algorithm ScalingAlgorithm // Reuse existing KpaScalingAlgorithm
	config    EngineConfig
}

func NewKPADecisionEngine(config EngineConfig) *KPADecisionEngine {
	return &KPADecisionEngine{
		algorithm: &KpaScalingAlgorithm{}, // Reuse existing algorithm
		config:    config,
	}
}

func (e *KPADecisionEngine) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	metrics := request.AggregatedMetrics

	// KPA logic: Choose between stable and panic metrics
	var currentValue float64
	var mode string

	// Simple panic mode detection
	if e.shouldEnterPanicMode(metrics) {
		currentValue = metrics.PanicValue
		mode = "panic"
	} else {
		currentValue = metrics.StableValue
		mode = "stable"
	}

	// Update scaling context with chosen metric value
	scalingCtx := request.ScalingContext
	scalingCtx.SetCurrentUsePerPod(currentValue)

	// Use existing algorithm logic
	desiredReplicas := e.algorithm.ComputeTargetReplicas(
		float64(request.CurrentReplicas),
		scalingCtx,
	)

	// Apply constraints
	desiredReplicas = e.applyConstraints(desiredReplicas, request.Constraints)

	return &ScalingRecommendation{
		DesiredReplicas: desiredReplicas,
		Confidence:      metrics.Confidence,
		Reason:          fmt.Sprintf("%s mode scaling", mode),
		Algorithm:       "kpa",
		ScaleValid:      true,
		Metadata: map[string]interface{}{
			"mode":          mode,
			"stable_value":  metrics.StableValue,
			"panic_value":   metrics.PanicValue,
			"current_value": currentValue,
		},
	}, nil
}

func (e *KPADecisionEngine) shouldEnterPanicMode(metrics *aggregation.AggregatedMetrics) bool {
	if metrics.StableValue <= 0 {
		return true
	}
	return metrics.PanicValue/metrics.StableValue > e.config.PanicThreshold
}

func (e *KPADecisionEngine) applyConstraints(replicas int32, constraints types.ScalingConstraints) int32 {
	if replicas < constraints.MinReplicas {
		return constraints.MinReplicas
	}
	if replicas > constraints.MaxReplicas {
		return constraints.MaxReplicas
	}
	return replicas
}

func (e *KPADecisionEngine) GetEngineType() string {
	return "kpa"
}

func (e *KPADecisionEngine) UpdateConfiguration(config EngineConfig) error {
	e.config = config
	return nil
}

// APADecisionEngine for APA strategy
type APADecisionEngine struct {
	algorithm ScalingAlgorithm // Could use existing or create APA-specific
	config    EngineConfig
}

func NewAPADecisionEngine(config EngineConfig) *APADecisionEngine {
	return &APADecisionEngine{
		algorithm: &KpaScalingAlgorithm{}, // Reuse KPA algorithm for now
		config:    config,
	}
}

func (e *APADecisionEngine) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	metrics := request.AggregatedMetrics

	// APA uses current value directly
	scalingCtx := request.ScalingContext
	scalingCtx.SetCurrentUsePerPod(metrics.CurrentValue)

	desiredReplicas := e.algorithm.ComputeTargetReplicas(float64(request.CurrentReplicas), scalingCtx)
	desiredReplicas = e.applyConstraints(desiredReplicas, request.Constraints)

	return &ScalingRecommendation{
		DesiredReplicas: desiredReplicas,
		Confidence:      metrics.Confidence,
		Reason:          "apa algorithm",
		Algorithm:       "apa",
		ScaleValid:      true,
		Metadata: map[string]interface{}{
			"current_value": metrics.CurrentValue,
		},
	}, nil
}

func (e *APADecisionEngine) applyConstraints(replicas int32, constraints types.ScalingConstraints) int32 {
	if replicas < constraints.MinReplicas {
		return constraints.MinReplicas
	}
	if replicas > constraints.MaxReplicas {
		return constraints.MaxReplicas
	}
	return replicas
}

func (e *APADecisionEngine) GetEngineType() string {
	return "apa"
}

func (e *APADecisionEngine) UpdateConfiguration(config EngineConfig) error {
	e.config = config
	return nil
}

// NewScalingDecisionEngine creates engines based on strategy
func NewScalingDecisionEngine(strategy autoscalingv1alpha1.ScalingStrategyType, config EngineConfig) ScalingDecisionEngine {
	switch strategy {
	case autoscalingv1alpha1.KPA:
		return NewKPADecisionEngine(config)
	case autoscalingv1alpha1.APA:
		return NewAPADecisionEngine(config)
	default:
		return NewKPADecisionEngine(config) // Default to KPA
	}
}
