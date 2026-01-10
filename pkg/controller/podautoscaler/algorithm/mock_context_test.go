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

package algorithm

import (
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
)

// mockScalingContext implements the ScalingContext interface for testing purposes.
type mockScalingContext struct {
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

	MetricTargets map[string]scalingctx.MetricTarget // key: metric name (e.g., "cpu", "kv_cache_usage_perc")
}

// Ensure MockScalingContext implements the ScalingContext interface
var _ scalingctx.ScalingContext = (*mockScalingContext)(nil)

func (m *mockScalingContext) GetTargetValueForMetric(metricName string) (float64, bool) {
	if target, ok := m.MetricTargets[metricName]; ok {
		return target.TargetValue, true
	}
	return 0, false
}

func (m *mockScalingContext) GetUpFluctuationTolerance() float64 {
	return m.UpFluctuationTolerance
}

func (m *mockScalingContext) GetDownFluctuationTolerance() float64 {
	return m.DownFluctuationTolerance
}

func (m *mockScalingContext) GetMaxScaleUpRate() float64 {
	return m.MaxScaleUpRate
}

func (m *mockScalingContext) GetMaxScaleDownRate() float64 {
	return m.MaxScaleDownRate
}

func (m *mockScalingContext) GetCurrentUsePerPod() float64 {
	return m.CurrentUsePerPod
}

func (m *mockScalingContext) SetCurrentUsePerPod(value float64) {
	m.CurrentUsePerPod = value
}

func (m *mockScalingContext) GetMinReplicas() int32 {
	return m.MinReplicas
}

func (m *mockScalingContext) GetMaxReplicas() int32 {
	return m.MaxReplicas
}

func (m *mockScalingContext) GetStableValue() float64 {
	return m.StableValue
}

func (m *mockScalingContext) SetStableValue(value float64) {
	m.StableValue = value
}

func (m *mockScalingContext) GetPanicValue() float64 {
	return m.PanicValue
}

func (m *mockScalingContext) SetPanicValue(value float64) {
	m.PanicValue = value
}

// Activation scale
func (m *mockScalingContext) GetActivationScale() int32 {
	return m.ActivationScale
}

func (m *mockScalingContext) SetActivationScale(value int32) {
	m.ActivationScale = value
}

// Panic threshold and mode
func (m *mockScalingContext) GetPanicThreshold() float64 {
	return m.PanicThreshold
}

func (m *mockScalingContext) GetInPanicMode() bool {
	return m.InPanicMode
}

func (m *mockScalingContext) SetInPanicMode(inPanic bool) {
	m.InPanicMode = inPanic
}

func (m *mockScalingContext) GetMaxPanicPods() int32 {
	return m.MaxPanicPods
}

func (m *mockScalingContext) SetMaxPanicPods(pods int32) {
	m.MaxPanicPods = pods
}

func (m *mockScalingContext) GetScaleUpCooldownWindow() time.Duration {
	return 0
}

func (m *mockScalingContext) GetScaleDownCooldownWindow() time.Duration {
	return 300 * time.Second
}

func (m *mockScalingContext) GetScaleToZero() bool {
	return false
}

// UpdateByPaTypes - no-op for mock
func (m *mockScalingContext) UpdateByPaTypes(pa *autoscalingv1alpha1.PodAutoscaler) error {
	return nil
}
