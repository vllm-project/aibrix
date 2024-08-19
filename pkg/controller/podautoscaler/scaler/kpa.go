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
	"log"
	"math"
	"time"
)

/**
This implementation of the algorithm is based on both the Knative KPA code and its documentation.

According to Knative documentation, the KPA Scale policy includes both a stable mode and a panic mode.
If the metric usage does not exceed the panic threshold, KPA tries to align the per-pod metric usage with the stable target value.
If metric usage exceeds the panic target during the panic window, KPA enters panic mode and tries to maintain the per-pod metric usage at the panic target.
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

This implementation is inspired by the scaling solutions provided by Knative.
Our implementation specifically mimics and adapts the autoscaling functionality found in:

- ScalePolicy:	pkg/autoscaler/scaling/autoscaler.go
- DeciderSpec:	pkg/autoscaler/scaling/multiscaler.go
- ScaleResult:	pkg/autoscaler/scaling/multiscaler.go
- autoscaler:		pkg/autoscaler/scaling/autoscaler.go

*/

// Scale computes the desired scale based on current metrics.
func (a *Autoscaler) Scale(readyPodsCount int32, observedStableValue float64, observedPanicValue float64, now time.Time) ScaleResult {
	/**
	1. `observedStableValue` and `observedPanicValue` are calculated using different window sizes in the `MetricClient`.
		For reference, see the KNative implementation at `pkg/autoscaler/metrics/collector.go：185`.
	2. In KPA, `readyPodsCount` is obtained from `autoscaler.podCounter.ReadyCount`. It's not a big issue, we can change it.
	*/
	a.specMux.RLock()
	spec := a.deciderSpec
	a.specMux.RUnlock()

	// Ensure there is at least one pod.
	readyCount := math.Max(1, float64(readyPodsCount))

	maxScaleUp := math.Ceil(spec.MaxScaleUpRate * readyCount)
	maxScaleDown := math.Floor(readyCount / spec.MaxScaleDownRate)

	desiredStablePodCount := int32(math.Min(math.Max(math.Ceil(observedStableValue/spec.TargetValue), maxScaleDown), maxScaleUp))
	desiredPanicPodCount := int32(math.Min(math.Max(math.Ceil(observedPanicValue/spec.TargetValue), maxScaleDown), maxScaleUp))

	isOverPanicThreshold := (observedPanicValue/spec.TargetValue)/readyCount >= spec.PanicThreshold

	if a.panicTime.IsZero() && isOverPanicThreshold {
		log.Println("Entering panic mode.")
		a.panicTime = now
	} else if !a.panicTime.IsZero() {
		if isOverPanicThreshold {
			log.Println("Continuing in panic mode.")
			a.panicTime = now
		} else if now.After(a.panicTime.Add(spec.StableWindow)) {
			log.Println("Exiting panic mode.")
			a.panicTime = time.Time{}
		}
	}

	if spec.ActivationScale > 1 {
		if desiredStablePodCount < spec.ActivationScale {
			desiredStablePodCount = spec.ActivationScale
		}
		if desiredPanicPodCount < spec.ActivationScale {
			desiredPanicPodCount = spec.ActivationScale
		}
	}

	desiredPodCount := desiredStablePodCount
	if !a.panicTime.IsZero() && desiredPodCount < desiredPanicPodCount {
		desiredPodCount = desiredPanicPodCount
		a.maxPanicPods = desiredPodCount
	}

	if spec.ScaleDownDelay > 0 && time.Since(a.delayWindow.lastRecorded) < spec.ScaleDownDelay {
		desiredPodCount = a.delayWindow.current()
	} else {
		a.delayWindow.record(now, desiredPodCount)
	}

	excessBCF := int32(math.Floor(float64(readyPodsCount)*spec.TotalValue - spec.TargetValue*float64(desiredPodCount)))

	return ScaleResult{
		DesiredPodCount:     desiredPodCount,
		ExcessBurstCapacity: excessBCF,
		ScaleValid:          true,
	}
}
