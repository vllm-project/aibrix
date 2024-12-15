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
	"flag"
	"fmt"
	"testing"
	"time"

	"k8s.io/klog/v2"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"
	scalingcontext "github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"

	v1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
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
	metricsFetcher := metrics.NewRestMetricsFetcher()
	kpaMetricsClient := metrics.NewKPAMetricsClient(metricsFetcher, spec.StableWindow, spec.PanicWindow)
	now := time.Now()
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

	pa := v1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test_ns",
		},
		Spec: v1alpha1.PodAutoscalerSpec{
			MetricsSources: []v1alpha1.MetricSource{
				{
					MetricSourceType: v1alpha1.POD,
					ProtocolType:     v1alpha1.HTTP,
					TargetMetric:     spec.ScalingMetric,
					TargetValue:      fmt.Sprintf("%f", spec.TargetValue),
				},
			},
			ScaleTargetRef: corev1.ObjectReference{
				Name: "llama-70b",
			},
		},
	}

	metricKey, _, err := metrics.NewNamespaceNameMetric(&pa)

	if err != nil {
		t.Errorf("NewNamespaceNameMetric() failed: %v", err)
	}

	result := kpaScaler.Scale(readyPodCount, metricKey, now)
	// recent rapid rising metric value make scaler adapt turn on panic mode
	if result.DesiredPodCount != 10 {
		t.Errorf("result.DesiredPodCount = 10, got %d", result.DesiredPodCount)
	}
}

func TestKpaUpdateContext(t *testing.T) {
	pa := &v1alpha1.PodAutoscaler{
		Spec: v1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas: nil, // expecting nil as default since it's a pointer and no value is assigned
			MaxReplicas: 5,
			MetricsSources: []v1alpha1.MetricSource{
				{
					Endpoint:     "service1.example.com",
					Path:         "/api/metrics/cpu",
					TargetMetric: "test.metrics",
					TargetValue:  "1",
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
				"kpa.autoscaling.aibrix.ai/panic-window":          "50s",
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
	if kpaSpec.PanicWindow != 50*time.Second {
		t.Errorf("expected PanicWindow = 50s, got %v", kpaSpec.PanicWindow)
	}
	if kpaSpec.ScaleDownDelay != 30*time.Second {
		t.Errorf("expected ScaleDownDelay = 10s, got %v", kpaSpec.ScaleDownDelay)
	}
}

// checkInPanic check AutoScaler's panic status is as expected
func checkInPanic(t *testing.T, expectIn bool, autoScaler *KpaAutoscaler) {
	if expectIn && !autoScaler.InPanicMode() {
		t.Fatalf("should be in panic mode")
	}
	if !expectIn && autoScaler.InPanicMode() {
		t.Fatalf("shouldn't be in panic mode")
	}
}

// TestKpaScale2 simulate from creating KPA scaler same as what `PodAutoscalerReconciler.updateMetricsForScale` do.
func TestKpaScale2(t *testing.T) {
	klog.InitFlags(nil)
	_ = flag.Set("v", "4")
	_ = flag.Set("v", "2")

	PANIC_WINDOW := 10 * time.Second
	STABLE_WINDOW := 30 * time.Second
	SCALE_DOWN_DELAY := 20 * time.Second
	pa := &v1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test_ns",
			Name:      "test_llm_for_pa",
			Labels: map[string]string{
				"kpa.autoscaling.aibrix.ai/stable-window":    STABLE_WINDOW.String(),
				"kpa.autoscaling.aibrix.ai/panic-window":     PANIC_WINDOW.String(),
				"kpa.autoscaling.aibrix.ai/scale-down-delay": SCALE_DOWN_DELAY.String(),
			},
		},
		Spec: v1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas: nil, // expecting nil as default since it's a pointer and no totalValue is assigned
			MaxReplicas: 5,
			MetricsSources: []v1alpha1.MetricSource{
				{
					MetricSourceType: v1alpha1.POD,
					ProtocolType:     v1alpha1.HTTP,
					Path:             "metrics",
					Port:             "8000",
					TargetMetric:     "ttot",
					TargetValue:      "50",
				},
			},
			ScalingStrategy: "KPA",
		},
	}

	readyPodCount := 5
	now := time.Unix(int64(10000), 0)
	autoScaler, err := NewKpaAutoscaler(readyPodCount, pa, now)
	if err != nil {
		t.Errorf("NewKpaAutoscaler() failed: %v", err)
	}

	kpaMetricsClient, ok := autoScaler.metricClient.(*metrics.KPAMetricsClient)
	if !ok {
		t.Errorf("autoscaler.metricClient is not of type *metrics.KPAMetricsClient")
		return
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	metricKey, _, err := metrics.NewNamespaceNameMetric(pa)
	if err != nil {
		t.Errorf("NewNamespaceNameMetric() failed: %v", err)
	}

	type TestData struct {
		ts              time.Time
		totalValue      float64
		desiredPodCount int32
		checkScalerAttr func()
	}

	//// add a time delta to trigger
	//time.Sleep(1 * time.Second)

	// The KPA scaling test scenario is as follows:
	testDataList := []TestData{
		// 1. KPA initially starts in panic mode.
		{now.Add(0 * time.Second), 10.0, 5,
			func() {
				checkInPanic(t, true, autoScaler)
			}},
		// 2. Metrics remain at a low level, and no scaling down occurs due to panic mode.
		{now.Add(10 * time.Second), 10.0, 5,
			func() {
				checkInPanic(t, true, autoScaler)
			}},
		{now.Add(20 * time.Second), 10.0, 5,
			func() {
				checkInPanic(t, true, autoScaler)
			}},
		// it's the right close boundary of panic window, scaler will exit panic mode at next update
		{now.Add(STABLE_WINDOW), 10.0, 5,
			func() {
				checkInPanic(t, true, autoScaler)
				if m, _ := autoScaler.delayWindow.Max(); m != 5 {
					t.Fatalf("max delayWindow should be 5")
				}
			}},
		// 3. After a sustained period of low metrics, KPA exits panic mode, but scaling down is still delayed because of the DelayWindow.
		{now.Add(40 * time.Second), 10.0, 5,
			func() {
				checkInPanic(t, false, autoScaler)
				if m, _ := autoScaler.delayWindow.Max(); m != 5 {
					t.Fatalf("max delayWindow should be 5")
				}
				expectDelayWindow := "TimeWindow(granularity=1s, window=window(size=2, values=[{10030, 5.00}, {10040, 2.00}]))"
				if s := autoScaler.delayWindow.String(); s != expectDelayWindow {
					t.Fatalf("unexpected delayWindow: expected %s, got %s", expectDelayWindow, s)
				}
			}},
		// 4. The max(DelayWindow) has reduce to low level, do scale down
		{now.Add(STABLE_WINDOW + SCALE_DOWN_DELAY), 10.0, 2,
			func() {
				checkInPanic(t, false, autoScaler)
				if m, _ := autoScaler.delayWindow.Max(); m != 2 {
					t.Fatalf("max delayWindow should be 2")
				}
			}},
		// 5. Metrics start to increase, and the PanicWindow suggests scaling up, but doesn't exceed the panic threshold.
		// the stableWindow suggest remain replica to 2. KPA does not enter panic mode.
		{now.Add(60 * time.Second), 150.0, 2,
			func() {
				checkInPanic(t, false, autoScaler)
			}},
		// 6. Metrics continue increasing. stableWindow suggests scaling up to 3, but still doesn't exceed the panic threshold,
		// thus, not in panicWindow, but scale up
		{now.Add(70 * time.Second), 150.0, 3,
			func() {
				checkInPanic(t, false, autoScaler)
			}},
		// 7. Metrics continue to rise, and the scaling rate > threshold, start panic
		{now.Add(80 * time.Second), 300.0, 6,
			func() {
				checkInPanic(t, true, autoScaler)
				if autoScaler.panicTime != now.Add(80*time.Second) {
					t.Fatalf("Expected panicTime to be %v, got %v", now.Add(80*time.Second), autoScaler.panicTime)
				}
			}},
		// 8. Metrics continue increasing, update panic time
		{now.Add(90 * time.Second), 600.0, 12,
			func() {
				checkInPanic(t, true, autoScaler)
				if autoScaler.panicTime != now.Add(90*time.Second) {
					t.Fatalf("Expected panicTime to be %v, got %v", now.Add(90*time.Second), autoScaler.panicTime)
				}
			}},
		// 9. Metrics stabilize and stop increasing, KPA remains in panic mode but no longer updates the panicTime.
		{now.Add(100 * time.Second), 300.0, 12,
			func() {
				checkInPanic(t, true, autoScaler)
				if autoScaler.panicTime != now.Add(90*time.Second) {
					t.Fatalf("Expected panicTime to be %v, got %v", now.Add(90*time.Second), autoScaler.panicTime)
				}
			}},
	}
	for _, testData := range testDataList {
		t.Logf("--- test ts=%v: totalValue=%.2f expect=%d", testData.ts.Unix(), testData.totalValue, testData.desiredPodCount)
		err := kpaMetricsClient.UpdateMetricIntoWindow(testData.ts, testData.totalValue)
		if err != nil {
			t.Fatalf("failed to update metric: %v", err)
		}

		result := autoScaler.Scale(readyPodCount, metricKey, testData.ts)
		testData.checkScalerAttr()

		if result.DesiredPodCount != testData.desiredPodCount {
			t.Fatalf("expected DesiredPodCount = %d, got %d", testData.desiredPodCount, result.DesiredPodCount)
		}
		// update the up-to-date pod count
		readyPodCount = int(result.DesiredPodCount)
		t.Logf("scale pod count to %d", readyPodCount)
	}

}
