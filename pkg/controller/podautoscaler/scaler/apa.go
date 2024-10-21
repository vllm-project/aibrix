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
	"strconv"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"
	scalingcontext "github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// ApaScalingContext defines parameters for scaling decisions.
type ApaScalingContext struct {
	scalingcontext.BaseScalingContext

	// The two following attributes are specific to APA.
	// UpFluctuationTolerance represents the threshold before scaling up,
	// which means no scaling up will occur unless the currentMetricValue exceeds the TargetValue by more than UpFluctuationTolerance
	UpFluctuationTolerance float64
	// UpFluctuationTolerance represents the threshold before scaling down,
	// which means no scaling down will occur unless the currentMetricValue is less than the TargetValue by more than UpFluctuationTolerance
	DownFluctuationTolerance float64
}

// NewApaScalingContext references KPA and sets up a default configuration.
func NewApaScalingContext() *ApaScalingContext {
	return &ApaScalingContext{
		BaseScalingContext:       *scalingcontext.NewBaseScalingContext(),
		UpFluctuationTolerance:   0.1, // Tolerance for scaling up, set at 10%
		DownFluctuationTolerance: 0.2, // Tolerance for scaling up, set at 10%
	}
}

var _ common.ScalingContext = (*ApaScalingContext)(nil)

type ApaAutoscaler struct {
	specMux      sync.RWMutex
	metricClient metrics.MetricClient
	k8sClient    client.Client

	panicTime      time.Time
	maxPanicPods   int32
	Status         ScaleResult
	scalingContext *ApaScalingContext
	algorithm      algorithm.ScalingAlgorithm
}

var _ Scaler = (*ApaAutoscaler)(nil)

// NewApaAutoscaler Initialize ApaAutoscaler
func NewApaAutoscaler(readyPodsCount int, spec *ApaScalingContext) (*ApaAutoscaler, error) {
	metricsFetcher := &metrics.RestMetricsFetcher{}
	metricsClient := metrics.NewAPAMetricsClient(metricsFetcher)
	scalingAlgorithm := algorithm.ApaScalingAlgorithm{}

	return &ApaAutoscaler{
		metricClient:   metricsClient,
		algorithm:      &scalingAlgorithm,
		scalingContext: spec,
	}, nil
}

func (a *ApaAutoscaler) Scale(originalReadyPodsCount int, metricKey metrics.NamespaceNameMetric, now time.Time) ScaleResult {
	spec, ok := a.GetScalingContext().(*ApaScalingContext)
	if !ok {
		// Handle the error if the conversion fails
		klog.Error("Failed to convert ScalingContext to ApaScalingContext")
	}

	apaMetricsClient := a.metricClient.(*metrics.APAMetricsClient)
	observedValue, err := apaMetricsClient.GetMetricValue(metricKey, now)
	if err != nil {
		klog.Errorf("Failed to get stable and panic metrics for %s: %v", metricKey, err)
		return ScaleResult{}
	}

	currentUsePerPod := observedValue / float64(originalReadyPodsCount)
	spec.SetCurrentUsePerPod(currentUsePerPod)

	desiredPodCount := a.algorithm.ComputeTargetReplicas(float64(originalReadyPodsCount), spec)
	klog.InfoS("Use APA scaling strategy", "currentPodCount", originalReadyPodsCount, "currentUsePerPod", currentUsePerPod, "desiredPodCount", desiredPodCount)
	return ScaleResult{
		DesiredPodCount:     desiredPodCount,
		ExcessBurstCapacity: 0,
		ScaleValid:          true,
	}
}

func (a *ApaAutoscaler) UpdateScaleTargetMetrics(ctx context.Context, metricKey metrics.NamespaceNameMetric, pods []v1.Pod, now time.Time) error {
	// TODO: let's update this fix port later.
	metricPort := 8000
	metricValues, err := a.metricClient.GetMetricsFromPods(ctx, pods, metricKey.MetricName, metricPort)
	if err != nil {
		return err
	}

	err = a.metricClient.UpdatePodListMetric(metricValues, metricKey, now)
	if err != nil {
		return err
	}

	return nil
}

func (a *ApaAutoscaler) UpdateScalingContext(pa autoscalingv1alpha1.PodAutoscaler) error {
	a.specMux.Lock()
	defer a.specMux.Unlock()

	targetValue, err := strconv.ParseFloat(pa.Spec.TargetValue, 64)
	if err != nil {
		klog.ErrorS(err, "Failed to parse target value", "targetValue", pa.Spec.TargetValue)
		return err
	}
	a.scalingContext.TargetValue = targetValue
	a.scalingContext.ScalingMetric = pa.Spec.TargetMetric

	return nil
}

func (a *ApaAutoscaler) GetScalingContext() common.ScalingContext {
	a.specMux.Lock()
	defer a.specMux.Unlock()

	return a.scalingContext
}
