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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock Prometheus metrics response
const mockVllmMetrics = `# HELP vllm_num_requests_running Number of requests currently running on GPU.
# TYPE vllm_num_requests_running gauge
vllm_num_requests_running{model_name="meta-llama/Llama-2-7b-chat-hf"} 2.0
# HELP vllm_num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm_num_requests_waiting gauge
vllm_num_requests_waiting{model_name="meta-llama/Llama-2-7b-chat-hf"} 3.0
# HELP vllm_kv_cache_usage_perc GPU KV-cache usage. 1.0 means 100 percent usage.
# TYPE vllm_kv_cache_usage_perc gauge
vllm_kv_cache_usage_perc 0.75
# HELP vllm_time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE vllm_time_to_first_token_seconds histogram
vllm_time_to_first_token_seconds_bucket{model_name="meta-llama/Llama-2-7b-chat-hf",le="0.001"} 0.0
vllm_time_to_first_token_seconds_bucket{model_name="meta-llama/Llama-2-7b-chat-hf",le="0.005"} 0.0
vllm_time_to_first_token_seconds_bucket{model_name="meta-llama/Llama-2-7b-chat-hf",le="0.01"} 1.0
vllm_time_to_first_token_seconds_bucket{model_name="meta-llama/Llama-2-7b-chat-hf",le="0.025"} 3.0
vllm_time_to_first_token_seconds_bucket{model_name="meta-llama/Llama-2-7b-chat-hf",le="+Inf"} 5.0
vllm_time_to_first_token_seconds_sum{model_name="meta-llama/Llama-2-7b-chat-hf"} 0.15
vllm_time_to_first_token_seconds_count{model_name="meta-llama/Llama-2-7b-chat-hf"} 5.0
`

const mockSglangMetrics = `# HELP sglang_running_requests Number of running requests.
# TYPE sglang_running_requests gauge
sglang_running_requests{model_name="meta-llama/Llama-2-7b-chat-hf"} 1.0
# HELP sglang_waiting_requests Number of waiting requests.
# TYPE sglang_waiting_requests gauge
sglang_waiting_requests{model_name="meta-llama/Llama-2-7b-chat-hf"} 2.0
# HELP sglang_cache_usage Cache usage percentage.
# TYPE sglang_cache_usage gauge
sglang_cache_usage 0.65
`

func setupMockServer(metrics string, statusCode int, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(statusCode)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, metrics)
	}))
}

func setupMockMetrics() {
	// Add test metrics to the global Metrics registry for testing
	Metrics["running_requests"] = Metric{
		MetricSource: PodRawMetrics,
		MetricType:   MetricType{Raw: Gauge},
		EngineMetricsNameMapping: map[string]string{
			"vllm":   "vllm_num_requests_running",
			"sglang": "sglang_running_requests",
		},
		Description: "Number of running requests",
		MetricScope: PodModelMetricScope,
	}

	Metrics["waiting_requests"] = Metric{
		MetricSource: PodRawMetrics,
		MetricType:   MetricType{Raw: Gauge},
		EngineMetricsNameMapping: map[string]string{
			"vllm":   "vllm_num_requests_waiting",
			"sglang": "sglang_waiting_requests",
		},
		Description: "Number of waiting requests",
		MetricScope: PodModelMetricScope,
	}

	Metrics["cache_usage"] = Metric{
		MetricSource: PodRawMetrics,
		MetricType:   MetricType{Raw: Gauge},
		EngineMetricsNameMapping: map[string]string{
			"vllm":   "vllm_kv_cache_usage_perc",
			"sglang": "sglang_cache_usage",
		},
		Description: "Cache usage percentage",
		MetricScope: PodMetricScope,
	}

	Metrics["time_to_first_token"] = Metric{
		MetricSource: PodRawMetrics,
		MetricType:   MetricType{Raw: Histogram},
		EngineMetricsNameMapping: map[string]string{
			"vllm": "vllm_time_to_first_token_seconds",
		},
		Description: "Time to first token histogram",
		MetricScope: PodModelMetricScope,
	}
}

func TestEngineMetricsFetcherConfig(t *testing.T) {
	t.Run("DefaultConfig", func(t *testing.T) {
		config := DefaultEngineMetricsFetcherConfig()
		assert.Equal(t, 10*time.Second, config.Timeout)
		assert.Equal(t, 3, config.MaxRetries)
		assert.Equal(t, 1*time.Second, config.BaseDelay)
		assert.Equal(t, 15*time.Second, config.MaxDelay)
		assert.True(t, config.InsecureTLS)
	})

	t.Run("CustomConfig", func(t *testing.T) {
		config := EngineMetricsFetcherConfig{
			Timeout:     5 * time.Second,
			MaxRetries:  2,
			BaseDelay:   500 * time.Millisecond,
			MaxDelay:    10 * time.Second,
			InsecureTLS: false,
		}

		fetcher := NewEngineMetricsFetcherWithConfig(config)
		assert.Equal(t, config, fetcher.config)
	})
}

func TestEngineMetricsFetcher_FetchTypedMetric(t *testing.T) {
	setupMockMetrics()

	tests := []struct {
		name          string
		metrics       string
		engineType    string
		metricName    string
		expectedValue float64
		expectedError string
		statusCode    int
	}{
		{
			name:          "FetchVllmRunningRequests",
			metrics:       mockVllmMetrics,
			engineType:    "vllm",
			metricName:    "running_requests",
			expectedValue: 2.0,
			statusCode:    200,
		},
		{
			name:          "FetchSglangRunningRequests",
			metrics:       mockSglangMetrics,
			engineType:    "sglang",
			metricName:    "running_requests",
			expectedValue: 1.0,
			statusCode:    200,
		},
		{
			name:          "FetchVllmCacheUsage",
			metrics:       mockVllmMetrics,
			engineType:    "vllm",
			metricName:    "cache_usage",
			expectedValue: 0.75,
			statusCode:    200,
		},
		{
			name:          "MetricNotFoundInRegistry",
			metrics:       mockVllmMetrics,
			engineType:    "vllm",
			metricName:    "nonexistent_metric",
			expectedError: "metric nonexistent_metric not found in central registry",
			statusCode:    200,
		},
		{
			name:          "MetricNotSupportedForEngine",
			metrics:       mockVllmMetrics,
			engineType:    "unsupported_engine",
			metricName:    "running_requests",
			expectedError: "metric running_requests not supported for engine type unsupported_engine",
			statusCode:    200,
		},
		{
			name:          "HTTPError",
			metrics:       "",
			engineType:    "vllm",
			metricName:    "running_requests",
			expectedError: "after 4 attempts", // Retry logic masks the original error
			statusCode:    500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := setupMockServer(tt.metrics, tt.statusCode, 0)
			defer server.Close()

			endpoint := strings.TrimPrefix(server.URL, "http://")

			fetcher := NewEngineMetricsFetcher()

			ctx := context.Background()
			value, err := fetcher.FetchTypedMetric(ctx, endpoint, tt.engineType, "test-pod", tt.metricName)

			if tt.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
			} else {
				require.NoError(t, err)
				require.NotNil(t, value)
				assert.Equal(t, tt.expectedValue, value.GetSimpleValue())
			}
		})
	}
}

func TestEngineMetricsFetcher_FetchAllTypedMetrics(t *testing.T) {
	setupMockMetrics()

	t.Run("FetchAllVllmMetrics", func(t *testing.T) {
		server := setupMockServer(mockVllmMetrics, 200, 0)
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		fetcher := NewEngineMetricsFetcher()

		ctx := context.Background()
		result, err := fetcher.FetchAllTypedMetrics(ctx, endpoint, "vllm", "test-pod", nil)

		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, "test-pod", result.Identifier)
		assert.Equal(t, endpoint, result.Endpoint)
		assert.Equal(t, "vllm", result.EngineType)

		// Check pod-scoped metrics
		assert.Contains(t, result.Metrics, "cache_usage")
		assert.Equal(t, 0.75, result.Metrics["cache_usage"].GetSimpleValue())

		// Check model-scoped metrics
		assert.Contains(t, result.ModelMetrics, "meta-llama/Llama-2-7b-chat-hf/running_requests")
		assert.Equal(t, 2.0, result.ModelMetrics["meta-llama/Llama-2-7b-chat-hf/running_requests"].GetSimpleValue())

		assert.Contains(t, result.ModelMetrics, "meta-llama/Llama-2-7b-chat-hf/waiting_requests")
		assert.Equal(t, 3.0, result.ModelMetrics["meta-llama/Llama-2-7b-chat-hf/waiting_requests"].GetSimpleValue())

		// Check histogram metrics
		assert.Contains(t, result.ModelMetrics, "meta-llama/Llama-2-7b-chat-hf/time_to_first_token")
		histMetric := result.ModelMetrics["meta-llama/Llama-2-7b-chat-hf/time_to_first_token"]
		histValue := histMetric.GetHistogramValue()
		require.NotNil(t, histValue)
		assert.Equal(t, 0.15, histValue.Sum)
		assert.Equal(t, 5.0, histValue.Count)
	})

	t.Run("FetchSpecificMetrics", func(t *testing.T) {
		server := setupMockServer(mockVllmMetrics, 200, 0)
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		fetcher := NewEngineMetricsFetcher()

		ctx := context.Background()
		requestedMetrics := []string{"running_requests", "cache_usage"}
		result, err := fetcher.FetchAllTypedMetrics(ctx, endpoint, "vllm", "test-pod", requestedMetrics)

		require.NoError(t, err)
		require.NotNil(t, result)

		// Should have requested metrics
		assert.Contains(t, result.Metrics, "cache_usage")
		assert.Contains(t, result.ModelMetrics, "meta-llama/Llama-2-7b-chat-hf/running_requests")

		// Should not have unrequested metrics
		assert.NotContains(t, result.ModelMetrics, "meta-llama/Llama-2-7b-chat-hf/waiting_requests")
	})
}

func TestEngineMetricsFetcher_RetryLogic(t *testing.T) {
	setupMockMetrics()

	t.Run("RetryOnHTTPError", func(t *testing.T) {
		callCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			if callCount < 3 {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(200)
			fmt.Fprint(w, mockVllmMetrics)
		}))
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		config := EngineMetricsFetcherConfig{
			Timeout:     5 * time.Second,
			MaxRetries:  3,
			BaseDelay:   10 * time.Millisecond, // Short delay for testing
			MaxDelay:    100 * time.Millisecond,
			InsecureTLS: true,
		}
		fetcher := NewEngineMetricsFetcherWithConfig(config)

		ctx := context.Background()
		value, err := fetcher.FetchTypedMetric(ctx, endpoint, "vllm", "test-pod", "running_requests")

		require.NoError(t, err)
		assert.Equal(t, 2.0, value.GetSimpleValue())
		assert.Equal(t, 3, callCount)
	})

	t.Run("ExceedMaxRetries", func(t *testing.T) {
		server := setupMockServer("", 500, 0)
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		config := EngineMetricsFetcherConfig{
			Timeout:     1 * time.Second,
			MaxRetries:  2,
			BaseDelay:   10 * time.Millisecond,
			MaxDelay:    100 * time.Millisecond,
			InsecureTLS: true,
		}
		fetcher := NewEngineMetricsFetcherWithConfig(config)

		ctx := context.Background()
		_, err := fetcher.FetchTypedMetric(ctx, endpoint, "vllm", "test-pod", "running_requests")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "after 3 attempts")
	})
}

func TestEngineMetricsFetcher_BackoffDelay(t *testing.T) {
	config := DefaultEngineMetricsFetcherConfig()
	fetcher := NewEngineMetricsFetcherWithConfig(config)

	tests := []struct {
		attempt     int
		expectedMin time.Duration
		expectedMax time.Duration
	}{
		{1, 1 * time.Second, 2 * time.Second},
		{2, 2 * time.Second, 4 * time.Second},
		{3, 4 * time.Second, 8 * time.Second},
		{4, 8 * time.Second, 15 * time.Second},  // Capped at MaxDelay
		{5, 15 * time.Second, 15 * time.Second}, // Still capped
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("Attempt%d", tt.attempt), func(t *testing.T) {
			delay := fetcher.calculateBackoffDelay(tt.attempt)
			assert.GreaterOrEqual(t, delay, tt.expectedMin)
			assert.LessOrEqual(t, delay, tt.expectedMax)
		})
	}
}

func TestEngineMetricsFetcher_ContextCancellation(t *testing.T) {
	setupMockMetrics()

	server := setupMockServer(mockVllmMetrics, 200, 100*time.Millisecond)
	defer server.Close()

	endpoint := strings.TrimPrefix(server.URL, "http://")

	fetcher := NewEngineMetricsFetcher()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := fetcher.FetchTypedMetric(ctx, endpoint, "vllm", "test-pod", "running_requests")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestEngineMetricsFetcher_InvalidMetrics(t *testing.T) {
	setupMockMetrics()

	t.Run("InvalidPrometheusFormat", func(t *testing.T) {
		server := setupMockServer("invalid metrics format", 200, 0)
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		fetcher := NewEngineMetricsFetcher()

		ctx := context.Background()
		_, err := fetcher.FetchTypedMetric(ctx, endpoint, "vllm", "test-pod", "running_requests")

		require.Error(t, err)
	})

	t.Run("MetricNotFound", func(t *testing.T) {
		emptyMetrics := `# No metrics here`
		server := setupMockServer(emptyMetrics, 200, 0)
		defer server.Close()

		endpoint := strings.TrimPrefix(server.URL, "http://")

		fetcher := NewEngineMetricsFetcher()

		ctx := context.Background()
		_, err := fetcher.FetchTypedMetric(ctx, endpoint, "vllm", "test-pod", "running_requests")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "after 4 attempts")
	})
}

func TestEngineMetricsResult(t *testing.T) {
	result := &EngineMetricsResult{
		Identifier:   "test-pod",
		Endpoint:     "10.0.0.1:8000",
		EngineType:   "vllm",
		Metrics:      make(map[string]MetricValue),
		ModelMetrics: make(map[string]MetricValue),
		Errors:       []error{},
	}

	result.Metrics["cache_usage"] = &SimpleMetricValue{Value: 0.75}
	result.ModelMetrics["model1/running_requests"] = &SimpleMetricValue{Value: 2.0}
	result.Errors = append(result.Errors, fmt.Errorf("test error"))

	assert.Equal(t, "test-pod", result.Identifier)
	assert.Equal(t, "10.0.0.1:8000", result.Endpoint)
	assert.Equal(t, "vllm", result.EngineType)
	assert.Equal(t, 0.75, result.Metrics["cache_usage"].GetSimpleValue())
	assert.Equal(t, 2.0, result.ModelMetrics["model1/running_requests"].GetSimpleValue())
	assert.Len(t, result.Errors, 1)
	assert.Equal(t, "test error", result.Errors[0].Error())
}
