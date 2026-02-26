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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

var (
	customGauges       = make(map[string]*prometheus.GaugeVec)
	customGaugesMu     sync.RWMutex
	customCounters     = make(map[string]*prometheus.CounterVec)
	customCountersMu   sync.RWMutex
	customHistograms   = make(map[string]*histogramCollector)
	customHistogramsMu sync.RWMutex
	gatewayPodName     = os.Getenv("POD_NAME")

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

func emitGaugeMetric(routingCtx *types.RoutingContext, pod *v1.Pod, name string, value float64, extras map[string]string) {
	var model string
	if routingCtx == nil {
		model = ""
	} else {
		model = routingCtx.Model
	}
	labelNames, labelValues := buildMetricLabels(pod, model, extras)
	SetGaugeMetric(name, GetMetricHelp(name), value, labelNames, labelValues...)
}

func emitCounterMetric(routingCtx *types.RoutingContext, pod *v1.Pod, name string, value float64, extras map[string]string) {
	var model string
	if routingCtx == nil {
		model = ""
	} else {
		model = routingCtx.Model
	}
	labelNames, labelValues := buildMetricLabels(pod, model, extras)
	IncrementCounterMetricFnForTest(name, GetMetricHelp(name), value, labelNames, labelValues...)
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

func EmitMetricToPrometheus(routingCtx *types.RoutingContext, pod *v1.Pod, metricName string, metricValue MetricValue, extra map[string]string) {
	metricDef, exists := Metrics[metricName]
	if !exists {
		return
	}
	var model string
	if routingCtx != nil {
		model = routingCtx.Model
	}

	switch metricDef.MetricType.Raw {
	case Gauge:
		emitGaugeMetric(routingCtx, pod, metricName, metricValue.GetSimpleValue(), extra)
	case Counter:
		emitCounterMetric(routingCtx, pod, metricName, metricValue.GetSimpleValue(), extra)
	default:
		labelNames, labelValues := buildMetricLabels(pod, model, extra)
		if hv := metricValue.GetHistogramValue(); hv != nil {
			SetHistogramMetric(metricName, GetMetricHelp(metricName), hv, labelNames, labelValues...)
			p50, _ := hv.GetPercentile(50)
			SetGaugeMetric(metricName+"_p50", GetMetricHelp(metricName), p50, labelNames, labelValues...)
			p90, _ := hv.GetPercentile(90)
			SetGaugeMetric(metricName+"_p90", GetMetricHelp(metricName), p90, labelNames, labelValues...)
			p99, _ := hv.GetPercentile(99)
			SetGaugeMetric(metricName+"_p99", GetMetricHelp(metricName), p99, labelNames, labelValues...)
		}
	}
}

func buildMetricLabels(pod *v1.Pod, model string, extras map[string]string) ([]string, []string) {
	defaultLabelMap := generateDefaultMetricLabelsMap(pod, model)
	labelNames := make([]string, 0, len(defaultLabelMap)+len(extras))
	labelValues := make([]string, 0, len(defaultLabelMap)+len(extras))
	for k, v := range defaultLabelMap {
		labelNames = append(labelNames, k)
		labelValues = append(labelValues, v)
	}

	if len(extras) > 0 {
		keys := make([]string, 0, len(extras))
		for k := range extras {
			if k == "" {
				continue
			}
			if _, exist := defaultLabelMap[k]; exist {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			labelNames = append(labelNames, k)
			labelValues = append(labelValues, extras[k])
		}
	}
	return labelNames, labelValues
}

func generateDefaultMetricLabelsMap(pod *v1.Pod, model string) map[string]string {
	if pod == nil {
		return map[string]string{
			"model":       model,
			"gateway_pod": gatewayPodName,
		}
	}
	return map[string]string{
		"namespace":          pod.Namespace,
		"pod":                pod.Name,
		"model":              model,
		"engine_type":        GetEngineType(*pod),
		"roleset":            utils.GetPodEnv(pod, "ROLESET_NAME", ""),
		"role":               utils.GetPodEnv(pod, "ROLE_NAME", ""),
		"role_replica_index": utils.GetPodEnv(pod, "ROLE_REPLICA_INDEX", ""),
		"gateway_pod":        gatewayPodName,
	}
}
