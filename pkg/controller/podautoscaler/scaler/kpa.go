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
	"errors"
	"math"
	"strconv"
	"time"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	v1 "k8s.io/api/core/v1"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"k8s.io/klog/v2"
)

/**
This implementation of the algorithm is based on both the Knative KpaAutoscaler code and its documentation.

According to Knative documentation, the KpaAutoscaler Scale policy includes both a stable mode and a panic mode.
If the metric usage does not exceed the panic threshold, KpaAutoscaler tries to align the per-pod metric usage with the stable target value.
If metric usage exceeds the panic target during the panic window, KpaAutoscaler enters panic mode and tries to maintain the per-pod metric usage at the panic target.
If the metric no longer exceeds the panic threshold, exit the panic mode.

                                                       |
                                  Panic Target--->  +--| 20
                                                    |  |
                                                    | <------Panic Window
                                                    |  |
       Stable Target--->  +-------------------------|--| 10   CONCURRENCY
                          |                         |  |
                          |                      <-----------Stable Window
                          |                         |  |
--------------------------+-------------------------+--+ 0
120                       60                           0

*/

// DeciderKpaSpec defines parameters for scaling decisions.
type DeciderKpaSpec struct {
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
	// The burst capacity that user wants to maintain without queuing at the POD level.
	// Note, that queueing still might happen due to the non-ideal load balancing.
	TargetBurstCapacity float64
	// ActivationScale is the minimum, non-zero value that a service should scale to.
	// For example, if ActivationScale = 2, when a service scaled from zero it would
	// scale up two replicas in this case. In essence, this allows one to set both a
	// min-scale value while also preserving the ability to scale to zero.
	// ActivationScale must be >= 1.
	ActivationScale int32

	// TODO: Note that the following attributes are specific to Knative; but we retain them here temporarily.
	// PanicThreshold is the threshold at which panic mode is entered. It represents
	// a factor of the currently observed load over the panic window over the ready
	// pods. I.e. if this is 2, panic mode will be entered if the observed metric
	// is twice as high as the current population can handle.
	PanicThreshold float64
	// StableWindow is needed to determine when to exit panic mode.
	StableWindow time.Duration
	// ScaleDownDelay is the time that must pass at reduced concurrency before a
	// scale-down decision is applied.
	ScaleDownDelay time.Duration

	// The two following attributes are specific to APA. We may separate them from DeciderKpaSpec later.
	// UpFluctuationTolerance represents the threshold before scaling up,
	// which means no scaling up will occur unless the currentMetricValue exceeds the TargetValue by more than UpFluctuationTolerance
	// UpFluctuationTolerance represents the threshold before scaling down,
	// which means no scaling down will occur unless the currentMetricValue is less than the TargetValue by more than UpFluctuationTolerance
	UpFluctuationTolerance   float64
	DownFluctuationTolerance float64
}

// NewDefaultDeciderKpaSpec references KPA and sets up a default configuration.
func NewDefaultDeciderKpaSpec() *DeciderKpaSpec {
	return &DeciderKpaSpec{
		MaxScaleUpRate:           2,                // Scale up rate of 200%, allowing rapid scaling
		MaxScaleDownRate:         2,                // Scale down rate of 50%, for more gradual reduction
		ScalingMetric:            "CPU",            // Metric used for scaling, here set to CPU utilization
		TargetValue:              30.0,             // Target CPU utilization set at 10%
		TotalValue:               100.0,            // Total CPU utilization capacity for pods is 100%
		TargetBurstCapacity:      2.0,              // Target burst capacity to handle sudden spikes
		ActivationScale:          1,                // Initial scaling factor upon activation
		PanicThreshold:           2.0,              // Panic threshold set at 200% to trigger rapid scaling
		StableWindow:             60 * time.Second, // Time window to stabilize before altering scale
		ScaleDownDelay:           30 * time.Minute, // Delay before scaling down to avoid flapping
		UpFluctuationTolerance:   0.1,              // Tolerance for scaling up, set at 10%
		DownFluctuationTolerance: 0.2,              // Tolerance for scaling up, set at 10%
	}
}

// DeciderStatus is the current scale recommendation.
type DeciderStatus struct {
	// DesiredScale is the target number of instances that autoscaler
	// this revision needs.
	DesiredScale int32

	// TODO: ExcessBurstCapacity might be a general attribute since it describes
	//  how much capacity users want to keep for preparing for burst traffic.

	// ExcessBurstCapacity is the difference between spare capacity
	// (how much more load the pods in the revision deployment can take before being
	// overloaded) and the configured target burst capacity.
	// If this number is negative: Activator will be threaded in
	// the request path by the PodAutoscaler controller.
	ExcessBurstCapacity int32
}

type KpaAutoscaler struct {
	*Autoscaler
	panicTime    time.Time
	maxPanicPods int32
	delayWindow  *aggregation.TimeWindow
	deciderSpec  *DeciderKpaSpec
	Status       DeciderStatus
}

var _ Scaler = (*KpaAutoscaler)(nil)

// NewKpaAutoscaler Initialize KpaAutoscaler: Referenced from `knative/pkg/autoscaler/scaling/autoscaler.go newAutoscaler`
func NewKpaAutoscaler(readyPodsCount int, spec *DeciderKpaSpec) (*KpaAutoscaler, error) {
	if spec == nil {
		return nil, errors.New("spec cannot be nil")
	}

	// Create a new delay window based on the ScaleDownDelay specified in the spec
	if spec.ScaleDownDelay <= 0 {
		return nil, errors.New("ScaleDownDelay must be positive")
	}
	delayWindow := aggregation.NewTimeWindow(spec.ScaleDownDelay, 1*time.Second)

	// As KNative stated:
	//   We always start in the panic mode, if the deployment is scaled up over 1 pod.
	//   If the scale is 0 or 1, normal Autoscaler behavior is fine.
	//   When Autoscaler restarts we lose metric history, which causes us to
	//   momentarily scale down, and that is not a desired behaviour.
	//   Thus, we're keeping at least the current scale until we
	//   accumulate enough data to make conscious decisions.
	var panicTime time.Time
	if readyPodsCount > 1 {
		panicTime = time.Now()
	} else {
		panicTime = time.Time{} // Zero value for time if not in panic mode
	}

	// TODO missing MetricClient
	metricsClient := metrics.NewKPAMetricsClient()
	autoscaler := &Autoscaler{metricsClient: metricsClient}

	return &KpaAutoscaler{
		Autoscaler:   autoscaler,
		panicTime:    panicTime,
		maxPanicPods: int32(readyPodsCount),
		delayWindow:  delayWindow,
		deciderSpec:  spec,
	}, nil
}

// APA_Scale references and enhances the algorithm in the following paper:.
//
//	 Huo, Qizheng, et al. "High Concurrency Response Strategy based on Kubernetes Horizontal Pod Autoscaler."
//		Journal of Physics: Conference Series. Vol. 2451. No. 1. IOP Publishing, 2023.
func (k *KpaAutoscaler) APA_Scale(currentPodCount float64, currentUsePerPod float64, spec *DeciderKpaSpec) int32 {
	expectedUse := spec.TargetValue
	upTolerance := spec.UpFluctuationTolerance
	downTolerance := spec.DownFluctuationTolerance

	// Check if scaling up is necessary
	if currentUsePerPod/expectedUse > (1 + upTolerance) {
		maxScaleUp := math.Ceil(spec.MaxScaleUpRate * currentPodCount)
		expectedPods := int32(math.Ceil(currentPodCount * (currentUsePerPod / expectedUse)))
		// Ensure the number of pods does not exceed the maximum scale-up limit
		if float64(expectedPods) > maxScaleUp {
			expectedPods = int32(maxScaleUp)
		}
		return expectedPods
	} else if currentUsePerPod/expectedUse < (1 - downTolerance) { // Check if scaling down is necessary
		maxScaleDown := math.Floor(currentPodCount / spec.MaxScaleDownRate)
		expectedPods := int32(math.Ceil(currentPodCount * (currentUsePerPod / expectedUse)))
		// Ensure the number of pods does not fall below the minimum scale-down limit
		if float64(expectedPods) < maxScaleDown {
			expectedPods = int32(maxScaleDown)
		}
		return expectedPods
	}

	// If the current utilization is within the expected range, maintain the current pod count
	return int32(currentPodCount)
}

// Scale implements Scaler interface in KpaAutoscaler.
// Refer to knative-serving: pkg/autoscaler/scaling/autoscaler.go, Scale function.
func (k *KpaAutoscaler) Scale(originalReadyPodsCount int, metricKey metrics.NamespaceNameMetric, now time.Time, strategy autoscalingv1alpha1.ScalingStrategyType) ScaleResult {
	/**
	`observedStableValue` and `observedPanicValue` are calculated using different window sizes in the `MetricClient`.
	 For reference, see the KNative implementation at `pkg/autoscaler/metrics/collector.go：185`.
	*/
	spec := k.GetSpec()

	kpaMetricsClient := k.metricsClient.(*metrics.KPAMetricsClient)
	observedStableValue, observedPanicValue, err := kpaMetricsClient.StableAndPanicMetrics(metricKey, now)
	if err != nil {
		klog.Errorf("Failed to get stable and panic metrics for %s: %v", metricKey, err)
		return ScaleResult{}
	}

	if strategy == autoscalingv1alpha1.APA {
		currentUsePerPod := observedPanicValue / float64(originalReadyPodsCount)
		desiredPodCount := k.APA_Scale(float64(originalReadyPodsCount), currentUsePerPod, spec)
		klog.InfoS("Use APA scaling strategy", "currentPodCount", originalReadyPodsCount, "currentUsePerPod", currentUsePerPod, "desiredPodCount", desiredPodCount)
		return ScaleResult{
			DesiredPodCount:     desiredPodCount,
			ExcessBurstCapacity: 0,
			ScaleValid:          true,
		}
	}

	// Use 1 if there are zero current pods.
	readyPodsCount := math.Max(1, float64(originalReadyPodsCount))

	maxScaleUp := math.Ceil(spec.MaxScaleUpRate * readyPodsCount)
	maxScaleDown := math.Floor(readyPodsCount / spec.MaxScaleDownRate)

	dspc := math.Ceil(observedStableValue / spec.TargetValue)
	dppc := math.Ceil(observedPanicValue / spec.TargetValue)

	// We want to keep desired pod count in the  [maxScaleDown, maxScaleUp] range.
	desiredStablePodCount := int32(math.Min(math.Max(dspc, maxScaleDown), maxScaleUp))
	desiredPanicPodCount := int32(math.Min(math.Max(dppc, maxScaleDown), maxScaleUp))

	//	If ActivationScale > 1, then adjust the desired pod counts
	if k.deciderSpec.ActivationScale > 1 {
		if k.deciderSpec.ActivationScale > desiredStablePodCount {
			desiredStablePodCount = k.deciderSpec.ActivationScale
		}
		if k.deciderSpec.ActivationScale > desiredPanicPodCount {
			desiredPanicPodCount = k.deciderSpec.ActivationScale
		}
	}

	isOverPanicThreshold := dppc/readyPodsCount >= spec.PanicThreshold

	if k.panicTime.IsZero() && isOverPanicThreshold {
		// Begin panicking when we cross the threshold in the panic window.
		klog.InfoS("Begin panicking")
		k.panicTime = now
	} else if isOverPanicThreshold {
		// If we're still over panic threshold right now — extend the panic window.
		k.panicTime = now
	} else if !k.panicTime.IsZero() && !isOverPanicThreshold && k.panicTime.Add(spec.StableWindow).Before(now) {
		// Stop panicking after the surge has made its way into the stable metric.
		klog.InfoS("Exit panicking.")
		k.panicTime = time.Time{}
		k.maxPanicPods = 0
	}

	desiredPodCount := desiredStablePodCount
	if !k.panicTime.IsZero() {
		// In some edgecases stable window metric might be larger
		// than panic one. And we should provision for stable as for panic,
		// so pick the larger of the two.
		if desiredPodCount < desiredPanicPodCount {
			desiredPodCount = desiredPanicPodCount
		}
		klog.InfoS("Operating in panic mode.")
		// We do not scale down while in panic mode. Only increases will be applied.
		if desiredPodCount > k.maxPanicPods {
			klog.InfoS("Increasing pods count.", "originalPodCount", originalReadyPodsCount, "desiredPodCount", desiredPodCount)
			k.maxPanicPods = desiredPodCount
		} else if desiredPodCount < k.maxPanicPods {
			klog.InfoS("Skipping pod count decrease", "current", k.maxPanicPods, "desired", desiredPodCount)
		}
		desiredPodCount = k.maxPanicPods
	} else {
		klog.InfoS("Operating in stable mode.")
	}

	// Delay scale down decisions, if a ScaleDownDelay was specified.
	// We only do this if there's a non-nil delayWindow because although a
	// one-element delay window is _almost_ the same as no delay at all, it is
	// not the same in the case where two Scale()s happen in the same time
	// interval (because the largest will be picked rather than the most recent
	// in that case).
	if k.delayWindow != nil {
		k.delayWindow.Record(now, float64(desiredPodCount))
		delayedPodCount, err := k.delayWindow.Max()
		if err != nil {
			klog.ErrorS(err, "Failed to get delayed pod count")
			return ScaleResult{}
		}
		if int32(delayedPodCount) != desiredPodCount {
			klog.InfoS(
				"Delaying scale to %d, staying at %d",
				"desiredPodCount", desiredPodCount, "delayedPodCount", delayedPodCount,
			)
			desiredPodCount = int32(delayedPodCount)
		}
	}

	// Compute excess burst capacity
	//
	// the excess burst capacity is based on panic value, since we don't want to
	// be making knee-jerk decisions about Activator in the request path.
	// Negative EBC means that the deployment does not have enough capacity to serve
	// the desired burst off hand.
	// EBC = TotCapacity - Cur#ReqInFlight - TargetBurstCapacity
	excessBCF := -1.
	switch {
	case spec.TargetBurstCapacity == 0:
		excessBCF = 0
	case spec.TargetBurstCapacity > 0:
		totCap := float64(originalReadyPodsCount) * spec.TotalValue
		excessBCF = math.Floor(totCap - spec.TargetBurstCapacity - observedPanicValue)
	}

	return ScaleResult{
		DesiredPodCount:     desiredPodCount,
		ExcessBurstCapacity: int32(excessBCF),
		ScaleValid:          true,
	}
}

func (k *KpaAutoscaler) UpdatePodListMetric(ctx context.Context, metricKey metrics.NamespaceNameMetric, list *v1.PodList, port int, now time.Time) {
	err := k.metricsClient.UpdatePodListMetric(ctx, metricKey, list, port, now)
	if err != nil {
		return
	}
}

func (k *KpaAutoscaler) UpdateSpec(pa autoscalingv1alpha1.PodAutoscaler) {
	k.specMux.Lock()
	defer k.specMux.Unlock()

	targetValue, err := strconv.ParseFloat(pa.Spec.TargetValue, 64)
	if err != nil {
		klog.ErrorS(err, "Failed to parse target value", "targetValue", pa.Spec.TargetValue)
		return
	}
	k.deciderSpec.TargetValue = targetValue
	k.deciderSpec.ScalingMetric = pa.Spec.TargetMetric
}

func (k *KpaAutoscaler) GetSpec() *DeciderKpaSpec {
	k.specMux.Lock()
	defer k.specMux.Unlock()

	return k.deciderSpec
}
