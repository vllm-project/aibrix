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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	customGauges     = make(map[string]*prometheus.GaugeVec)
	customGaugesMu   sync.RWMutex
	customCounters   = make(map[string]*prometheus.CounterVec)
	customCountersMu sync.RWMutex

	// Function variables that can be overridden for testing
	SetGaugeMetricFnForTest         = defaultSetGaugeMetric
	IncrementCounterMetricFnForTest = defaultIncrementCounterMetric
)

func SetGaugeMetric(name string, help string, value float64, labelNames []string, labelValues ...string) {
	SetGaugeMetricFnForTest(name, help, value, labelNames, labelValues...)
}

func defaultSetGaugeMetric(name string, help string, value float64, labelNames []string, labelValues ...string) {
	customGaugesMu.RLock()
	gauge, ok := customGauges[name]
	customGaugesMu.RUnlock()

	if !ok {
		customGaugesMu.Lock()
		gauge, ok = customGauges[name]
		if !ok {
			gauge = promauto.NewGaugeVec(
				prometheus.GaugeOpts{Name: name, Help: help},
				labelNames,
			)
			customGauges[name] = gauge
		}
		customGaugesMu.Unlock()
	}

	gauge.WithLabelValues(labelValues...).Set(value)
}

func IncrementCounterMetric(name string, help string, value float64, labelNames []string, labelValues ...string) {
	IncrementCounterMetricFnForTest(name, help, value, labelNames, labelValues...)
}

func defaultIncrementCounterMetric(name string, help string, value float64, labelNames []string, labelValues ...string) {
	customCountersMu.RLock()
	counter, ok := customCounters[name]
	customCountersMu.RUnlock()

	if !ok {
		customCountersMu.Lock()
		counter, ok = customCounters[name]
		if !ok {
			counter = promauto.NewCounterVec(
				prometheus.CounterOpts{Name: name, Help: help},
				labelNames,
			)
			customCounters[name] = counter
		}
		customCountersMu.Unlock()
	}

	counter.WithLabelValues(labelValues...).Add(value)
}

func GetMetricHelp(metricName string) string {
	metric, ok := Metrics[metricName]
	if !ok {
		return ""
	}
	return metric.Description
}

func GetGaugeValueForTest(name string, labelValues ...string) float64 {
	customGaugesMu.RLock()
	defer customGaugesMu.RUnlock()
	// Tests should use their own registry
	return 0
}

func SetupMetricsForTest(metricName string, labelNames []string) (*prometheus.GaugeVec, func()) {
	testRegistry := prometheus.NewRegistry()
	testGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: metricName},
		labelNames,
	)
	testRegistry.MustRegister(testGauge)

	originalFn := SetGaugeMetricFnForTest
	SetGaugeMetricFnForTest = func(name string, help string, value float64, labels []string, labelValues ...string) {
		if name == metricName {
			testGauge.WithLabelValues(labelValues...).Set(value)
		}
	}

	return testGauge, func() { SetGaugeMetricFnForTest = originalFn }
}

func SetupCounterMetricsForTest(metricName string, labelNames []string) (*prometheus.CounterVec, func()) {
	testRegistry := prometheus.NewRegistry()
	testCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: metricName},
		labelNames,
	)
	testRegistry.MustRegister(testCounter)

	originalFn := IncrementCounterMetricFnForTest
	IncrementCounterMetricFnForTest = func(name string, help string, value float64, labels []string, labelValues ...string) {
		if name == metricName {
			testCounter.WithLabelValues(labelValues...).Add(value)
		}
	}

	return testCounter, func() { IncrementCounterMetricFnForTest = originalFn }
}
