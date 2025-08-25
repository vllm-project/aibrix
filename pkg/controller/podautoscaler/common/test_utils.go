/*
Copyright 2025 The Aibrix Team.

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
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// MockScalingContext implements the ScalingContext interface for testing purposes.
// This mock can be used across all algorithm tests (KPA, APA, HPA, etc.) and any
// other components that need to test with ScalingContext.
type MockScalingContext struct {
	TargetValue              float64
	UpFluctuationTolerance   float64
	DownFluctuationTolerance float64
	MaxScaleUpRate           float64
	MaxScaleDownRate         float64
	CurrentUsePerPod         float64
	MaxReplicas              int32
	MinReplicas              int32

	// Stable and Panic values
	StableValue float64
	PanicValue  float64

	// Activation and Panic settings
	ActivationScale int32
	PanicThreshold  float64

	// Panic mode state
	InPanicMode  bool
	MaxPanicPods int32

	// Time windows
	StableWindow   time.Duration
	PanicWindow    time.Duration
	ScaleDownDelay time.Duration
}

// Ensure MockScalingContext implements the ScalingContext interface
var _ ScalingContext = (*MockScalingContext)(nil)

// Basic scaling parameters
func (m *MockScalingContext) GetTargetValue() float64 {
	return m.TargetValue
}

func (m *MockScalingContext) GetUpFluctuationTolerance() float64 {
	return m.UpFluctuationTolerance
}

func (m *MockScalingContext) GetDownFluctuationTolerance() float64 {
	return m.DownFluctuationTolerance
}

func (m *MockScalingContext) GetMaxScaleUpRate() float64 {
	return m.MaxScaleUpRate
}

func (m *MockScalingContext) GetMaxScaleDownRate() float64 {
	return m.MaxScaleDownRate
}

func (m *MockScalingContext) GetCurrentUsePerPod() float64 {
	return m.CurrentUsePerPod
}

// Replica constraints
func (m *MockScalingContext) GetMinReplicas() int32 {
	return m.MinReplicas
}

func (m *MockScalingContext) GetMaxReplicas() int32 {
	return m.MaxReplicas
}

// Stable and Panic values
func (m *MockScalingContext) GetStableValue() float64 {
	return m.StableValue
}

func (m *MockScalingContext) SetStableValue(value float64) {
	m.StableValue = value
}

func (m *MockScalingContext) GetPanicValue() float64 {
	return m.PanicValue
}

func (m *MockScalingContext) SetPanicValue(value float64) {
	m.PanicValue = value
}

// Activation scale
func (m *MockScalingContext) GetActivationScale() int32 {
	return m.ActivationScale
}

func (m *MockScalingContext) SetActivationScale(value int32) {
	m.ActivationScale = value
}

// Panic threshold and mode
func (m *MockScalingContext) GetPanicThreshold() float64 {
	return m.PanicThreshold
}

func (m *MockScalingContext) GetInPanicMode() bool {
	return m.InPanicMode
}

func (m *MockScalingContext) SetInPanicMode(inPanic bool) {
	m.InPanicMode = inPanic
}

func (m *MockScalingContext) GetMaxPanicPods() int32 {
	return m.MaxPanicPods
}

func (m *MockScalingContext) SetMaxPanicPods(pods int32) {
	m.MaxPanicPods = pods
}

// Time windows
func (m *MockScalingContext) GetStableWindow() time.Duration {
	return m.StableWindow
}

func (m *MockScalingContext) GetPanicWindow() time.Duration {
	return m.PanicWindow
}

func (m *MockScalingContext) GetScaleDownDelay() time.Duration {
	return m.ScaleDownDelay
}

// UpdateByPaTypes - no-op for mock
func (m *MockScalingContext) UpdateByPaTypes(pa *autoscalingv1alpha1.PodAutoscaler) error {
	return nil
}

// NewMockScalingContext creates a new MockScalingContext with default values
// that are commonly used in tests.
func NewMockScalingContext() *MockScalingContext {
	return &MockScalingContext{
		TargetValue:              50.0,
		UpFluctuationTolerance:   0.1,
		DownFluctuationTolerance: 0.1,
		MaxScaleUpRate:           2.0,
		MaxScaleDownRate:         2.0,
		CurrentUsePerPod:         50.0,
		MinReplicas:              1,
		MaxReplicas:              10,

		// Stable and Panic values (initialized to reasonable defaults)
		StableValue: 50.0,
		PanicValue:  50.0,

		// Activation and Panic settings
		ActivationScale: 1,
		PanicThreshold:  2.0,

		// Panic mode state
		InPanicMode:  false,
		MaxPanicPods: 0,

		// Time windows
		StableWindow:   60 * time.Second,
		PanicWindow:    10 * time.Second,
		ScaleDownDelay: 30 * time.Minute,
	}
}

// NewMockKpaScalingContext creates a MockScalingContext with KPA-specific default values
// for easier testing of KPA algorithms.
func NewMockKpaScalingContext() *MockScalingContext {
	return &MockScalingContext{
		TargetValue:              10.0,
		UpFluctuationTolerance:   0.1,
		DownFluctuationTolerance: 0.1,
		MaxScaleUpRate:           2.0,
		MaxScaleDownRate:         2.0,
		CurrentUsePerPod:         10.0,
		MinReplicas:              0,
		MaxReplicas:              100,

		// Stable and Panic values
		StableValue: 10.0,
		PanicValue:  10.0,

		// KPA-specific settings
		ActivationScale: 1,
		PanicThreshold:  2.0, // 200% threshold

		// Panic mode state
		InPanicMode:  false,
		MaxPanicPods: 0,

		// KPA time windows
		StableWindow:   60 * time.Second,
		PanicWindow:    10 * time.Second,
		ScaleDownDelay: 30 * time.Minute,
	}
}
