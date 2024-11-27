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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseHistogramWithLabels(t *testing.T) {
	body := []byte(`
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0
# HELP vllm:gpu_cache_usage_perc GPU KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0
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
