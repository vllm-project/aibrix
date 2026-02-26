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

// FetchTypedMetric fetches a single typed metric from an engine endpoint
// Note: if the client needs to fetch multiple metrics, it's better to use FetchAllTypedMetrics
func (ef *EngineMetricsFetcher) FetchTypedMetric(ctx context.Context, endpoint, engineType, identifier, metricName string) (MetricValue, error) {
	// Get metric definition from central registry
	metricDef, exists := Metrics[metricName]
	if !exists {
		return nil, fmt.Errorf("metric %s not found in central registry", metricName)
	}

	// Only support raw pod metrics for simple fetching
	if metricDef.MetricSource != PodRawMetrics {
		return nil, fmt.Errorf("metric %s is not a raw pod metric, use FetchAllTypedMetrics for complex queries", metricName)
	}

	// Get raw metric name for this engine
	rawMetricName, exists := metricDef.EngineMetricsNameMapping[engineType]
	if !exists {
		return nil, fmt.Errorf("metric %s not supported for engine type %s", metricName, engineType)
	}

	url := fmt.Sprintf("http://%s/metrics", endpoint)

	// Fetch with retry logic
	for attempt := 0; attempt <= ef.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := ef.calculateBackoffDelay(attempt)
			klog.V(4).InfoS("Retrying typed metric fetch from engine endpoint",
				"attempt", attempt, "delay", delay, "identifier", identifier, "metric", metricName)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Fetch all metrics and parse the one we need
		allMetrics, err := ef.fetchAllMetricsFromURL(ctx, url)
		if err != nil {
			klog.V(4).InfoS("Failed to fetch metrics from engine endpoint",
				"attempt", attempt+1, "identifier", identifier, "error", err)
			continue
		}

		// Parse the specific metric we need
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

	url := fmt.Sprintf("http://%s/metrics", endpoint)

	// Fetch raw metrics with retry logic
	var allMetrics map[string]*dto.MetricFamily
	var err error

	for attempt := 0; attempt <= ef.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := ef.calculateBackoffDelay(attempt)
			klog.V(4).InfoS("Retrying all typed metrics fetch from engine endpoint",
				"attempt", attempt, "delay", delay, "identifier", identifier)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
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

	// Parse requested metrics or all available metrics
	// TODO: it's better to return all metrics from the engine instead, right now the interface still accepts a list of metrics for filter.
	// filter logic could go outside.
	metricsToProcess := requestedMetrics
	if len(metricsToProcess) == 0 {
		// Get all available metrics for this engine type
		metricsToProcess = ef.getAvailableMetricsForEngine(result.EngineType)
	}

	// Process each requested metric
	for _, metricName := range metricsToProcess {
		metricDef, exists := Metrics[metricName]
		if !exists {
			result.Errors = append(result.Errors, fmt.Errorf("metric %s not found in central registry", metricName))
			continue
		}

		// Only process raw pod metrics (Prometheus queries handled separately)
		if metricDef.MetricSource != PodRawMetrics {
			continue
		}

		// Get raw metric name for this engine
		rawMetricName, exists := metricDef.EngineMetricsNameMapping[result.EngineType]
		if !exists {
			klog.V(5).InfoS("Metric not supported for engine type", "metric", metricName, "engine", result.EngineType)
			continue
		}

		// Parse the metric
		metricValue, err := ef.parseMetricFromFamily(allMetrics, rawMetricName, metricDef)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to parse metric %s: %v in endpoint %s", metricName, err, identifier))
			continue
		}

		// Store in appropriate scope
		if metricDef.MetricScope == PodMetricScope {
			result.Metrics[metricName] = metricValue
		} else if metricDef.MetricScope == PodModelMetricScope {
			// For model-scoped metrics, we need to extract model names from the raw metrics
			modelNames := ef.extractModelNamesFromMetrics(allMetrics, rawMetricName)
			for _, modelName := range modelNames {
				key := fmt.Sprintf("%s/%s", modelName, metricName)
				result.ModelMetrics[key] = metricValue
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

// calculateBackoffDelay calculates exponential backoff delay
func (ef *EngineMetricsFetcher) calculateBackoffDelay(attempt int) time.Duration {
	delay := time.Duration(float64(ef.config.BaseDelay) * math.Pow(2, float64(attempt-1)))
	if delay > ef.config.MaxDelay {
		delay = ef.config.MaxDelay
	}
	return delay
}

// getAvailableMetricsForEngine returns all metrics available for a given engine type
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

// parseMetricFromFamily parses a specific metric from Prometheus metric families
func (ef *EngineMetricsFetcher) parseMetricFromFamily(allMetrics map[string]*dto.MetricFamily, rawMetricName string, metric Metric) (MetricValue, error) {
	metricFamily, exists := allMetrics[rawMetricName]
	if !exists {
		return nil, fmt.Errorf("raw metric %s not found", rawMetricName)
	}

	if len(metricFamily.Metric) == 0 {
		return nil, fmt.Errorf("no metric instances found for %s", rawMetricName)
	}

	// Take the first metric instance (could be enhanced to handle multiple instances)
	firstMetric := metricFamily.Metric[0]

	// Parse based on metric type
	if metric.MetricType.IsRawMetric() {
		switch metric.MetricType.Raw {
		case Gauge, Counter:
			simpleValue, err := GetCounterGaugeValue(firstMetric, metricFamily.GetType())
			if err != nil {
				return nil, fmt.Errorf("failed to parse counter/gauge metric %s: %v", rawMetricName, err)
			}
			return simpleValue, nil

		case Histogram:
			histValue, err := GetHistogramValue(firstMetric)
			if err != nil {
				return nil, fmt.Errorf("failed to parse histogram metric %s: %v", rawMetricName, err)
			}
			return histValue, nil

		default:
			return nil, fmt.Errorf("unsupported raw metric type: %v", metric.MetricType.Raw)
		}
	} else if metric.MetricType.Query == QueryLabel {
		label, err := GetLabelValueForKey(firstMetric, metric.LabelKey)
		if err != nil {
			return nil, fmt.Errorf("failed to extract label %s for metric %s: %v", metric.LabelKey, rawMetricName, err)
		}
		return &LabelValueMetricValue{Value: label}, nil
	}

	return nil, fmt.Errorf("unsupported metric type for raw parsing: %v", metric.MetricType)
}

// extractModelNamesFromMetrics extracts model names from metric labels
func (ef *EngineMetricsFetcher) extractModelNamesFromMetrics(allMetrics map[string]*dto.MetricFamily, rawMetricName string) []string {
	metricFamily, exists := allMetrics[rawMetricName]
	if !exists {
		return []string{}
	}

	modelNames := make(map[string]struct{}) // Use map to deduplicate
	for _, familyMetric := range metricFamily.Metric {
		// TODO: confirm whether vLLM/SGLang uses the same label_key.
		if modelName, err := GetLabelValueForKey(familyMetric, "model_name"); err == nil && modelName != "" {
			modelNames[modelName] = struct{}{}
		}
	}

	// Convert to slice
	result := make([]string, 0, len(modelNames))
	for modelName := range modelNames {
		result = append(result, modelName)
	}
	return result
}

// fetchAllMetricsFromURL performs a single HTTP request and parses all Prometheus metrics
func (ef *EngineMetricsFetcher) fetchAllMetricsFromURL(ctx context.Context, url string) (map[string]*dto.MetricFamily, error) {
	// Use our configured HTTP client with the existing parsing logic
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %v", url, err)
	}

	resp, err := ef.client.Do(req)
	if err != nil {
		EmitMetricToPrometheus(nil, nil, LLMEngineMetricsQueryFail, &SimpleMetricValue{Value: 1.0}, nil)
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

	// Parse using existing Prometheus parser logic
	return ParseMetricsFromReader(resp.Body)
}
