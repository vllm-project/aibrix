/*
Copyright 2025 The Aibrix Team.

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

package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

func TestUpdateMetrics(t *testing.T) {
	client := NewMetricsClient(time.Second)

	metricKey := types.MetricKey{
		Namespace:   "default",
		Name:        "test-llm",
		MetricName:  "gpu_cache_usage_perc",
		PaNamespace: "default",
		PaName:      "test-llm-apa",
	}

	expectedValue := 40.0
	metricValues := []float64{30.0, 50.0}

	fixedTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	err := client.UpdateMetrics(fixedTime, metricKey, metricValues...)

	assert.NoError(t, err)

	assert.Len(t, client.stableWindows, 1)
	tw := client.stableWindows["default/test-llm-apa/gpu_cache_usage_perc"]
	assert.NotNil(t, tw)
	assert.Len(t, tw.Values(), 1)
	assert.Equal(t, expectedValue, tw.Values()[0])

	assert.Len(t, client.panicWindows, 1)
	tw = client.panicWindows["default/test-llm-apa/gpu_cache_usage_perc"]
	assert.NotNil(t, tw)
	assert.Len(t, tw.Values(), 1)
	assert.Equal(t, expectedValue, tw.Values()[0])

	assert.Len(t, client.stableHistory, 1)
	assert.NotNil(t, client.stableHistory["default/test-llm-apa/gpu_cache_usage_perc"])

	assert.Len(t, client.panicHistory, 1)
	assert.NotNil(t, client.panicHistory["default/test-llm-apa/gpu_cache_usage_perc"])
}

func TestGetMetricValue(t *testing.T) {
	client := NewMetricsClient(time.Second)

	metricKey := types.MetricKey{
		Namespace:   "default",
		Name:        "test-llm",
		MetricName:  "gpu_cache_usage_perc",
		PaNamespace: "default",
		PaName:      "test-llm-apa",
	}

	metricValue := 70.0
	fixedTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	err := client.UpdateMetrics(fixedTime, metricKey, metricValue)
	require.NoError(t, err)

	stableValue, panicValue, err := client.GetMetricValue(metricKey, fixedTime)

	assert.NoError(t, err)
	assert.Equal(t, metricValue, stableValue)
	assert.Equal(t, metricValue, panicValue)
}

func TestConfigureMetricWindowsUsesCustomDurations(t *testing.T) {
	client := NewMetricsClient(time.Second)

	metricKey := types.MetricKey{
		Namespace:   "default",
		Name:        "test-llm",
		MetricName:  "gpu_cache_usage_perc",
		PaNamespace: "default",
		PaName:      "test-llm-kpa",
	}
	require.NoError(t, client.ConfigureMetricWindows(metricKey, 90*time.Second, 30*time.Second))

	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, client.UpdateMetrics(start, metricKey, 10))
	require.NoError(t, client.UpdateMetrics(start.Add(60*time.Second), metricKey, 70))

	stableValue, panicValue, err := client.GetMetricValue(metricKey, start.Add(60*time.Second))

	require.NoError(t, err)
	assert.Equal(t, 40.0, stableValue)
	assert.Equal(t, 70.0, panicValue)
}

func TestConfigureMetricWindowsClampsHistoryDuration(t *testing.T) {
	client := NewMetricsClient(time.Second)

	metricKey := types.MetricKey{
		Namespace:   "default",
		Name:        "test-llm",
		MetricName:  "gpu_cache_usage_perc",
		PaNamespace: "default",
		PaName:      "test-llm-kpa",
	}
	largeWindow := time.Duration(1<<63 - 1)

	require.NoError(t, client.ConfigureMetricWindows(metricKey, largeWindow, largeWindow))

	assert.Equal(t, maxDuration, historyDuration(largeWindow))
	assert.Equal(t, 10*time.Second, historyDuration(time.Second))
}

func TestDeleteMetricsRemovesOnlyThatPodAutoscaler(t *testing.T) {
	client := NewMetricsClient(time.Second)
	now := time.Now()

	deleted := []types.MetricKey{
		{PaNamespace: "default", PaName: "llm", MetricName: "cpu"},
		{PaNamespace: "default", PaName: "llm", MetricName: "gpu_cache_usage_perc"},
	}
	kept := []types.MetricKey{
		// Shares the deleted name as a prefix.
		{PaNamespace: "default", PaName: "llm-canary", MetricName: "cpu"},
		// Same name in another namespace.
		{PaNamespace: "prod", PaName: "llm", MetricName: "cpu"},
	}
	for _, key := range append(append([]types.MetricKey{}, deleted...), kept...) {
		require.NoError(t, client.UpdateMetrics(now, key, 10.0))
	}

	client.DeleteMetrics("default", "llm")

	for _, key := range deleted {
		_, _, err := client.GetMetricValue(key, now)
		assert.Error(t, err, "windows for %s should be gone", key)
		assert.NotContains(t, client.stableHistory, key.String())
		assert.NotContains(t, client.panicHistory, key.String())
	}
	for _, key := range kept {
		stable, panicValue, err := client.GetMetricValue(key, now)
		require.NoError(t, err, "windows for %s should be untouched", key)
		assert.Equal(t, 10.0, stable)
		assert.Equal(t, 10.0, panicValue)
	}
	assert.Len(t, client.stableWindows, len(kept))
	assert.Len(t, client.panicWindows, len(kept))
	assert.Len(t, client.stableHistory, len(kept))
	assert.Len(t, client.panicHistory, len(kept))
}

// A PodAutoscaler recreated under the same name must not inherit the samples
// its predecessor recorded.
func TestDeleteMetricsLetsARecreatedPodAutoscalerStartClean(t *testing.T) {
	client := NewMetricsClient(time.Second)
	key := types.MetricKey{PaNamespace: "default", PaName: "llm", MetricName: "cpu"}
	now := time.Now()

	require.NoError(t, client.UpdateMetrics(now, key, 90.0))
	client.DeleteMetrics("default", "llm")
	require.NoError(t, client.UpdateMetrics(now.Add(2*time.Second), key, 10.0))

	stable, panicValue, err := client.GetMetricValue(key, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 10.0, stable)
	assert.Equal(t, 10.0, panicValue)
}
