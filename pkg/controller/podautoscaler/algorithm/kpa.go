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
	"errors"
	"fmt"
	"math"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"k8s.io/klog/v2"
)

// KPAAlgorithm implements Knative-style Pod Autoscaling with panic and stable windows.
// The struct holds no state. The panic window that has to outlive one
// recommendation is kept per target by the caller and passed in as
// ScalingRequest.Panic.
type KPAAlgorithm struct{}

var _ ScalingAlgorithm = (*KPAAlgorithm)(nil)

const (
	MinScaleRate     = 1.0
	DefaultTolerance = 0.0
)

// PanicState is the panic-mode memory of one KPA target, as in Knative's
// autoscaler. The zero value means the target is not panicking.
type PanicState struct {
	// PanicTime is the last time the panic window was over the panic
	// threshold. It is zero while the target is not panicking.
	PanicTime time.Time
	// MaxPanicPods is the highest pod count recommended during the current
	// panic. Recommendations never go below it until the panic ends.
	MaxPanicPods int32
}

// kpaPodCounts is what the stable and panic windows each ask for.
type kpaPodCounts struct {
	stable, panic int32
	// overPanicThreshold reports whether the panic window asks for at least
	// PanicThreshold times the current pods.
	overPanicThreshold bool
}

// ComputeRecommendation calculates desired replica count for KPA strategy
func (a *KPAAlgorithm) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	metrics := request.AggregatedMetrics

	// Update context with current metrics
	request.ScalingContext.SetStableValue(metrics.StableValue)
	request.ScalingContext.SetPanicValue(metrics.PanicValue)

	metricName := request.AggregatedMetrics.MetricKey.MetricName
	loadPods := request.LoadPods
	if loadPods <= 0 {
		loadPods = float64(request.CurrentReplicas)
	}
	counts, ok := a.desiredPodCounts(float64(request.CurrentReplicas), loadPods, request.ScalingContext, metricName)
	if !ok {
		return nil, fmt.Errorf("no target value for metric %q", metricName)
	}

	state := request.Panic
	if state == nil {
		// Without a caller-kept state every recommendation starts outside a
		// panic, so only the current panic window can trigger one.
		state = &PanicState{}
	}
	desiredReplicas, inPanic := state.apply(request.Timestamp, request.StableWindow, counts)

	mode, currentValue := "stable", metrics.StableValue
	if inPanic {
		mode, currentValue = "panic", metrics.PanicValue
	}

	// Apply constraints from ScalingContext
	desiredReplicas = applyConstraints(desiredReplicas, request.ScalingContext)

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

// GetAlgorithmType returns the algorithm type
func (a *KPAAlgorithm) GetAlgorithmType() string {
	return "kpa"
}

// apply advances the panic state to now and returns the pod count to
// recommend and whether the target is panicking, following Knative:
//   - a panic starts, or is extended, whenever the panic window asks for at
//     least PanicThreshold times the current pods;
//   - it ends once a whole stable window has passed without that, by which
//     time the surge is part of the stable metric;
//   - while it lasts the larger of the two windows is used and the
//     recommendation never goes down.
func (s *PanicState) apply(now time.Time, stableWindow time.Duration, counts kpaPodCounts) (int32, bool) {
	switch {
	case counts.overPanicThreshold:
		s.PanicTime = now
	case !s.PanicTime.IsZero() && s.PanicTime.Add(stableWindow).Before(now):
		s.PanicTime = time.Time{}
		s.MaxPanicPods = 0
	}

	desired := counts.stable
	if s.PanicTime.IsZero() {
		klog.V(4).InfoS("Operating in stable mode.", "desiredPodCount", desired)
		return desired, false
	}

	// In some edge cases the stable window asks for more than the panic one,
	// and we provision for whichever is larger.
	if desired < counts.panic {
		desired = counts.panic
	}
	if desired > s.MaxPanicPods {
		klog.InfoS("Operating in panic mode, increasing pods count.", "previous", s.MaxPanicPods, "desiredPodCount", desired)
		s.MaxPanicPods = desired
	} else if desired < s.MaxPanicPods {
		klog.InfoS("Operating in panic mode, skipping pod count decrease.", "current", s.MaxPanicPods, "desired", desired)
	}
	return s.MaxPanicPods, true
}

// IsPerPodSource reports whether the source's windowed value is a per-pod mean.
func IsPerPodSource(t autoscalingv1alpha1.MetricSourceType) bool {
	return t != autoscalingv1alpha1.EXTERNAL && t != autoscalingv1alpha1.DOMAIN
}

// podCountEpsilon absorbs floating-point error in a desired pod count before
// it is rounded up: 10 × 0.84 / 0.7 evaluates to 12.000000000000002, and a
// plain math.Ceil would ask for 13 pods instead of 12.
const podCountEpsilon = 1e-9

// CeilPodCount rounds a desired pod count up to a whole pod, treating a value
// within podCountEpsilon of an integer as that integer. A count in
// (0, podCountEpsilon] therefore rounds to 0 pods, not 1; that is far below any
// real demand.
func CeilPodCount(pods float64) float64 {
	return math.Ceil(pods - podCountEpsilon)
}

// desiredPodCounts computes the pod counts the stable and panic windows ask
// for. Tolerance is applied to prevent scaling for minor metric fluctuations.
// ok is false when the context has no target for the metric.
func (a *KPAAlgorithm) desiredPodCounts(currentPodCount, loadPodCount float64, context scalingctx.ScalingContext, metricsName string) (kpaPodCounts, bool) {
	// Get all the necessary values from context
	observedStableValue := context.GetStableValue()
	observedPanicValue := context.GetPanicValue()
	targetValue, ok := context.GetTargetValueForMetric(metricsName)
	if !ok {
		klog.ErrorS(errors.New("get no TargetValueForMetric"), "KPA Details", "metricsName", metricsName)
		return kpaPodCounts{}, false
	}
	panicThreshold := context.GetPanicThreshold()
	upTolerance := context.GetUpFluctuationTolerance()
	downTolerance := context.GetDownFluctuationTolerance()
	// Original scaling rate calculations
	readyPodsCount := math.Max(0, currentPodCount)                                   // A little sanitizing.
	maxScaleUp := math.Max(1, math.Ceil(context.GetMaxScaleUpRate()*readyPodsCount)) // Keep scale up non zero
	maxScaleDown := math.Floor(readyPodsCount / context.GetMaxScaleDownRate())       // Make scale down zero-able

	// Establish scale-up and scale-down thresholds using tolerances
	scaleUpThreshold := targetValue * (1 + upTolerance)
	scaleDownThreshold := targetValue * (1 - downTolerance)

	// Pod, resource and custom sources give a per-pod mean, so the total load is
	// the mean times the pods carrying it. Pods still starting carry none, and
	// counting them would ask for more pods than the load needs; a panic would
	// then hold that count after they are ready. External and domain sources
	// already report one total.
	//
	// The tolerance band is per pod, so a total is judged per pod too, the way
	// APA does: otherwise a total near one pod's target holds any replica count.
	// With no pods, any load is outside the band so that it activates one.
	load := 1.0
	tolerancePods := math.Max(1, currentPodCount)
	perPodSource := false
	if sourceType, _ := context.GetMetricSourceTypeForMetric(metricsName); IsPerPodSource(sourceType) {
		load = math.Max(1, loadPodCount)
		tolerancePods = 1
		perPodSource = true
	}
	outsideTolerance := func(observed float64) bool {
		if !perPodSource && currentPodCount == 0 && observed > 0 {
			return true
		}
		v := observed / tolerancePods
		return v > scaleUpThreshold || v < scaleDownThreshold
	}

	// Calculate desired replicas based on tolerance-adjusted targets
	var dspc, dppc float64
	if outsideTolerance(observedStableValue) {
		dspc = CeilPodCount(load * observedStableValue / targetValue)
	} else {
		// Within tolerance, maintain current replica count
		dspc = currentPodCount
	}

	if outsideTolerance(observedPanicValue) {
		dppc = CeilPodCount(load * observedPanicValue / targetValue)
	} else {
		// Within tolerance, maintain current replica count
		dppc = currentPodCount
	}

	// We want to keep desired pod count in the [maxScaleDown, maxScaleUp] range.
	desiredStablePodCount := int32(math.Min(math.Max(dspc, maxScaleDown), maxScaleUp))
	desiredPanicPodCount := int32(math.Min(math.Max(dppc, maxScaleDown), maxScaleUp))

	// If ActivationScale > 1, then adjust the desired pod counts
	activationScale := context.GetActivationScale()
	if activationScale > 1 {
		// ActivationScale only makes sense if activated (desired > 0)
		if activationScale > desiredStablePodCount && desiredStablePodCount > 0 {
			desiredStablePodCount = activationScale
		}
		if activationScale > desiredPanicPodCount && desiredPanicPodCount > 0 {
			desiredPanicPodCount = activationScale
		}
	}

	// Knative compares what the panic window asks for with the pods there are
	// now. readyPodsCount can be 0, so use max(1, readyPodsCount).
	isOverPanicThreshold := dppc/math.Max(1, readyPodsCount) >= panicThreshold

	// TODO: hard to distinguish the deployment from the logs
	klog.V(4).InfoS("KPA Details", "readyPodsCount", readyPodsCount,
		"MaxScaleUpRate", context.GetMaxScaleUpRate(), "MaxScaleDownRate", context.GetMaxScaleDownRate(),
		"TargetValue", targetValue, "PanicThreshold", panicThreshold,
		"UpTolerance", upTolerance, "DownTolerance", downTolerance,
		"dppc", dppc, "dspc", dspc, "desiredStablePodCount", desiredStablePodCount,
		"desiredPanicPodCount", desiredPanicPodCount, "isOverPanicThreshold", isOverPanicThreshold,
	)

	return kpaPodCounts{
		stable:             desiredStablePodCount,
		panic:              desiredPanicPodCount,
		overPanicThreshold: isOverPanicThreshold,
	}, true
}
