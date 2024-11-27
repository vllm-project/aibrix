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
	"testing"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"
	scalingcontext "github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
)

// TestKpaScale tests the KPA behavior under high traffic rising condition.
// KPA stable mode recommend number of replicas 3.
// However, in the event of a traffic spike within the last 10 seconds,
// and surpassing the PanicThreshold, the system should enter panic mode and scale up to 10 replicas.
func TestKpaScale(t *testing.T) {
	readyPodCount := 5
	spec := KpaScalingContext{
		BaseScalingContext: scalingcontext.BaseScalingContext{
			MaxScaleUpRate:   2,
			MaxScaleDownRate: 2,
			ScalingMetric:    "ttot",
			TargetValue:      10,
			TotalValue:       500,
		},
		TargetBurstCapacity: 2.0,
		ActivationScale:     2,
		PanicThreshold:      2.0,
		StableWindow:        60 * time.Second,
		PanicWindow:         10 * time.Second,
		ScaleDownDelay:      30 * time.Minute,
	}
	metricsFetcher := &metrics.RestMetricsFetcher{}
	kpaMetricsClient := metrics.NewKPAMetricsClient(metricsFetcher, spec.StableWindow, spec.PanicWindow)
	now := time.Now()
	metricKey := metrics.NewNamespaceNameMetric("test_ns", "llama-70b", spec.ScalingMetric)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-60*time.Second), 10.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-50*time.Second), 11.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-40*time.Second), 12.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-30*time.Second), 13.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-20*time.Second), 14.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(now.Add(-10*time.Second), 100.0)

	kpaScaler := KpaAutoscaler{
		metricClient:   kpaMetricsClient,
		panicTime:      now,
		maxPanicPods:   int32(readyPodCount),
		delayWindow:    aggregation.NewTimeWindow(spec.ScaleDownDelay, time.Second),
		algorithm:      &algorithm.KpaScalingAlgorithm{},
		scalingContext: &spec,
	}

	kpaScaler.metricClient = kpaMetricsClient
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	result := kpaScaler.Scale(readyPodCount, metricKey, now)
	// recent rapid rising metric value make scaler adapt turn on panic mode
	if result.DesiredPodCount != 10 {
		t.Errorf("result.DesiredPodCount = 10, got %d", result.DesiredPodCount)
	}
}

func TestKpaUpdateContext(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas:  nil, // expecting nil as default since it's a pointer and no value is assigned
			MaxReplicas:  5,
			TargetValue:  "1",
			TargetMetric: "test.metrics",
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					Endpoint: "service1.example.com",
					Path:     "/api/metrics/cpu",
				},
			},
			ScalingStrategy: "KPA",
		},
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"autoscaling.aibrix.ai/max-scale-up-rate":         "32.1",
				"autoscaling.aibrix.ai/max-scale-down-rate":       "12.3",
				"kpa.autoscaling.aibrix.ai/target-burst-capacity": "45.6",
				"kpa.autoscaling.aibrix.ai/activation-scale":      "3",
				"kpa.autoscaling.aibrix.ai/panic-threshold":       "2.5",
				"kpa.autoscaling.aibrix.ai/stable-window":         "60s",
				"kpa.autoscaling.aibrix.ai/scale-down-delay":      "30s",
			},
		},
	}
	kpaSpec := NewKpaScalingContext()
	err := kpaSpec.UpdateByPaTypes(pa)
	if err != nil {
		t.Errorf("Failed to update KpaScalingContext: %v", err)
	}
	if kpaSpec.MaxScaleUpRate != 32.1 {
		t.Errorf("expected MaxScaleDownRate = 32.1, got %f", kpaSpec.MaxScaleDownRate)
	}
	if kpaSpec.MaxScaleDownRate != 12.3 {
		t.Errorf("expected MaxScaleDownRate = 12.3, got %f", kpaSpec.MaxScaleDownRate)
	}
	if kpaSpec.TargetBurstCapacity != 45.6 {
		t.Errorf("expected TargetBurstCapacity = 45.6, got %f", kpaSpec.TargetBurstCapacity)
	}
	if kpaSpec.ActivationScale != 3 {
		t.Errorf("expected ActivationScale = 3, got %d", kpaSpec.ActivationScale)
	}
	if kpaSpec.PanicThreshold != 2.5 {
		t.Errorf("expected PanicThreshold = 2.5, got %f", kpaSpec.PanicThreshold)
	}
	if kpaSpec.StableWindow != 60*time.Second {
		t.Errorf("expected StableWindow = 60s, got %v", kpaSpec.StableWindow)
	}
	if kpaSpec.ScaleDownDelay != 30*time.Second {
		t.Errorf("expected ScaleDownDelay = 10s, got %v", kpaSpec.ScaleDownDelay)
	}
}
