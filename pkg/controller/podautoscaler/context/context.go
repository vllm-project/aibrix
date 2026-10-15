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

package context

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	"k8s.io/klog/v2"
)

// ScalingContext defines the generalized context that holds all necessary data for scaling calculations.
// This is the single source of truth for all scaling configuration, extracted per-PodAutoscaler.
type ScalingContext interface {
	GetTargetValueForMetric(metricName string) (float64, bool)
	GetMetricSourceTypeForMetric(metricName string) (autoscalingv1alpha1.MetricSourceType, bool)
	GetUpFluctuationTolerance() float64
	GetDownFluctuationTolerance() float64
	GetMaxScaleUpRate() float64
	GetMaxScaleDownRate() float64
	GetCurrentUsePerPod() float64
	SetCurrentUsePerPod(float64)
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
	GetScaleUpCooldownWindow() time.Duration
	GetScaleDownCooldownWindow() time.Duration
	GetScaleToZero() bool
}

// baseScalingContext provides a base implementation of the ScalingContext interface.
type baseScalingContext struct {
	// Maximum rate at which to scale up
	MaxScaleUpRate float64
	// Maximum rate at which to scale down, a value of 2.5 means the count can reduce to at most 2.5 times less than the current value in one step.
	MaxScaleDownRate float64
	// The current use per pod.
	currentUsePerPod float64
	// The minimum number of replicas to which the target can be scaled down.
	MinReplicas int32
	// The maximum number of replicas to which the target can be scaled up
	MaxReplicas int32
	// Tolerance for fluctuations in metrics before scaling up
	UpFluctuationTolerance float64
	// Tolerance for fluctuations in metrics before scaling down
	DownFluctuationTolerance float64
	// Cooldown window for scale-up decisions (prevents rapid scale-ups)
	ScaleUpCooldownWindow time.Duration
	// Cooldown window for scale-down decisions (prevents rapid scale-downs)
	ScaleDownCooldownWindow time.Duration
	// Scale to zero flag
	ScaleToZero bool
	// Panic threshold for KPA
	PanicThreshold float64

	// Stable and Panic values
	StableValue float64
	PanicValue  float64

	// MetricTargets used to store multiple metrics
	MetricTargets map[string]MetricTarget // key: metric name (e.g., "cpu", "gpu_cache_usage_perc")
}

type MetricTarget struct {
	// The value of scaling metric per pod that we target to maintain.
	TargetValue float64
	// The total value of scaling metric that a pod can maintain.
	TotalValue float64
	// The metric used for scaling, i.e. CPU, Memory, QPS.
	ScalingMetric string
	MetricType    autoscalingv1alpha1.MetricSourceType
}

var _ ScalingContext = (*baseScalingContext)(nil)

// NewBaseScalingContext creates a new instance of BaseScalingContext with default values.
func NewBaseScalingContext() *baseScalingContext {
	return &baseScalingContext{
		MaxScaleUpRate:           2,                 // Scale up rate of 200%, allowing rapid scaling
		MaxScaleDownRate:         2,                 // Scale down rate of 50%, for more gradual reduction
		UpFluctuationTolerance:   0.1,               // Default 10% tolerance for scale-up
		DownFluctuationTolerance: 0.1,               // Default 10% tolerance for scale-down
		ScaleUpCooldownWindow:    0 * time.Second,   // Default: no cooldown for scale-up
		ScaleDownCooldownWindow:  300 * time.Second, // Default: 5 minutes cooldown for scale-down
		ScaleToZero:              false,             // Default: do not scale to zero
		PanicThreshold:           2.0,               // Default panic threshold for KPA
		MetricTargets:            make(map[string]MetricTarget),
	}
}

// annotationParser parses one annotation value, checks that the autoscaler can
// use it, and stores it on the context. It must leave the context untouched
// when it returns an error.
type annotationParser func(b *baseScalingContext, value string) error

// annotationParsers maps annotation keys to their parsing functions
var annotationParsers = map[string]annotationParser{
	types.MaxScaleUpRateLabel: func(b *baseScalingContext, value string) error {
		v, err := parseScaleRate(value)
		if err == nil {
			b.MaxScaleUpRate = v
		}
		return err
	},
	types.MaxScaleDownRateLabel: func(b *baseScalingContext, value string) error {
		v, err := parseScaleRate(value)
		if err == nil {
			b.MaxScaleDownRate = v
		}
		return err
	},
	types.ScaleUpToleranceLabel: func(b *baseScalingContext, value string) error {
		v, err := parseTolerance(value)
		if err == nil {
			b.UpFluctuationTolerance = v
		}
		return err
	},
	types.ScaleDownToleranceLabel: func(b *baseScalingContext, value string) error {
		v, err := parseTolerance(value)
		if err == nil {
			b.DownFluctuationTolerance = v
		}
		return err
	},
	types.PanicThresholdLabel: func(b *baseScalingContext, value string) error {
		v, err := parseFiniteFloat(value)
		if err != nil {
			return err
		}
		if v <= 0 {
			return fmt.Errorf("must be greater than 0")
		}
		b.PanicThreshold = v
		return nil
	},
	types.ScaleUpCooldownWindowLabel: func(b *baseScalingContext, value string) error {
		v, err := parseCooldownWindow(value)
		if err == nil {
			b.ScaleUpCooldownWindow = v
		}
		return err
	},
	types.ScaleDownCooldownWindowLabel: func(b *baseScalingContext, value string) error {
		v, err := parseCooldownWindow(value)
		if err == nil {
			b.ScaleDownCooldownWindow = v
		}
		return err
	},
	types.ScaleToZeroLabel: func(b *baseScalingContext, value string) error {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("must be a boolean")
		}
		b.ScaleToZero = v
		return nil
	},
}

// parseFiniteFloat rejects NaN and infinities, which strconv.ParseFloat accepts
// and which would otherwise flow into every replica calculation.
func parseFiniteFloat(value string) (float64, error) {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("must be a finite number")
	}
	return v, nil
}

// parseScaleRate parses a max scale-up or scale-down rate. A rate is the factor
// by which replicas may change in one decision, so 1 means "no change" and
// anything lower has no meaning: KPA and APA divide by the scale-down rate and
// use the scale-up rate as an upper bound on the next replica count.
func parseScaleRate(value string) (float64, error) {
	v, err := parseFiniteFloat(value)
	if err != nil {
		return 0, err
	}
	if v < 1 {
		return 0, fmt.Errorf("must be greater than or equal to 1")
	}
	return v, nil
}

func parseTolerance(value string) (float64, error) {
	v, err := parseFiniteFloat(value)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return v, nil
}

func parseCooldownWindow(value string) (time.Duration, error) {
	v, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a duration such as 30s or 5m")
	}
	if v < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return v, nil
}

// AnnotationError reports one autoscaling annotation whose value cannot be used.
type AnnotationError struct {
	Key   string
	Value string
	Err   error
}

func (e AnnotationError) Error() string {
	return fmt.Sprintf("annotation %s: invalid value %q: %v", e.Key, e.Value, e.Err)
}

func (e AnnotationError) Unwrap() error {
	return e.Err
}

// ValidateAnnotations checks every autoscaling annotation the controller
// understands and returns one error per unusable value, ordered by key.
// Annotations the controller does not read are ignored.
func ValidateAnnotations(annotations map[string]string) []AnnotationError {
	return applyAnnotations(NewBaseScalingContext(), annotations)
}

// applyAnnotations stores every usable annotation value on b and returns the
// unusable ones. Keys are visited in sorted order and each key is independent
// of the others, so the outcome does not depend on map iteration order.
func applyAnnotations(b *baseScalingContext, annotations map[string]string) []AnnotationError {
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		if _, ok := annotationParsers[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var errs []AnnotationError
	for _, key := range keys {
		value := annotations[key]
		if err := annotationParsers[key](b, value); err != nil {
			errs = append(errs, AnnotationError{Key: key, Value: value, Err: err})
		}
	}
	return errs
}

// UpdateByPaTypes should be invoked in any scaling context that embeds BaseScalingContext.
func (b *baseScalingContext) UpdateByPaTypes(pa *autoscalingv1alpha1.PodAutoscaler) error {
	for _, ms := range pa.Spec.MetricsSources {
		targetValue, err := strconv.ParseFloat(ms.TargetValue, 64)
		if err != nil {
			klog.ErrorS(err, "Failed to parse target value", "metric", ms.TargetMetric, "value", ms.TargetValue)
			return fmt.Errorf("invalid targetValue for metric %q: %w", ms.TargetMetric, err)
		}
		if targetValue <= 0 {
			return fmt.Errorf("targetValue for metric %q must be greater than 0, got %f", ms.TargetMetric, targetValue)
		}

		b.MetricTargets[ms.TargetMetric] = MetricTarget{
			TargetValue:   targetValue,
			TotalValue:    100.0,
			ScalingMetric: ms.TargetMetric,
			MetricType:    ms.MetricSourceType,
		}
	}

	// Apply every usable annotation. An unusable value keeps the default for
	// that one setting and is reported, without affecting the others.
	if annotationErrs := applyAnnotations(b, pa.Annotations); len(annotationErrs) > 0 {
		errs := make([]error, 0, len(annotationErrs))
		for _, annotationErr := range annotationErrs {
			errs = append(errs, annotationErr)
		}
		return errors.Join(errs...)
	}
	return nil
}

func (b *baseScalingContext) SetCurrentUsePerPod(value float64) {
	b.currentUsePerPod = value
}

func (b *baseScalingContext) SetMinReplicas(minReplicas int32) {
	b.MinReplicas = minReplicas
}

func (b *baseScalingContext) SetMaxReplicas(maxReplicas int32) {
	b.MaxReplicas = maxReplicas
}

func (b *baseScalingContext) GetUpFluctuationTolerance() float64 {
	return b.UpFluctuationTolerance
}

func (b *baseScalingContext) GetDownFluctuationTolerance() float64 {
	return b.DownFluctuationTolerance
}

func (b *baseScalingContext) GetMaxScaleUpRate() float64 {
	return b.MaxScaleUpRate
}

func (b *baseScalingContext) GetMaxScaleDownRate() float64 {
	return b.MaxScaleDownRate
}

func (b *baseScalingContext) GetCurrentUsePerPod() float64 {
	return b.currentUsePerPod
}

func (b *baseScalingContext) GetTargetValueForMetric(metricName string) (float64, bool) {
	if target, ok := b.MetricTargets[metricName]; ok {
		return target.TargetValue, true
	}
	return 0, false
}

func (b *baseScalingContext) GetMetricSourceTypeForMetric(metricName string) (autoscalingv1alpha1.MetricSourceType, bool) {
	if target, ok := b.MetricTargets[metricName]; ok {
		return target.MetricType, true
	}
	return "", false
}

func (b *baseScalingContext) GetScalingTolerance() (up float64, down float64) {
	return b.MaxScaleUpRate, b.MaxScaleDownRate
}

func (b *baseScalingContext) GetMinReplicas() int32 {
	return b.MinReplicas
}

func (b *baseScalingContext) GetMaxReplicas() int32 {
	return b.MaxReplicas
}

func (b *baseScalingContext) GetStableValue() float64 {
	return b.StableValue
}

func (b *baseScalingContext) SetStableValue(value float64) {
	b.StableValue = value
}

func (b *baseScalingContext) GetPanicValue() float64 {
	return b.PanicValue
}

func (b *baseScalingContext) SetPanicValue(value float64) {
	b.PanicValue = value
}

func (b *baseScalingContext) GetActivationScale() int32 {
	return 1
}

func (b *baseScalingContext) SetActivationScale(value int32) {
	// No-op in base implementation
}

func (b *baseScalingContext) GetPanicThreshold() float64 {
	return b.PanicThreshold
}

func (b *baseScalingContext) GetScaleUpCooldownWindow() time.Duration {
	return b.ScaleUpCooldownWindow
}

func (b *baseScalingContext) GetScaleDownCooldownWindow() time.Duration {
	return b.ScaleDownCooldownWindow
}

func (b *baseScalingContext) GetScaleToZero() bool {
	return b.ScaleToZero
}
