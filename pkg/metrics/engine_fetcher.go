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
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/klog/v2"
)

// EngineMetricsFetcherConfig holds configuration for engine metrics fetching
type EngineMetricsFetcherConfig struct {
	Timeout     time.Duration
	MaxRetries  int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	InsecureTLS bool
}

// DefaultEngineMetricsFetcherConfig returns sensible defaults for engine metrics fetching
func DefaultEngineMetricsFetcherConfig() EngineMetricsFetcherConfig {
	return EngineMetricsFetcherConfig{
		Timeout:     10 * time.Second,
		MaxRetries:  3,
		BaseDelay:   1 * time.Second,
		MaxDelay:    15 * time.Second,
		InsecureTLS: true, // Engine pods typically use self-signed certs
	}
}

// EngineMetricsFetcher provides a unified interface for fetching typed metrics from inference engine pods
// It leverages the centralized metrics registry and type system in pkg/metrics
type EngineMetricsFetcher struct {
	client *http.Client
	config EngineMetricsFetcherConfig
}

// NewEngineMetricsFetcher creates a new engine metrics fetcher with default configuration
func NewEngineMetricsFetcher() *EngineMetricsFetcher {
	return NewEngineMetricsFetcherWithConfig(DefaultEngineMetricsFetcherConfig())
}

// NewEngineMetricsFetcherWithConfig creates a new engine metrics fetcher with custom configuration
func NewEngineMetricsFetcherWithConfig(config EngineMetricsFetcherConfig) *EngineMetricsFetcher {
	transport := &http.Transport{}
	if config.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &EngineMetricsFetcher{
		client: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
		},
		config: config,
	}
}

// EngineMetricsResult contains the result of fetching metrics from an engine endpoint
type EngineMetricsResult struct {
	Identifier   string // Caller-provided identifier (e.g., pod name)
	Endpoint     string // The endpoint that was queried
	EngineType   string
	Metrics      map[string]MetricValue // Pod-scoped metrics
	ModelMetrics map[string]MetricValue // Pod+Model-scoped metrics (key format: "model/metric")
	Errors       []error                // Any errors encountered during fetching
}

// metricsPathForEngine returns the Prometheus metrics path for the given engine type.
// trtllm exposes Prometheus-compatible metrics at /prometheus/metrics, all others use /metrics.
func metricsPathForEngine(engineType string) string {
	if engineType == "trtllm" {
		return "prometheus/metrics"
	}
	return "metrics"
}

// FetchTypedMetric fetches a single typed metric from an engine endpoint
func (ef *EngineMetricsFetcher) FetchTypedMetric(ctx context.Context, endpoint, engineType, identifier, metricName string) (MetricValue, error) {
	metricDef, exists := Metrics[metricName]
	if !exists {
		return nil, fmt.Errorf("metric %s not found in central registry", metricName)
	}
	if metricDef.MetricSource != PodRawMetrics {
		return nil, fmt.Errorf("metric %s is not a raw pod metric, use FetchAllTypedMetrics for complex queries", metricName)
	}
	rawMetricName, exists := metricDef.EngineMetricsNameMapping[engineType]
	if !exists {
		return nil, fmt.Errorf("metric %s not supported for engine type %s", metricName, engineType)
	}
	url := fmt.Sprintf("http://%s/%s", endpoint, metricsPathForEngine(engineType))
	for attempt := 0; attempt <= ef.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := ef.calculateBackoffDelay(attempt)
			klog.V(4).InfoS("Retrying typed metric fetch from engine endpoint",
				"attempt", attempt, "delay", delay, "identifier", identifier, "metric", metricName)
			if err := sleepWithContext(ctx, delay); err != nil {
				return nil, err
			}
		}
		allMetrics, err := ef.fetchAllMetricsFromURL(ctx, url)
		if err != nil {
			klog.V(4).InfoS("Failed to fetch metrics from engine endpoint",
				"attempt", attempt+1, "identifier", identifier, "error", err)
			continue
		}
		metricValue, err := ef.parseMetricFromFamily(allMetrics, rawMetricName, metricDef)
		if err != nil {
			klog.V(4).InfoS("Failed to parse metric from engine endpoint",
				"attempt", attempt+1, "identifier", identifier, "metric", metricName, "error", err)
			continue
		}
		klog.V(4).InfoS("Successfully fetched typed metric from engine endpoint",
			"identifier", identifier, "metric", metricName, "value", metricValue, "attempt", attempt+1)
		return metricValue, nil
	}
	return nil, fmt.Errorf("failed to fetch typed metric %s from engine endpoint %s after %d attempts",
		metricName, identifier, ef.config.MaxRetries+1)
}

// FetchRawMetric fetches a metric by its raw Prometheus name from an explicit metrics URL,
// bypassing the central metric registry.
func (ef *EngineMetricsFetcher) FetchRawMetric(ctx context.Context, url, identifier, rawMetricName string) (MetricValue, error) {
	for attempt := 0; attempt <= ef.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := ef.calculateBackoffDelay(attempt)
			klog.V(4).InfoS("Retrying raw metric fetch",
				"attempt", attempt, "delay", delay, "identifier", identifier, "metric", rawMetricName)
			if err := sleepWithContext(ctx, delay); err != nil {
				return nil, err
			}
		}
		allMetrics, err := ef.fetchMetricsFromURL(ctx, url, "")
		if err != nil {
			klog.V(4).InfoS("Failed to fetch metrics from URL",
				"attempt", attempt+1, "identifier", identifier, "url", url, "error", err)
			continue
		}
		family, exists := allMetrics[rawMetricName]
		if !exists || len(family.Metric) == 0 {
			klog.V(4).InfoS("Raw metric not found in response",
				"attempt", attempt+1, "identifier", identifier, "metric", rawMetricName)
			continue
		}
		metricValue, err := GetCounterGaugeValue(family.Metric[0], family.GetType())
		if err != nil {
			return nil, fmt.Errorf("failed to parse raw metric %s from %s: %w", rawMetricName, identifier, err)
		}
		return metricValue, nil
	}
	return nil, fmt.Errorf("failed to fetch raw metric %s from %s after %d attempts",
		rawMetricName, identifier, ef.config.MaxRetries+1)
}

// FetchAllTypedMetrics fetches all available typed metrics from an engine endpoint
func (ef *EngineMetricsFetcher) FetchAllTypedMetrics(ctx context.Context, endpoint, engineType, identifier string, requestedMetrics []string) (*EngineMetricsResult, error) {
	result := &EngineMetricsResult{
		Identifier:   identifier,
		Endpoint:     endpoint,
		EngineType:   engineType,
		Metrics:      make(map[string]MetricValue),
		ModelMetrics: make(map[string]MetricValue),
		Errors:       []error{},
	}
	url := fmt.Sprintf("http://%s/%s", endpoint, metricsPathForEngine(engineType))
	var allMetrics map[string]*dto.MetricFamily
	var err error
	for attempt := 0; attempt <= ef.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := ef.calculateBackoffDelay(attempt)
			klog.V(4).InfoS("Retrying all typed metrics fetch from engine endpoint",
				"attempt", attempt, "delay", delay, "identifier", identifier)
			if err := sleepWithContext(ctx, delay); err != nil {
				return nil, err
			}
		}
		allMetrics, err = ef.fetchAllMetricsFromURL(ctx, url)
		if err == nil {
			klog.V(4).InfoS("Successfully fetched raw metrics from engine endpoint",
				"identifier", identifier, "rawMetricsCount", len(allMetrics), "attempt", attempt+1)
			break
		}
		klog.V(4).InfoS("Failed to fetch raw metrics from engine endpoint",
			"attempt", attempt+1, "identifier", identifier, "error", err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch raw metrics from engine endpoint %s after %d attempts: %v",
			identifier, ef.config.MaxRetries+1, err)
	}
	metricsToProcess := requestedMetrics
	if len(metricsToProcess) == 0 {
		metricsToProcess = ef.getAvailableMetricsForEngine(result.EngineType)
	}
	for _, metricName := range metricsToProcess {
		metricDef, exists := Metrics[metricName]
		if !exists {
			result.Errors = append(result.Errors, fmt.Errorf("metric %s not found in central registry", metricName))
			continue
		}
		if metricDef.MetricSource != PodRawMetrics {
			continue
		}
		rawMetricName, exists := metricDef.EngineMetricsNameMapping[result.EngineType]
		if !exists {
			klog.V(5).InfoS("Metric not supported for engine type", "metric", metricName, "engine", result.EngineType)
			continue
		}
		if metricDef.MetricScope == PodMetricScope {
			metricValue, err := ef.parseMetricFromFamily(allMetrics, rawMetricName, metricDef)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to parse metric %s: %v in endpoint %s", metricName, err, identifier))
				continue
			}
			result.Metrics[metricName] = metricValue
		} else if metricDef.MetricScope == PodModelMetricScope {
			modelMetrics, err := ef.parseModelMetricsFromFamily(allMetrics, rawMetricName, metricDef)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("failed to parse metric %s: %v in endpoint %s", metricName, err, identifier))
				continue
			}
			for modelName, value := range modelMetrics {
				key := fmt.Sprintf("%s/%s", modelName, metricName)
				result.ModelMetrics[key] = value
			}
		}
		klog.V(5).InfoS("Successfully processed typed metric",
			"identifier", identifier, "metric", metricName, "scope", metricDef.MetricScope)
	}
	klog.V(4).InfoS("Completed typed metrics processing for engine endpoint",
		"identifier", identifier, "engine", result.EngineType,
		"podMetrics", len(result.Metrics), "modelMetrics", len(result.ModelMetrics),
		"errors", len(result.Errors))
	return result, nil
}

// Helper methods

func (ef *EngineMetricsFetcher) calculateBackoffDelay(attempt int) time.Duration {
	delay := time.Duration(float64(ef.config.BaseDelay) * math.Pow(2, float64(attempt-1)))
	if delay > ef.config.MaxDelay {
		delay = ef.config.MaxDelay
	}
	return delay
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (ef *EngineMetricsFetcher) getAvailableMetricsForEngine(engineType string) []string {
	var availableMetrics []string
	for metricName, metricDef := range Metrics {
		if metricDef.MetricSource == PodRawMetrics {
			if _, exists := metricDef.EngineMetricsNameMapping[engineType]; exists {
				availableMetrics = append(availableMetrics, metricName)
			}
		}
	}
	return availableMetrics
}

func (ef *EngineMetricsFetcher) parseMetricFromFamily(allMetrics map[string]*dto.MetricFamily, rawMetricName string, metric Metric) (MetricValue, error) {
	metricFamily, exists := allMetrics[rawMetricName]
	if !exists {
		return nil, fmt.Errorf("raw metric %s not found", rawMetricName)
	}
	if len(metricFamily.Metric) == 0 {
		return nil, fmt.Errorf("no metric instances found for %s", rawMetricName)
	}
	instance, err := selectMetricInstance(metricFamily.Metric, metric, rawMetricName)
	if err != nil {
		return nil, err
	}
	return ef.parseMetricInstance(instance, metricFamily, metric, rawMetricName)
}

// isPriorityTotal returns true when the instance carries no "priority" label
// or carries one with an empty value. SGLang's priority scheduling metrics
// mark their queue total with priority="" and per-priority breakdowns with
// priority="1", priority="5", etc.; this helper identifies the total.
func isPriorityTotal(instance *dto.Metric) bool {
	value, err := GetLabelValueForKey(instance, "priority")
	return err != nil || value == ""
}

// selectMetricInstance returns the family instance parseMetricFromFamily
// should read. Most metrics report exactly one instance per pod/model and
// keep today's behavior of taking the first one; metric.RequiredLabelKey
// narrows that to the instance whose label matches, for a family that
// reports several instances under the same name distinguished only by a
// label.
//
// When no RequiredLabelKey is set and the family contains multiple
// instances, prefer the one that represents the queue total (empty or
// absent priority label) over per-priority breakdowns, regardless of the
// order the scrape reported them in.
func selectMetricInstance(instances []*dto.Metric, metric Metric, rawMetricName string) (*dto.Metric, error) {
	if metric.RequiredLabelKey == "" {
		for _, instance := range instances {
			if isPriorityTotal(instance) {
				return instance, nil
			}
		}
		return instances[0], nil
	}
	for _, instance := range instances {
		value, err := GetLabelValueForKey(instance, metric.RequiredLabelKey)
		if err == nil && value == metric.RequiredLabelValue {
			return instance, nil
		}
	}
	return nil, fmt.Errorf(
		"no instance of %s has label %s=%s",
		rawMetricName, metric.RequiredLabelKey, metric.RequiredLabelValue,
	)
}

// parseModelMetricsFromFamily returns one MetricValue per model_name in the family.
// Instances that repeat a model_name (differing only by a non-model label) are folded
// together by aggregateModelMetric.
func (ef *EngineMetricsFetcher) parseModelMetricsFromFamily(allMetrics map[string]*dto.MetricFamily, rawMetricName string, metric Metric) (map[string]MetricValue, error) {
	metricFamily, exists := allMetrics[rawMetricName]
	if !exists {
		return nil, fmt.Errorf("raw metric %s not found", rawMetricName)
	}
	if len(metricFamily.Metric) == 0 {
		return nil, fmt.Errorf("no metric instances found for %s", rawMetricName)
	}
	modelMetrics := make(map[string]MetricValue)
	// modelIsTotal remembers, per model_name, whether the stored value came
	// from a priority-total (empty/absent priority label) instance.
	modelIsTotal := make(map[string]bool)
	for _, familyMetric := range metricFamily.Metric {
		modelName, err := GetLabelValueForKey(familyMetric, "model_name")
		if err != nil || modelName == "" {
			continue
		}
		if metric.RequiredLabelKey != "" {
			labelValue, labelErr := GetLabelValueForKey(familyMetric, metric.RequiredLabelKey)
			if labelErr != nil || labelValue != metric.RequiredLabelValue {
				continue
			}
		}
		value, err := ef.parseMetricInstance(familyMetric, metricFamily, metric, rawMetricName)
		if err != nil {
			klog.V(4).InfoS("skipping metric instance that failed to parse",
				"metric", rawMetricName, "model", modelName, "err", err)
			continue
		}
		isTotal := isPriorityTotal(familyMetric)
		existing, hasExisting := modelMetrics[modelName]
		existingTotal := modelIsTotal[modelName]
		switch {
		case isTotal && !hasExisting:
			modelMetrics[modelName] = value
			modelIsTotal[modelName] = true
		case isTotal && hasExisting && !existingTotal:
			modelMetrics[modelName] = value
			modelIsTotal[modelName] = true
		case !isTotal && hasExisting && existingTotal:
			// Already have the total — skip per-priority breakdowns.
		default:
			if hasExisting {
				value = aggregateModelMetric(existing, value, modelName, metric.MetricType.Raw)
			}
			modelMetrics[modelName] = value
			modelIsTotal[modelName] = isTotal
		}
	}
	if len(modelMetrics) == 0 {
		klog.V(4).InfoS("metric family has no model_name-labeled instances", "metric", rawMetricName)
	}
	return modelMetrics, nil
}

func aggregateModelMetric(existing, incoming MetricValue, modelName string, rawType RawMetricType) MetricValue {
	switch rawType {
	case Counter:
		existingVal, ok := existing.(*SimpleMetricValue)
		if !ok {
			return incoming
		}
		incomingVal, ok := incoming.(*SimpleMetricValue)
		if !ok {
			return incoming
		}
		existingVal.Value += incomingVal.Value
		existingVal.Labels = map[string]string{"model_name": modelName}
		return existingVal
	case Histogram:
		existingVal, ok := existing.(*HistogramMetricValue)
		if !ok {
			return incoming
		}
		incomingVal, ok := incoming.(*HistogramMetricValue)
		if !ok {
			return incoming
		}
		existingVal.Sum += incomingVal.Sum
		existingVal.Count += incomingVal.Count
		for bound, count := range incomingVal.Buckets {
			existingVal.Buckets[bound] += count
		}
		existingVal.Labels = map[string]string{"model_name": modelName}
		return existingVal
	case Gauge:
		return incoming
	default:
		return incoming
	}
}

func (ef *EngineMetricsFetcher) parseMetricInstance(familyMetric *dto.Metric, metricFamily *dto.MetricFamily, metric Metric, rawMetricName string) (MetricValue, error) {
	if metric.MetricType.IsRawMetric() {
		switch metric.MetricType.Raw {
		case Gauge, Counter:
			simpleValue, err := GetCounterGaugeValue(familyMetric, metricFamily.GetType())
			if err != nil {
				return nil, fmt.Errorf("failed to parse counter/gauge metric %s: %v", rawMetricName, err)
			}
			return simpleValue, nil
		case Histogram:
			histValue, err := GetHistogramValue(familyMetric)
			if err != nil {
				return nil, fmt.Errorf("failed to parse histogram metric %s: %v", rawMetricName, err)
			}
			return histValue, nil
		default:
			return nil, fmt.Errorf("unsupported raw metric type: %v", metric.MetricType.Raw)
		}
	} else if metric.MetricType.Query == QueryLabel {
		label, err := GetLabelValueForKey(familyMetric, metric.LabelKey)
		if err != nil {
			return nil, fmt.Errorf("failed to extract label %s for metric %s: %v", metric.LabelKey, rawMetricName, err)
		}
		return &LabelValueMetricValue{Value: label}, nil
	}
	return nil, fmt.Errorf("unsupported metric type for raw parsing: %v", metric.MetricType)
}

func (ef *EngineMetricsFetcher) fetchAllMetricsFromURL(ctx context.Context, url string) (map[string]*dto.MetricFamily, error) {
	return ef.fetchMetricsFromURL(ctx, url, LLMEngineMetricsQueryFail)
}

func (ef *EngineMetricsFetcher) fetchMetricsFromURL(ctx context.Context, url, failureMetric string) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %v", url, err)
	}
	resp, err := ef.client.Do(req)
	if err != nil {
		if failureMetric != "" {
			EmitMetricToPrometheus(nil, nil, failureMetric, &SimpleMetricValue{Value: 1.0}, nil)
		}
		return nil, fmt.Errorf("failed to fetch metrics from %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.ErrorS(err, "failed to close response body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code while fetching metrics from %s: %d", url, resp.StatusCode)
	}
	return ParseMetricsFromReader(resp.Body)
}
