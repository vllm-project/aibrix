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
	"errors"
	"math"
	"time"

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

// DeciderSpec defines parameters for scaling decisions.
type DeciderSpec struct {
	// Maximum rate at which to scale up
	MaxScaleUpRate float64
	// Maximum rate at which to scale down
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
	// ActivationScale must be >= 2.
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
	podCounter   int
	deciderSpec  *DeciderSpec
	Status       DeciderStatus
}

var _ Scaler = (*KpaAutoscaler)(nil)

func NewKpaAutoscaler(readyPodsCount int, spec *DeciderSpec, panicTime time.Time,
	maxPanicPods int32, delayWindow *aggregation.TimeWindow) (*KpaAutoscaler, error) {
	if spec == nil {
		return nil, errors.New("spec cannot be nil")
	}
	if delayWindow == nil {
		return nil, errors.New("delayWindow cannot be nil")
	}
	autoscaler := &Autoscaler{}
	return &KpaAutoscaler{
		Autoscaler:   autoscaler,
		podCounter:   readyPodsCount,
		panicTime:    panicTime,
		maxPanicPods: maxPanicPods,
		delayWindow:  delayWindow,
		deciderSpec:  spec,
	}, nil
}

// Scale implements Scaler interface in KpaAutoscaler.
func (k *KpaAutoscaler) Scale(observedStableValue float64, observedPanicValue float64, now time.Time) ScaleResult {
	/**
	`observedStableValue` and `observedPanicValue` are calculated using different window sizes in the `MetricClient`.
	 For reference, see the KNative implementation at `pkg/autoscaler/metrics/collector.go：185`.
	*/
	k.specMux.RLock()
	spec := k.deciderSpec
	k.specMux.RUnlock()

	originalReadyPodsCount := k.podCounter
	// Use 1 if there are zero current pods.
	readyPodsCount := math.Max(1, float64(originalReadyPodsCount))

	// TODO if we add metricClient into Autoscaler, we can uncomment following codes, then get
	//  observedStableValue and observedPanicValue from metricClient. It could be more elegant.
	//metricName := spec.ScalingMetric
	//switch metricName {
	//case v1alpha1.CPU:
	//	observedStableValue, observedPanicValue, err = a.metricClient.GetCPU(now)
	//default:
	//	observedStableValue, observedPanicValue, err = a.metricClient.GetCPU(now)
	//}

	maxScaleUp := math.Ceil(spec.MaxScaleUpRate * readyPodsCount)
	maxScaleDown := math.Floor(readyPodsCount / spec.MaxScaleDownRate)

	dspc := math.Ceil(observedStableValue / spec.TargetValue)
	dppc := math.Ceil(observedPanicValue / spec.TargetValue)

	// We want to keep desired pod count in the  [maxScaleDown, maxScaleUp] range.
	desiredStablePodCount := int32(math.Min(math.Max(dspc, maxScaleDown), maxScaleUp))
	desiredPanicPodCount := int32(math.Min(math.Max(dppc, maxScaleDown), maxScaleUp))

	//	If ActivationScale > 1, then adjust the desired pod counts
	if k.deciderSpec.ActivationScale > 1 {
		if dspc > 0 && k.deciderSpec.ActivationScale > desiredStablePodCount {
			desiredStablePodCount = k.deciderSpec.ActivationScale
		}
		if dppc > 0 && k.deciderSpec.ActivationScale > desiredPanicPodCount {
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
			klog.InfoS("Skipping pod count decrease from - to - ", "a.maxPanicPods", k.maxPanicPods, "desiredPodCount", desiredPodCount)
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
		k.delayWindow.Record(now, desiredPodCount)
		delayedPodCount := k.delayWindow.Max()
		if delayedPodCount != desiredPodCount {
			klog.InfoS(
				"Delaying scale to %d, staying at %d",
				"desiredPodCount", desiredPodCount, "delayedPodCount", delayedPodCount,
			)
			desiredPodCount = delayedPodCount
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
