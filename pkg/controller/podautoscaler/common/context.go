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

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
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
	b.ScalingMetric = pa.Spec.TargetMetric
	// parse target value
	targetValue, err := strconv.ParseFloat(pa.Spec.TargetValue, 64)
	if err != nil {
		klog.ErrorS(err, "Failed to parse target value", "targetValue", pa.Spec.TargetValue)
		return err
	}
	b.TargetValue = targetValue

	for key, value := range pa.Labels {
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
