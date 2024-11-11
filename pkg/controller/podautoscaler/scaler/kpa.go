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
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	scalingcontext "github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"
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

// KpaScalingContext defines parameters for scaling decisions.
type KpaScalingContext struct {
	scalingcontext.BaseScalingContext

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
}

var _ scalingcontext.ScalingContext = (*KpaScalingContext)(nil)

// NewKpaScalingContext references KPA and sets up a default configuration.
func NewKpaScalingContext() *KpaScalingContext {
	return &KpaScalingContext{
		BaseScalingContext:  *scalingcontext.NewBaseScalingContext(),
		TargetBurstCapacity: 2.0,              // Target burst capacity to handle sudden spikes
		ActivationScale:     1,                // Initial scaling factor upon activation
		PanicThreshold:      2.0,              // Panic threshold set at 200% to trigger rapid scaling
		StableWindow:        60 * time.Second, // Time window to stabilize before altering scale
		ScaleDownDelay:      30 * time.Minute, // Delay before scaling down to avoid flapping
	}
}

type KpaAutoscaler struct {
	specMux      sync.RWMutex
	metricClient metrics.MetricClient
	k8sClient    client.Client

	panicTime      time.Time
	maxPanicPods   int32
	delayWindow    *aggregation.TimeWindow
	Status         *ScaleResult
	scalingContext *KpaScalingContext
	algorithm      algorithm.ScalingAlgorithm
}

var _ Scaler = (*KpaAutoscaler)(nil)

// NewKpaAutoscaler Initialize KpaAutoscaler: Referenced from `knative/pkg/autoscaler/scaling/autoscaler.go newAutoscaler`
func NewKpaAutoscaler(readyPodsCount int, spec *KpaScalingContext) (*KpaAutoscaler, error) {
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
	metricsFetcher := &metrics.RestMetricsFetcher{}
	metricsClient := metrics.NewKPAMetricsClient(metricsFetcher)
	scalingAlgorithm := algorithm.KpaScalingAlgorithm{}

	return &KpaAutoscaler{
		metricClient:   metricsClient,
		panicTime:      panicTime,
		maxPanicPods:   int32(readyPodsCount),
		delayWindow:    delayWindow,
		algorithm:      &scalingAlgorithm,
		scalingContext: spec,
	}, nil
}

// Scale implements Scaler interface in KpaAutoscaler.
// Refer to knative-serving: pkg/autoscaler/scaling/autoscaler.go, Scale function.
func (k *KpaAutoscaler) Scale(originalReadyPodsCount int, metricKey metrics.NamespaceNameMetric, now time.Time) ScaleResult {
	/**
	`observedStableValue` and `observedPanicValue` are calculated using different window sizes in the `MetricClient`.
	 For reference, see the KNative implementation at `pkg/autoscaler/metrics/collector.go：185`.
	*/

	// Attempt to convert spec to *KpaScalingContext
	spec, ok := k.GetScalingContext().(*KpaScalingContext)
	if !ok {
		// Handle the error if the conversion fails
		klog.Error("Failed to convert ScalingContext to KpaScalingContext")
	}

	kpaMetricsClient := k.metricClient.(*metrics.KPAMetricsClient)
	observedStableValue, observedPanicValue, err := kpaMetricsClient.StableAndPanicMetrics(metricKey, now)
	if err != nil {
		klog.Errorf("Failed to get stable and panic metrics for %s: %v", metricKey, err)
		return ScaleResult{}
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
	if k.scalingContext.ActivationScale > 1 {
		if k.scalingContext.ActivationScale > desiredStablePodCount {
			desiredStablePodCount = k.scalingContext.ActivationScale
		}
		if k.scalingContext.ActivationScale > desiredPanicPodCount {
			desiredPanicPodCount = k.scalingContext.ActivationScale
		}
	}

	isOverPanicThreshold := dppc/readyPodsCount >= spec.PanicThreshold

	klog.V(4).InfoS("--- KPA Details", "readyPodsCount", readyPodsCount,
		"MaxScaleUpRate", spec.MaxScaleUpRate, "MaxScaleDownRate", spec.MaxScaleDownRate,
		"TargetValue", spec.TargetValue, "PanicThreshold", spec.PanicThreshold,
		"StableWindow", spec.StableWindow, "ScaleDownDelay", spec.ScaleDownDelay,
		"dppc", dppc, "dspc", dspc, "desiredStablePodCount", desiredStablePodCount,
		"PanicThreshold", spec.PanicThreshold, "isOverPanicThreshold", isOverPanicThreshold,
	)

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
		klog.InfoS("Operating in panic mode.", "desiredPodCount", desiredPodCount, "desiredPanicPodCount", desiredPanicPodCount)
		if desiredPodCount < desiredPanicPodCount {
			desiredPodCount = desiredPanicPodCount
		}
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
	klog.V(4).InfoS("DelayWindow details", "delayWindow", k.delayWindow.String())
	if k.delayWindow != nil {
		k.delayWindow.Record(now, float64(desiredPodCount))
		delayedPodCount, err := k.delayWindow.Max()
		if err != nil {
			klog.ErrorS(err, "Failed to get delayed pod count")
			return ScaleResult{}
		}
		if int32(delayedPodCount) != desiredPodCount {
			klog.InfoS(
				fmt.Sprintf("Delaying scale to %d, staying at %d", int(desiredPodCount), int(delayedPodCount)),
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

func (k *KpaAutoscaler) UpdateScaleTargetMetrics(ctx context.Context, metricKey metrics.NamespaceNameMetric, pods []v1.Pod, now time.Time) error {
	// TODO: let's update this fix port later.
	metricPort := 8000
	metricValues, err := k.metricClient.GetMetricsFromPods(ctx, pods, metricKey.MetricName, metricPort)
	if err != nil {
		return err
	}

	err = k.metricClient.UpdatePodListMetric(metricValues, metricKey, now)
	if err != nil {
		return err
	}

	return nil
}

func (k *KpaAutoscaler) UpdateScalingContext(pa autoscalingv1alpha1.PodAutoscaler) error {
	k.specMux.Lock()
	defer k.specMux.Unlock()

	targetValue, err := strconv.ParseFloat(pa.Spec.TargetValue, 64)
	if err != nil {
		klog.ErrorS(err, "Failed to parse target value", "targetValue", pa.Spec.TargetValue)
		return err
	}
	k.scalingContext.TargetValue = targetValue
	k.scalingContext.ScalingMetric = pa.Spec.TargetMetric

	return nil
}

func (k *KpaAutoscaler) GetScalingContext() scalingcontext.ScalingContext {
	k.specMux.Lock()
	defer k.specMux.Unlock()

	return k.scalingContext
}
