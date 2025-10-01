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

// Package algorithm provides scaling algorithms for different autoscaling strategies.
// Each strategy (KPA, APA, HPA) has its own algorithm implementation that computes
// target replicas based on metrics and scaling context.
package algorithm

import (
	"context"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// ScalingAlgorithm computes target replicas based on metrics and context
// Implementations must be stateless and thread-safe for concurrent use
type ScalingAlgorithm interface {
	// ComputeRecommendation calculates desired replica count based on aggregated metrics
	// All configuration is passed via the request to keep the algorithm stateless
	ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error)

	// GetAlgorithmType returns the algorithm type (kpa, apa, hpa)
	GetAlgorithmType() string
}

// ScalingRequest contains all data needed for scaling decision
type ScalingRequest struct {
	Target            types.ScaleTarget
	CurrentReplicas   int32
	AggregatedMetrics *types.AggregatedMetrics
	ScalingContext    scalingctx.ScalingContext
	Constraints       types.ScalingConstraints
	Config            AlgorithmConfig // Algorithm-specific configuration
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

// AlgorithmConfig defines algorithm parameters
type AlgorithmConfig struct {
	Strategy       autoscalingv1alpha1.ScalingStrategyType
	PanicThreshold float64
	StableWindow   time.Duration
	PanicWindow    time.Duration
	ScaleToZero    bool
}

// NewScalingAlgorithm creates a stateless algorithm instance for the given strategy
// Instances can be safely cached and reused across goroutines
func NewScalingAlgorithm(strategy autoscalingv1alpha1.ScalingStrategyType) ScalingAlgorithm {
	switch strategy {
	case autoscalingv1alpha1.KPA:
		return &KPAAlgorithm{}
	case autoscalingv1alpha1.APA:
		return &APAAlgorithm{}
	case autoscalingv1alpha1.HPA:
		return &HPAAlgorithm{}
	default:
		return &KPAAlgorithm{} // Default to KPA
	}
}

// applyConstraints applies min/max replica constraints
func applyConstraints(replicas int32, constraints types.ScalingConstraints) int32 {
	if replicas < constraints.MinReplicas {
		return constraints.MinReplicas
	}
	if replicas > constraints.MaxReplicas {
		return constraints.MaxReplicas
	}
	return replicas
}
