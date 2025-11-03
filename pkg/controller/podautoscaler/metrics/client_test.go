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
