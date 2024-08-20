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

package kpa

import (
	"errors"
	"math"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"k8s.io/klog/v2"
)

/**
This implementation of the algorithm is based on both the Knative KpaScaler code and its documentation.

According to Knative documentation, the KpaScaler Scale policy includes both a stable mode and a panic mode.
If the metric usage does not exceed the panic threshold, KpaScaler tries to align the per-pod metric usage with the stable target value.
If metric usage exceeds the panic target during the panic window, KpaScaler enters panic mode and tries to maintain the per-pod metric usage at the panic target.
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

type KpaScaler struct {
	scaler       *Autoscaler
	panicTime    time.Time
	maxPanicPods int32
	delayWindow  *aggregation.TimeWindow
}

func NewKpaScaler(readyPodsCount int, spec *DeciderSpec, panicTime time.Time,
	maxPanicPods int32, delayWindow *aggregation.TimeWindow) (*KpaScaler, error) {
	if spec == nil {
		return nil, errors.New("spec cannot be nil")
	}
	if delayWindow == nil {
		return nil, errors.New("delayWindow cannot be nil")
	}
	scaler := &Autoscaler{
		podCounter:  readyPodsCount,
		deciderSpec: spec,
	}
	return &KpaScaler{
		scaler:       scaler,
		panicTime:    panicTime,
		maxPanicPods: maxPanicPods,
		delayWindow:  delayWindow,
	}, nil
}

// Scale implements Scaler interface in KpaScaler.
func (k *KpaScaler) Scale(observedStableValue float64, observedPanicValue float64, now time.Time) ScaleResult {
	/**
	`observedStableValue` and `observedPanicValue` are calculated using different window sizes in the `MetricClient`.
	 For reference, see the KNative implementation at `pkg/autoscaler/metrics/collector.go：185`.
	*/
	a := k.scaler
	a.specMux.RLock()
	spec := a.deciderSpec
	a.specMux.RUnlock()

	originalReadyPodsCount := a.podCounter
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
	if a.deciderSpec.ActivationScale > 1 {
		if dspc > 0 && a.deciderSpec.ActivationScale > desiredStablePodCount {
			desiredStablePodCount = a.deciderSpec.ActivationScale
		}
		if dppc > 0 && a.deciderSpec.ActivationScale > desiredPanicPodCount {
			desiredPanicPodCount = a.deciderSpec.ActivationScale
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
