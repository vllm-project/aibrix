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
	"fmt"
	"testing"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/algorithm"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
)

// TestHcpaScale tests the APA behavior. For now, APA implements HCPA algorithm.
func TestAPAScale(t *testing.T) {
	// TODO (jiaxin.shan): make the logics to enable the test later.
	t.Skip("Skipping this test")

	readyPodCount := 5
	spec := NewApaScalingContext()
	metricsFetcher := metrics.NewRestMetricsFetcher()
	apaMetricsClient := metrics.NewAPAMetricsClient(metricsFetcher, spec.Window)
	now := time.Now()

	pa := autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test_ns",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
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
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-60*time.Second), 10.0)
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-50*time.Second), 11.0)
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-40*time.Second), 12.0)
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-30*time.Second), 13.0)
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-20*time.Second), 14.0)
	_ = apaMetricsClient.UpdateMetricIntoWindow(now.Add(-10*time.Second), 100.0)

	apaScaler := ApaAutoscaler{
		metricClient:   apaMetricsClient,
		algorithm:      &algorithm.ApaScalingAlgorithm{},
		scalingContext: spec,
	}
	apaScaler.metricClient = apaMetricsClient
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// test 1:
	result := apaScaler.Scale(readyPodCount, metricKey, now)
	// recent rapid rising metric value make scaler adapt turn on panic mode
	if result.DesiredPodCount != 10 {
		t.Errorf("result.DesiredPodCount = 10, got %d", result.DesiredPodCount)
	}

	// test 2:
	// 1.1 means APA won't scale up unless current usage > TargetValue * (1+1.1), i.e. 210%
	// In this test case with UpFluctuationTolerance = 1.1, APA will not scale up.
	apaScaler.scalingContext.UpFluctuationTolerance = 1.1
	result = apaScaler.Scale(readyPodCount, metricKey, now)
	// recent rapid rising metric value make scaler adapt turn on panic mode
	if result.DesiredPodCount != int32(readyPodCount) {
		t.Errorf("result should remain previous replica = %d, but got %d", readyPodCount, result.DesiredPodCount)
	}
}

func TestApaUpdateContext(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas: nil, // expecting nil as default since it's a pointer and no value is assigned
			MaxReplicas: 5,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					Endpoint:     "service1.example.com",
					Path:         "/api/metrics/cpu",
					TargetValue:  "1",
					TargetMetric: "test.metrics",
				},
			},
			ScalingStrategy: "APA",
		},
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"autoscaling.aibrix.ai/max-scale-up-rate":              "32.1",
				"autoscaling.aibrix.ai/max-scale-down-rate":            "12.3",
				"apa.autoscaling.aibrix.ai/up-fluctuation-tolerance":   "1.2",
				"apa.autoscaling.aibrix.ai/down-fluctuation-tolerance": "0.9",
			},
		},
	}
	apaSpec := NewApaScalingContext()
	err := apaSpec.UpdateByPaTypes(pa)
	if err != nil {
		t.Errorf("Failed to update ApaScalingContext: %v", err)
	}
	if apaSpec.MaxScaleUpRate != 32.1 {
		t.Errorf("expected MaxScaleDownRate = 32.1, got %f", apaSpec.MaxScaleDownRate)
	}
	if apaSpec.MaxScaleDownRate != 12.3 {
		t.Errorf("expected MaxScaleDownRate = 12.3, got %f", apaSpec.MaxScaleDownRate)
	}

	if apaSpec.UpFluctuationTolerance != 1.2 {
		t.Errorf("expected UpFluctuationTolerance = 1.2, got %f", apaSpec.UpFluctuationTolerance)
	}
	if apaSpec.DownFluctuationTolerance != 0.9 {
		t.Errorf("expected DownFluctuationTolerance = 0.9, got %f", apaSpec.DownFluctuationTolerance)
	}

}

// TestApaScale2 simulate from creating APA scaler same as what `PodAutoscalerReconciler.updateMetricsForScale` do.
func TestApaScale2(t *testing.T) {

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test_ns",
			Name:      "test_llm_for_pa",
			Labels: map[string]string{
				"autoscaling.aibrix.ai/max-scale-up-rate":          "2",
				"autoscaling.aibrix.ai/max-scale-down-rate":        "1.25",
				"autoscaling.aibrix.ai/up-fluctuation-tolerance":   "0.1",
				"autoscaling.aibrix.ai/down-fluctuation-tolerance": "0.2",
				"apa.autoscaling.aibrix.ai/window":                 "30s",
			},
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas: nil, // expecting nil as default since it's a pointer and no totalValue is assigned
			MaxReplicas: 5,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
					Path:             "metrics",
					Port:             "8000",
					TargetMetric:     "ttot",
					TargetValue:      "50",
				},
			},
			ScalingStrategy: "APA",
		},
	}

	readyPodCount := 5
	now := time.Unix(int64(10000), 0)
	autoScaler, err := NewApaAutoscaler(readyPodCount, pa)
	if err != nil {
		t.Errorf("NewApaAutoscaler() failed: %v", err)
	}

	apaMetricsClient, ok := autoScaler.metricClient.(*metrics.APAMetricsClient)
	if !ok {
		t.Errorf("autoscaler.metricClient is not of type *metrics.APAMetricsClient")
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

	// The APA scaling test scenario is as follows:
	testDataList := []TestData{
		{now.Add(0 * time.Second), 50.0 * 5, 5,
			func() {},
		},
		// the window average is 55 / 50 = 1+0.1, within fluctuation tolerance, no scaling
		{now.Add(10 * time.Second), 60.0 * 5, 5,
			func() {},
		},
		// the window average is 40 / 50 = 1-0.2, within fluctuation tolerance, no scaling
		{now.Add(20 * time.Second), 10.0 * 5, 5,
			func() {},
		},
		// lower than 1-0.2, try to scale down to 3, but reset to 4 = 5 / max_down_scale_rate = 5 / 1.25 = 4
		{now.Add(30 * time.Second), 20 * 5, 4,
			func() {},
		},
	}
	for _, testData := range testDataList {
		t.Logf("--- test ts=%v: totalValue=%.2f expect=%d", testData.ts.Unix(), testData.totalValue, testData.desiredPodCount)
		err := apaMetricsClient.UpdateMetricIntoWindow(testData.ts, testData.totalValue)
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
