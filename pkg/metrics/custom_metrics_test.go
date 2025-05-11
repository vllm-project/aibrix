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
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

const (
	testMetricName  = "test_metric"
	testMetricHelp  = "Test metric for unit tests"
	testLabelName   = "test_label"
	testLabelValue  = "test_value"
	testLabelName2  = "test_label2"
	testLabelValue2 = "test_value2"
)

func TestSetGaugeMetric(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: testMetricName, Help: testMetricHelp},
		[]string{testLabelName},
	)
	registry.MustRegister(gauge)

	originalFn := SetGaugeMetricFnForTest
	defer func() { SetGaugeMetricFnForTest = originalFn }()

	SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name == testMetricName {
			gauge.WithLabelValues(labelValues...).Set(value)
		}
	}

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue, []string{testLabelName}, testLabelValue)

	value := testutil.ToFloat64(gauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should match what was set")
}

func TestSetupMetricsForTest(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	testGauge, cleanup := SetupMetricsForTest(testMetricName, []string{testLabelName})
	defer cleanup()

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue, []string{testLabelName}, testLabelValue)

	value := testutil.ToFloat64(testGauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should match what was set")

	cleanup()
	SetGaugeMetric(testMetricName, testMetricHelp, 99.0, []string{testLabelName}, testLabelValue)

	value = testutil.ToFloat64(testGauge.WithLabelValues(testLabelValue))
	assert.Equal(t, testValue, value, "Metric value should not change after cleanup")
}

func TestMetricRegistrationOnlyOnce(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	for i := 0; i < 5; i++ {
		SetGaugeMetric(testMetricName, testMetricHelp, float64(i), []string{testLabelName}, testLabelValue)
	}

	customGaugesMu.RLock()
	count := len(customGauges)
	customGaugesMu.RUnlock()

	assert.Equal(t, 1, count, "Should only register the metric once")
}

func TestConcurrentMetricUpdates(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	numGoroutines := 10
	numUpdatesPerGoroutine := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numUpdatesPerGoroutine; j++ {
				labelValue := testLabelValue + "_" + string(rune('A'+id))
				SetGaugeMetric(testMetricName, testMetricHelp, float64(j), []string{testLabelName}, labelValue)
			}
		}(i)
	}

	wg.Wait()

	customGaugesMu.RLock()
	count := len(customGauges)
	gauge := customGauges[testMetricName]
	customGaugesMu.RUnlock()

	assert.Equal(t, 1, count, "Should only register the metric once even with concurrent updates")

	ch := make(chan *prometheus.Desc, 1)
	gauge.Describe(ch)
	desc := <-ch
	assert.Contains(t, desc.String(), testMetricName, "Metric description should contain the metric name")
}

func TestMultipleLabels(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customGauges = make(map[string]*prometheus.GaugeVec)

	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: testMetricName, Help: testMetricHelp},
		[]string{testLabelName, testLabelName2},
	)
	registry.MustRegister(gauge)

	originalFn := SetGaugeMetricFnForTest
	defer func() { SetGaugeMetricFnForTest = originalFn }()

	SetGaugeMetricFnForTest = func(name string, help string, value float64, labelNames []string, labelValues ...string) {
		if name == testMetricName {
			gauge.WithLabelValues(labelValues...).Set(value)
		}
	}

	testValue := 42.0
	SetGaugeMetric(testMetricName, testMetricHelp, testValue,
		[]string{testLabelName, testLabelName2},
		testLabelValue, testLabelValue2)

	value := testutil.ToFloat64(gauge.WithLabelValues(testLabelValue, testLabelValue2))
	assert.Equal(t, testValue, value, "Metric value with multiple labels should match what was set")
}

func TestSetupCounterMetricsForTest(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	customCounters = make(map[string]*prometheus.CounterVec)

	testCounter, cleanup := SetupCounterMetricsForTest("test_counter", []string{"pod", "model"})
	defer cleanup()

	IncrementCounterMetric("test_counter", "Test counter metric", 5.0, []string{"pod", "model"}, "pod-1", "model-1")

	metricValue := testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 5.0, metricValue, "Counter metric value should match the incremented value")

	IncrementCounterMetric("test_counter", "Test counter metric", 3.0, []string{"pod", "model"}, "pod-1", "model-1")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 8.0, metricValue, "Counter metric value should accumulate")

	IncrementCounterMetric("test_counter", "Test counter metric", 10.0, []string{"pod", "model"}, "pod-2", "model-1")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-1", "model-1"))
	assert.Equal(t, 8.0, metricValue, "Original counter metric should be unchanged")

	metricValue = testutil.ToFloat64(testCounter.WithLabelValues("pod-2", "model-1"))
	assert.Equal(t, 10.0, metricValue, "Counter metric with different labels should have correct value")
}
