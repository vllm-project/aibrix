package main

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

import (
	"log"
	"math"
	"sync"
	"time"
)

// DeciderSpec defines parameters for scaling decisions.
type DeciderSpec struct {
	MaxScaleUpRate   float64       // Maximum rate at which to scale up
	MaxScaleDownRate float64       // Maximum rate at which to scale down
	TargetValue      float64       // Target metric value per pod
	TotalValue       float64       // Total metric capacity per pod
	PanicThreshold   float64       // Threshold to enter panic mode
	StableWindow     time.Duration // Duration to stabilize before exiting panic mode
	ScaleDownDelay   time.Duration // Delay before applying scale-down
	ActivationScale  int32         // Minimum scale at which to activate the service
}

// ScaleResult contains the results of a scaling decision.
type ScaleResult struct {
	DesiredPodCount     int32 // Suggested number of pods
	ExcessBurstCapacity int32 // Calculated excess capacity
	ScaleValid          bool  // Validity of the scale result
}

// autoscaler represents an instance of the autoscaling engine.
type autoscaler struct {
	specMux      sync.RWMutex
	deciderSpec  *DeciderSpec
	panicTime    time.Time
	maxPanicPods int32
	delayWindow  *timeWindow
}

// timeWindow is used to manage delay in scale-down decisions.
type timeWindow struct {
	lastRecorded time.Time
	count        int32
}

// record updates the count in the time window.
func (tw *timeWindow) record(currentTime time.Time, count int32) {
	tw.lastRecorded = currentTime
	tw.count = count
}

// current returns the current count in the time window.
func (tw *timeWindow) current() int32 {
	return tw.count
}

// NewAutoscaler creates a new instance of the autoscaler.
func NewAutoscaler(spec *DeciderSpec) *autoscaler {
	return &autoscaler{
		deciderSpec: spec,
		delayWindow: &timeWindow{},
	}
}

// Scale computes the desired scale based on current metrics.
func (a *autoscaler) Scale(readyPodsCount int32, observedStableValue, observedPanicValue float64, now time.Time) ScaleResult {
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

func main() {
	as := NewAutoscaler(&DeciderSpec{
		MaxScaleUpRate:   1.5,
		MaxScaleDownRate: 0.75,
		TargetValue:      100,
		TotalValue:       500,
		PanicThreshold:   2.0,
		StableWindow:     1 * time.Minute,
		ScaleDownDelay:   1 * time.Minute,
		ActivationScale:  2,
	})

	readyPodsCount := int32(5)
	observedStableValue := 120.0
	observedPanicValue := 240.0
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		log.Printf("Scaling evaluation at %s", now)
		result := as.Scale(readyPodsCount, observedStableValue, observedPanicValue, now)
		log.Printf("Scale result: Desired Pod Count = %d, Excess Burst Capacity = %d, Valid = %v", result.DesiredPodCount, result.ExcessBurstCapacity, result.ScaleValid)

		// Stop if the desired pod count has increased
		if result.DesiredPodCount > 5 {
			log.Println("Scaling up, breaking loop.")
			break
		}
	}
}
