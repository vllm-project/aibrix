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
	"time"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	"k8s.io/klog/v2"
)

type ApaAutoscaler struct {
	*BaseAutoscaler
	panicTime    time.Time
	maxPanicPods int32
	Status       DeciderStatus
	deciderSpec  *DeciderKpaSpec
	algorithm    algorithm.ScalingAlgorithm
}

var _ Scaler = (*ApaAutoscaler)(nil)

// NewApaAutoscaler Initialize ApaAutoscaler
func NewApaAutoscaler(readyPodsCount int, spec *DeciderKpaSpec) (*ApaAutoscaler, error) {
	metricsClient := metrics.NewAPAMetricsClient()
	autoscaler := &BaseAutoscaler{metricsClient: metricsClient}
	scalingAlgorithm := algorithm.ApaScalingAlgorithm{}

	return &ApaAutoscaler{
		BaseAutoscaler: autoscaler,
		algorithm:      &scalingAlgorithm,
	}, nil
}

func getMetrics() (float64, float64, error) {
	// Simulate fetching metrics
	return 1.0, 1.0, nil
}

func (a *ApaAutoscaler) Scale(originalReadyPodsCount int, metricKey metrics.NamespaceNameMetric, now time.Time, strategy autoscalingv1alpha1.ScalingStrategyType) ScaleResult {
	spec := a.GetSpec()
	// TODO: fix me
	//observedStableValue, observedPanicValue, err := apaMetricsClient.StableAndPanicMetrics(metricKey, now)
	_, observedPanicValue, err := getMetrics()
	if err != nil {
		klog.Errorf("Failed to get stable and panic metrics for %s: %v", metricKey, err)
		return ScaleResult{}
	}

	currentUsePerPod := observedPanicValue / float64(originalReadyPodsCount)
	spec.SetCurrentUsePerPod(currentUsePerPod)

	desiredPodCount := a.algorithm.ComputeTargetReplicas(float64(originalReadyPodsCount), spec)
	klog.InfoS("Use APA scaling strategy", "currentPodCount", originalReadyPodsCount, "currentUsePerPod", currentUsePerPod, "desiredPodCount", desiredPodCount)
	return ScaleResult{
		DesiredPodCount:     desiredPodCount,
		ExcessBurstCapacity: 0,
		ScaleValid:          true,
	}
}

func (a *ApaAutoscaler) GetSpec() *DeciderKpaSpec {
	a.specMux.Lock()
	defer a.specMux.Unlock()

	return a.deciderSpec
}
