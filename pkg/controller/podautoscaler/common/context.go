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

package common

import (
	"strconv"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"k8s.io/klog/v2"
)

const (
	AutoscalingLabelPrefix = "autoscaling.aibrix.ai/"
	maxScaleUpRateLabel    = AutoscalingLabelPrefix + "max-scale-up-rate"
	maxScaleDownRateLabel  = AutoscalingLabelPrefix + "max-scale-down-rate"
)

// ScalingContext defines the generalized common that holds all necessary data for scaling calculations.
type ScalingContext interface {
	GetTargetValue() float64
	GetUpFluctuationTolerance() float64
	GetDownFluctuationTolerance() float64
	GetMaxScaleUpRate() float64
	GetMaxScaleDownRate() float64
	GetCurrentUsePerPod() float64
	UpdateByPaTypes(pa *autoscalingv1alpha1.PodAutoscaler) error
	GetMinReplicas() int32
	GetMaxReplicas() int32
	GetStableValue() float64
	SetStableValue(float64)
	GetPanicValue() float64
	SetPanicValue(float64)
	SetActivationScale(value int32)
	GetActivationScale() int32
	GetPanicThreshold() float64
	GetInPanicMode() bool
	SetInPanicMode(bool)
	GetMaxPanicPods() int32
	SetMaxPanicPods(int32)
	GetStableWindow() time.Duration
	GetPanicWindow() time.Duration
	GetScaleDownDelay() time.Duration
}

// BaseScalingContext provides a base implementation of the ScalingContext interface.
type BaseScalingContext struct {
	// Maximum rate at which to scale up
	MaxScaleUpRate float64
	// Maximum rate at which to scale down, a value of 2.5 means the count can reduce to at most 2.5 times less than the current value in one step.
	MaxScaleDownRate float64
	// The metric used for scaling, i.e. CPU, Memory, QPS.
	ScalingMetric string
	// The value of scaling metric per pod that we target to maintain.
	TargetValue float64
	// The total value of scaling metric that a pod can maintain.
	TotalValue float64
	// The current use per pod.
	currentUsePerPod float64
	// The minimum number of replicas to which the target can be scaled down.
	MinReplicas int32
	// The maximum number of replicas to which the target can be scaled up
	MaxReplicas int32
}

var _ ScalingContext = (*BaseScalingContext)(nil)

// NewBaseScalingContext creates a new instance of BaseScalingContext with default values.
func NewBaseScalingContext() *BaseScalingContext {
	return &BaseScalingContext{
		MaxScaleUpRate:   2,     // Scale up rate of 200%, allowing rapid scaling
		MaxScaleDownRate: 2,     // Scale down rate of 50%, for more gradual reduction
		ScalingMetric:    "CPU", // Metric used for scaling, here set to CPU utilization
		TargetValue:      30.0,  // Target CPU utilization set at 10%
		TotalValue:       100.0, // Total CPU utilization capacity for pods is 100%
	}
}

// UpdateByPaTypes should be invoked in any scaling context that embeds BaseScalingContext.
func (b *BaseScalingContext) UpdateByPaTypes(pa *autoscalingv1alpha1.PodAutoscaler) error {
	source, err := autoscalingv1alpha1.GetPaMetricSources(*pa)
	if err != nil {
		return err
	}

	b.ScalingMetric = source.TargetMetric
	// parse target value
	targetValue, err := strconv.ParseFloat(source.TargetValue, 64)
	if err != nil {
		klog.ErrorS(err, "Failed to parse target value", "targetValue", source.TargetValue)
		return err
	}
	b.TargetValue = targetValue

	for key, value := range pa.Annotations {
		switch key {
		case maxScaleUpRateLabel:
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return err
			}
			b.MaxScaleUpRate = v
		case maxScaleDownRateLabel:
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return err
			}
			b.MaxScaleDownRate = v
		}
	}
	return nil
}

func (b *BaseScalingContext) SetCurrentUsePerPod(value float64) {
	b.currentUsePerPod = value
}

func (b *BaseScalingContext) SetMinReplicas(minReplicas int32) {
	b.MinReplicas = minReplicas
}

func (b *BaseScalingContext) SetMaxReplicas(maxReplicas int32) {
	b.MaxReplicas = maxReplicas
}

func (b *BaseScalingContext) GetUpFluctuationTolerance() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetDownFluctuationTolerance() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetMaxScaleUpRate() float64 {
	return b.MaxScaleUpRate
}

func (b *BaseScalingContext) GetMaxScaleDownRate() float64 {
	return b.MaxScaleDownRate
}

func (b *BaseScalingContext) GetCurrentUsePerPod() float64 {
	return b.currentUsePerPod
}

func (b *BaseScalingContext) GetTargetValue() float64 {
	return b.TargetValue
}

func (b *BaseScalingContext) GetScalingTolerance() (up float64, down float64) {
	return b.MaxScaleUpRate, b.MaxScaleDownRate
}

func (b *BaseScalingContext) GetMinReplicas() int32 {
	return b.MinReplicas
}

func (b *BaseScalingContext) GetMaxReplicas() int32 {
	return b.MaxReplicas
}

// Missing methods - implementing with default/zero values for base implementation

// Stable and Panic values - base implementation returns 0
func (b *BaseScalingContext) GetStableValue() float64 {
	return 0
}

func (b *BaseScalingContext) SetStableValue(value float64) {
	// No-op in base implementation
}

func (b *BaseScalingContext) GetPanicValue() float64 {
	return 0
}

func (b *BaseScalingContext) SetPanicValue(value float64) {
	// No-op in base implementation
}

// Activation scale - base implementation returns 1 (minimum valid value)
func (b *BaseScalingContext) GetActivationScale() int32 {
	return 1
}

func (b *BaseScalingContext) SetActivationScale(value int32) {
	// No-op in base implementation
}

// Panic mode state - base implementation returns false/0
func (b *BaseScalingContext) GetPanicThreshold() float64 {
	return 0
}

func (b *BaseScalingContext) GetInPanicMode() bool {
	return false
}

func (b *BaseScalingContext) SetInPanicMode(inPanic bool) {
	// No-op in base implementation
}

func (b *BaseScalingContext) GetMaxPanicPods() int32 {
	return 0
}

func (b *BaseScalingContext) SetMaxPanicPods(pods int32) {
	// No-op in base implementation
}

// Time windows - base implementation returns 0 duration
func (b *BaseScalingContext) GetStableWindow() time.Duration {
	return 0
}

func (b *BaseScalingContext) GetPanicWindow() time.Duration {
	return 0
}

func (b *BaseScalingContext) GetScaleDownDelay() time.Duration {
	return 0
}
