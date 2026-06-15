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

package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/expfmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// ParseHistogramFromBody parses a histogram metric from the Prometheus response body.
func ParseHistogramFromBody(body []byte, metricName string) (*HistogramMetricValue, error) {
	lines := strings.Split(string(body), "\n")
	histogram := &HistogramMetricValue{
		Buckets: make(map[string]float64),
	}
	found := false

	for _, line := range lines {
		if strings.Contains(line, metricName+"_sum") {
			value, err := extractMetricValue(line)
			if err != nil {
				return nil, fmt.Errorf("failed to parse sum for metric %s: %w", metricName, err)
			}
			histogram.Sum = value
			found = true
		} else if strings.Contains(line, metricName+"_count") {
			value, err := extractMetricValue(line)
			if err != nil {
				return nil, fmt.Errorf("failed to parse count for metric %s: %w", metricName, err)
			}
			histogram.Count = value
			found = true
		} else if strings.Contains(line, metricName+"_bucket") {
			bucketBoundary := extractBucketBoundary(line)
			if bucketBoundary == "" {
				return nil, fmt.Errorf("failed to extract bucket boundary for metric %s", metricName)
			}
			value, err := extractMetricValue(line)
			if err != nil {
				return nil, fmt.Errorf("failed to parse bucket value for %s: %w", metricName, err)
			}
			histogram.Buckets[bucketBoundary] = value
			found = true
		}
	}

	if !found {
		return nil, fmt.Errorf("metrics %s not found", metricName)
	}
	return histogram, nil
}

// extractBucketBoundary extracts the `le` label from a bucket line.
func extractBucketBoundary(line string) string {
	startIndex := strings.Index(line, `le="`)
	if startIndex == -1 {
		return ""
	}
	startIndex += len(`le="`)
	endIndex := strings.Index(line[startIndex:], `"`)
	if endIndex == -1 {
		return ""
	}
	return line[startIndex : startIndex+endIndex]
}

// extractMetricValue extracts the metric value from a Prometheus metric line.
func extractMetricValue(line string) (float64, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("unexpected format: %s", line)
	}
	return strconv.ParseFloat(parts[len(parts)-1], 64)
}

// ParseMetricFromBody parses a simple metric from the Prometheus response body.
func ParseMetricFromBody(body []byte, metricName string) (float64, error) {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, metricName) {
			value, err := extractMetricValue(line)
			if err != nil {
				return 0, fmt.Errorf("failed to parse metric value for %s: %w", metricName, err)
			}
			return value, nil
		}
	}
	return 0, fmt.Errorf("metrics %s not found", metricName)
}

// BuildQuery dynamically injects labels into a PromQL query template.
func BuildQuery(queryTemplate string, queryLabels map[string]string) string {
	placeholderPattern := regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

	// Replace placeholders with actual values from queryLabels
	queryWithReplacements := placeholderPattern.ReplaceAllStringFunc(queryTemplate, func(match string) string {
		key := placeholderPattern.FindStringSubmatch(match)[1]
		if value, exists := queryLabels[key]; exists {
			return value
		}
		return match
	})

	// Collect additional labels
	var additionalLabels []string
	for key, value := range queryLabels {
		if !strings.Contains(queryTemplate, fmt.Sprintf("${%s}", key)) {
			additionalLabels = append(additionalLabels, fmt.Sprintf(`%s="%s"`, key, value))
		}
	}

	// Append additional labels to the query
	if len(additionalLabels) > 0 {
		labels := strings.Join(additionalLabels, ",")
		if strings.Contains(queryWithReplacements, "{") {
			queryWithReplacements = strings.Replace(queryWithReplacements, "{", fmt.Sprintf("{%s,", labels), 1)
		} else {
			queryWithReplacements = fmt.Sprintf("%s{%s}", queryWithReplacements, labels)
		}
	}

	return queryWithReplacements
}

// InitializePrometheusAPI initializes the Prometheus API client.
func InitializePrometheusAPI(endpoint, username, password string) (prometheusv1.API, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("prometheus endpoint is not provided")
	}

	client, err := api.NewClient(api.Config{
		Address: endpoint,
		RoundTripper: config.NewBasicAuthRoundTripper(config.NewInlineSecret(username),
			config.NewInlineSecret(password), api.DefaultRoundTripper),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	return prometheusv1.NewAPI(client), nil
}

func GetLabelValueForKey(metric *dto.Metric, key string) (string, error) {
	for _, labelPair := range metric.Label {
		if labelPair.GetName() == key {
			return labelPair.GetValue(), nil
		}
	}
	return "", fmt.Errorf("Label %s not found", key)
}

func GetCounterGaugeValue(metric *dto.Metric, metricType dto.MetricType) (*SimpleMetricValue, error) {
	labels := make(map[string]string)
	for _, labelPair := range metric.Label {
		labels[labelPair.GetName()] = labelPair.GetValue()
	}
	switch metricType {
	case dto.MetricType_COUNTER:
		return &SimpleMetricValue{Value: metric.GetCounter().GetValue(), Labels: labels}, nil
	case dto.MetricType_GAUGE:
		return &SimpleMetricValue{Value: metric.GetGauge().GetValue(), Labels: labels}, nil
	default:
		return nil, fmt.Errorf("Metric type not supported: %v", metricType)
	}
}

func GetHistogramValue(metric *dto.Metric) (*HistogramMetricValue, error) {
	histogram := &HistogramMetricValue{
		Buckets: make(map[string]float64),
		Labels:  make(map[string]string),
	}
	histogramMetric := metric.GetHistogram()
	if histogramMetric == nil {
		return nil, fmt.Errorf("Histogram metric not found")
	}

	histogram.Sum = histogramMetric.GetSampleSum()
	histogram.Count = float64(histogramMetric.GetSampleCount())
	for _, bucket := range histogramMetric.GetBucket() {
		bound := fmt.Sprintf("%f", bucket.GetUpperBound())
		histogram.Buckets[bound] = float64(bucket.GetCumulativeCount())
	}
	for _, labelPair := range metric.Label {
		histogram.Labels[labelPair.GetName()] = labelPair.GetValue()
	}
	return histogram, nil
}

func ParseMetricsURLWithContext(ctx context.Context, url string) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Failed to create request for %s: %v\n", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch metrics from %s: %v\n", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.ErrorS(err, "failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bad status code while fetching metrics from %s: %d\n", url, resp.StatusCode)
	}

	var parser expfmt.TextParser
	allMetrics, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error parsing metric families: %v\n", err)
	}
	return allMetrics, nil
}

// ParseMetricsFromReader parses Prometheus metrics from an io.Reader (extracted for reuse)
func ParseMetricsFromReader(reader io.Reader) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	allMetrics, err := parser.TextToMetricFamilies(reader)
	if err != nil {
		return nil, fmt.Errorf("error parsing metric families: %v", err)
	}
	return allMetrics, nil
}

// GetEngineType extracts the engine type from pod labels, defaults to "vllm" for backward compatibility
// This function is centralized to avoid duplication across packages
func GetEngineType(pod v1.Pod) string {
	if engineType, exists := pod.Labels[constants.ModelLabelEngine]; exists && engineType != "" {
		return engineType
	}
	return "vllm" // Default to vllm for backward compatibility
}

func HttpFailureStatusCode(ctx context.Context, err error, resp *http.Response) (string, string) {
	// 1. HTTP response status code
	if resp != nil {
		return "http_error", strconv.Itoa(resp.StatusCode)
	}

	// 2. No error and no response (should not happen, but be safe)
	if err == nil {
		return "ok", "200"
	}

	// 3. Context always wins
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return "context_canceled", "499"
		case context.DeadlineExceeded:
			return "deadline_exceeded", "504"
		}
	}

	// 4. Unwrap url.Error if present
	if urlErr, ok := err.(*url.Error); ok {
		err = urlErr.Err
	}

	// 5. Error-level classification
	if errors.Is(err, context.Canceled) {
		return "context_canceled", "499"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded", "504"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout", "504"
	}

	// 6. Fallback
	return "error", "502"
}
