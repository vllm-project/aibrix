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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseHistogramWithLabels(t *testing.T) {
	body := []byte(`
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0
# HELP vllm:kv_cache_usage_perc GPU KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0
# HELP vllm:time_per_output_token_seconds histogram
vllm:time_per_output_token_seconds_sum{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.23455095291137695
vllm:time_per_output_token_seconds_count{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 29.0
vllm:time_per_output_token_seconds_bucket{le="0.1",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 29.0
vllm:time_per_output_token_seconds_bucket{le="0.5",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 29.0
vllm:time_per_output_token_seconds_bucket{le="+Inf",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 29.0
`)

	t.Run("Parse histogram with model labels", func(t *testing.T) {
		histogram, err := ParseHistogramFromBody(body, "vllm:time_per_output_token_seconds")

		assert.NoError(t, err)

		assert.Equal(t, 0.23455095291137695, histogram.Sum)
		assert.Equal(t, 29.0, histogram.Count)
		assert.Equal(t, map[string]float64{
			"0.1":  29.0,
			"0.5":  29.0,
			"+Inf": 29.0,
		}, histogram.Buckets)
	})
}

func TestParseMetricFromBody(t *testing.T) {
	body := []byte(`
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 2
`)

	t.Run("Parse simple metric with model labels", func(t *testing.T) {
		value, err := ParseMetricFromBody(body, "vllm:num_requests_waiting")
		assert.NoError(t, err)
		assert.Equal(t, 2.0, value)
	})
}

func TestExtractBucketBoundary(t *testing.T) {
	line := `vllm:time_per_output_token_seconds_bucket{le="0.1",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 29.0`

	t.Run("Extract bucket boundary with model labels", func(t *testing.T) {
		boundary := extractBucketBoundary(line)
		assert.Equal(t, "0.1", boundary)
	})
}

func TestBuildQueryWithModelLabel(t *testing.T) {
	queryTemplate := `sum(rate(${metric}[5m])) by (model_name)`
	queryLabels := map[string]string{
		"metric":     "vllm:time_per_output_token_seconds_bucket",
		"model_name": "Qwen/Qwen2.5-1.5B-Instruct",
	}

	t.Run("Build PromQL query with model labels", func(t *testing.T) {
		query := BuildQuery(queryTemplate, queryLabels)
		assert.Contains(t, query, `vllm:time_per_output_token_seconds_bucket`)
		assert.Contains(t, query, `model_name="Qwen/Qwen2.5-1.5B-Instruct"`)
	})
}

func TestParseMetricsURLWithContext(t *testing.T) {
	tests := []struct {
		name           string
		metricsContent string
		statusCode     int
		timeout        time.Duration
		wantErr        bool
		errContains    string
	}{
		{
			name: "Success with valid metrics",
			metricsContent: `
				# HELP http_requests_total The total number of HTTP requests.
				# TYPE http_requests_total counter
				http_requests_total{method="post",code="200"} 1027 1395066363000
			`,
			statusCode: http.StatusOK,
			timeout:    5 * time.Second,
			wantErr:    false,
		},
		{
			name:           "HTTP non-200 status code",
			metricsContent: "",
			statusCode:     http.StatusServiceUnavailable,
			timeout:        5 * time.Second,
			wantErr:        true,
			errContains:    "Bad status code while fetching metrics",
		},
		{
			name: "Context timeout",
			metricsContent: `
				http_requests_total{method="post",code="200"} 1027
			`,
			statusCode:  http.StatusOK,
			timeout:     1 * time.Nanosecond,
			wantErr:     true,
			errContains: "context deadline exceeded",
		},
		{
			name:           "Invalid metric format",
			metricsContent: "invalid_metric_format_here",
			statusCode:     http.StatusOK,
			timeout:        5 * time.Second,
			wantErr:        true,
			errContains:    "Error parsing metric families",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, err := io.WriteString(w, tt.metricsContent)
				if err != nil {
					return
				}
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()
			metrics, err := ParseMetricsURLWithContext(ctx, server.URL)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error but got none")
				}
				if tt.errContains == "context deadline exceeded" {
					if !strings.Contains(err.Error(), tt.errContains) {
						t.Fatalf("expected error to contain %q, got %q", tt.errContains, err.Error())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(metrics) == 0 {
				t.Errorf("expected non-empty metrics map")
			}
		})
	}
}
