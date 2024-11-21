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

package cache

import (
	"testing"
)

func TestParseHistogramFromBody(t *testing.T) {
	body := []byte(`
vllm:request_generation_tokens_sum{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 31.0
vllm:request_generation_tokens_bucket{le="1.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0
vllm:request_generation_tokens_bucket{le="2.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 11.0
vllm:request_generation_tokens_bucket{le="5.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 7.0
vllm:request_generation_tokens_bucket{le="10.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 10.0
vllm:request_generation_tokens_bucket{le="20.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 2.0
vllm:request_generation_tokens_bucket{le="50.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 2.0
vllm:request_generation_tokens_count{model_name="Qwen/Qwen2.5-1.5B-Instruct"} 31.0
`)

	expectedSum := 31.0
	expectedCount := 31.0
	expectedBuckets := map[string]float64{
		"1.0":  0.0,
		"2.0":  11.0,
		"5.0":  7.0,
		"10.0": 10.0,
		"20.0": 2.0,
		"50.0": 2.0,
	}

	histogram, err := parseHistogramFromBody(body, "request_generation_tokens")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if histogram.Sum != expectedSum {
		t.Errorf("Expected sum: %v, got: %v", expectedSum, histogram.Sum)
	}

	if histogram.Count != expectedCount {
		t.Errorf("Expected count: %v, got: %v", expectedCount, histogram.Count)
	}

	for le, expectedValue := range expectedBuckets {
		value, ok := histogram.Buckets[le]
		if !ok {
			t.Errorf("Expected bucket for le=%s not found", le)
		} else if value != expectedValue {
			t.Errorf("Expected value for le=%s: %v, got: %v", le, expectedValue, value)
		}
	}
}

func TestExtractBucketBoundary(t *testing.T) {
	tests := []struct {
		line       string
		expectedLe string
	}{
		{
			line:       `vllm:request_generation_tokens_bucket{le="1.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 0.0`,
			expectedLe: "1.0",
		},
		{
			line:       `vllm:request_generation_tokens_bucket{le="20.0",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 2.0`,
			expectedLe: "20.0",
		},
		{
			line:       `vllm:request_generation_tokens_bucket{le="+Inf",model_name="Qwen/Qwen2.5-1.5B-Instruct"} 2.0`,
			expectedLe: "+Inf",
		},
	}

	for _, test := range tests {
		le := extractBucketBoundary(test.line)
		if le != test.expectedLe {
			t.Errorf("Expected le=%s, got: %s", test.expectedLe, le)
		}
	}
}
