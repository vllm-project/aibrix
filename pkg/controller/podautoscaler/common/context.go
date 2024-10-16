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

// ScalingContext defines the generalized common that holds all necessary data for scaling calculations.
type ScalingContext interface {
	GetTargetValue() float64
	GetUpFluctuationTolerance() float64
	GetDownFluctuationTolerance() float64
	GetMaxScaleUpRate() float64
	GetMaxScaleDownRate() float64
	GetCurrentUsePerPod() float64
}

// BaseScalingContext provides a base implementation of the ScalingContext interface.
type BaseScalingContext struct {
	currentUsePerPod float64
	targetValue      float64
	upTolerance      float64
	downTolerance    float64
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
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetMaxScaleDownRate() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetCurrentUsePerPod() float64 {
	return b.currentUsePerPod
}

func (b *BaseScalingContext) GetTargetValue() float64 {
	return b.targetValue
}

func (b *BaseScalingContext) GetScalingTolerance() (up float64, down float64) {
	return b.upTolerance, b.downTolerance
}
