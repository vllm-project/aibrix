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

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
)

// TestKpaScale tests the KPA behavior under high traffic rising condition.
// KPA stable mode recommend number of replicas 3.
// However, in the event of a traffic spike within the last 10 seconds,
// and surpassing the PanicThreshold, the system should enter panic mode and scale up to 10 replicas.
func TestKpaScale(t *testing.T) {
	readyPodCount := 5
	kpaMetricsClient := metrics.NewKPAMetricsClient()
	now := time.Now()
	metricKey := metrics.NewNamespaceNameMetric("test_ns", "llama-70b", "ttot")
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-60*time.Second), 10.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-50*time.Second), 11.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-40*time.Second), 12.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-30*time.Second), 13.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-20*time.Second), 14.0)
	_ = kpaMetricsClient.UpdateMetricIntoWindow(metricKey, now.Add(-10*time.Second), 100.0)

	kpaScaler, err := NewKpaAutoscaler(readyPodCount,
		&DeciderKpaSpec{
			MaxScaleUpRate:   2,
			MaxScaleDownRate: 2,
			ScalingMetric:    metricKey.MetricName,
			TargetValue:      10,
			TotalValue:       500,
			PanicThreshold:   2.0,
			StableWindow:     60 * time.Second,
			ScaleDownDelay:   10 * time.Second,
			ActivationScale:  2,
		},
	)
	kpaScaler.metricsClient = kpaMetricsClient
	if err != nil {
		t.Errorf("Failed to create KpaAutoscaler: %v", err)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	result := kpaScaler.Scale(readyPodCount, metricKey, now)
	// recent rapid rising metric value make scaler adapt turn on panic mode
	if result.DesiredPodCount != 10 {
		t.Errorf("result.DesiredPodCount = 10, got %d", result.DesiredPodCount)
	}
}
