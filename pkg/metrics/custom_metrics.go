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
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	customGauges     = make(map[string]*prometheus.GaugeVec)
	customGaugesMu   sync.RWMutex
	customCounters   = make(map[string]*prometheus.CounterVec)
	customCountersMu sync.RWMutex
	customHistograms   = make(map[string]*histogramCollector)
	customHistogramsMu sync.RWMutex

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

func SetHistogramMetric(name string, help string, value *HistogramMetricValue, labelNames []string, labelValues ...string) {
	if value == nil {
		return
	}

	customHistogramsMu.RLock()
	collector, ok := customHistograms[name]
	customHistogramsMu.RUnlock()

	if !ok {
		customHistogramsMu.Lock()
		collector, ok = customHistograms[name]
		if !ok {
			collector = newHistogramCollector(name, help, labelNames)
			prometheus.MustRegister(collector)
			customHistograms[name] = collector
		}
		customHistogramsMu.Unlock()
	}

	collector.Set(value, labelValues...)
}

type histogramSnapshot struct {
	labelValues []string
	count       uint64
	sum         float64
	buckets     map[float64]uint64
}

type histogramCollector struct {
	desc       *prometheus.Desc
	labelNames []string
	mu         sync.RWMutex
	data       map[string]*histogramSnapshot
}

func newHistogramCollector(name string, help string, labelNames []string) *histogramCollector {
	return &histogramCollector{
		desc:       prometheus.NewDesc(name, help, labelNames, nil),
		labelNames: labelNames,
		data:       make(map[string]*histogramSnapshot),
	}
}

func (c *histogramCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *histogramCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshots := make([]*histogramSnapshot, 0, len(c.data))
	for _, snapshot := range c.data {
		snapshots = append(snapshots, snapshot)
	}
	c.mu.RUnlock()

	for _, snapshot := range snapshots {
		ch <- prometheus.MustNewConstHistogram(
			c.desc,
			snapshot.count,
			snapshot.sum,
			snapshot.buckets,
			snapshot.labelValues...,
		)
	}
}

func (c *histogramCollector) Set(value *HistogramMetricValue, labelValues ...string) {
	if value == nil || len(labelValues) != len(c.labelNames) {
		return
	}

	buckets := make(map[float64]uint64, len(value.Buckets))
	for bound, cumulativeCount := range value.Buckets {
		bound = strings.TrimSpace(bound)
		upper, err := strconv.ParseFloat(bound, 64)
		if err != nil {
			continue
		}
		if cumulativeCount < 0 {
			continue
		}
		buckets[upper] = uint64(cumulativeCount)
	}

	snapshot := &histogramSnapshot{
		labelValues: append([]string(nil), labelValues...),
		count:       uint64(value.Count),
		sum:         value.Sum,
		buckets:     buckets,
	}

	key := strings.Join(labelValues, "\xff")

	c.mu.Lock()
	c.data[key] = snapshot
	c.mu.Unlock()
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
