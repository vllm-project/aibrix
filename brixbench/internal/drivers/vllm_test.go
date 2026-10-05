/*
Copyright 2026 The Aibrix Team.

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

package drivers

import (
	"strings"
	"testing"
)

func TestApplyRuntimeOverridesOverridesNamespaceAndBaseURL(t *testing.T) {
	t.Setenv("BASE_URL", "http://example.test:8080")
	t.Setenv("BENCHMARK_NAMESPACE", "brixbench-case-a")

	config := &vllmBenchConfig{
		Namespace: "brixbench-adhoc",
		VLLMArgs: map[string]interface{}{
			"base-url": "http://old.example:10080",
		},
	}

	applyRuntimeOverrides(config)

	if config.Namespace != "brixbench-case-a" {
		t.Fatalf("expected namespace override, got %q", config.Namespace)
	}
	if got := config.VLLMArgs["base-url"]; got != "http://example.test:8080" {
		t.Fatalf("expected base-url override, got %#v", got)
	}
}

func TestBuildPodManifestUsesExplicitDatasetName(t *testing.T) {
	config := &vllmBenchConfig{
		Image:         "example.test/vllm-bench:latest",
		Namespace:     "brixbench-adhoc",
		PodName:       "vllm-bench-client",
		ModelHostPath: "/models",
		RootHostPath:  "/root",
		VLLMArgs: map[string]interface{}{
			"base-url":     "http://example.test:10080",
			"model":        "qwen3-8b",
			"tokenizer":    "/models/Qwen3-8B",
			"dataset-name": "prefix_repetition",
		},
	}
	config.Artifacts.ResultFilename = "bench_results.json"

	manifest, err := buildPodManifest(config)
	if err != nil {
		t.Fatalf("buildPodManifest() error = %v", err)
	}
	command := string(manifest)

	if !strings.Contains(command, `--dataset-name "prefix_repetition"`) {
		t.Fatalf("expected explicit dataset-name in generated pod manifest:\n%s", command)
	}
	if strings.Contains(command, "--dataset-name random") {
		t.Fatalf("did not expect default random dataset when dataset-name is explicit:\n%s", command)
	}
}

func TestBuildPodManifestRespectsExplicitSeed(t *testing.T) {
	config := &vllmBenchConfig{
		Image:         "example.test/vllm-bench:latest",
		Namespace:     "brixbench-adhoc",
		PodName:       "vllm-bench-client",
		ModelHostPath: "/models",
		RootHostPath:  "/root",
		VLLMArgs: map[string]interface{}{
			"base-url":  "http://example.test:10080",
			"model":     "qwen3-8b",
			"tokenizer": "/models/Qwen3-8B",
			"seed":      111,
		},
	}
	config.Artifacts.ResultFilename = "bench_results.json"

	manifest, err := buildPodManifest(config)
	if err != nil {
		t.Fatalf("buildPodManifest() error = %v", err)
	}
	command := string(manifest)

	if !strings.Contains(command, `--seed "111"`) {
		t.Fatalf("expected explicit seed in generated pod manifest:\n%s", command)
	}
	if strings.Contains(command, "--seed 1") {
		t.Fatalf("did not expect default seed when seed is explicit:\n%s", command)
	}
}

func TestBuildPodManifestExpandsListArgValues(t *testing.T) {
	config := &vllmBenchConfig{
		Image:         "example.test/vllm-bench:latest",
		Namespace:     "brixbench-adhoc",
		PodName:       "vllm-bench-client",
		ModelHostPath: "/models",
		RootHostPath:  "/root",
		VLLMArgs: map[string]interface{}{
			"base-url":  "http://example.test:10080",
			"model":     "qwen3-8b",
			"tokenizer": "/models/Qwen3-8B",
			"goodput":   []interface{}{"ttft:5000", "tpot:250"},
		},
	}
	config.Artifacts.ResultFilename = "bench_results.json"

	manifest, err := buildPodManifest(config)
	if err != nil {
		t.Fatalf("buildPodManifest() error = %v", err)
	}
	command := string(manifest)

	if !strings.Contains(command, `--goodput "ttft:5000" "tpot:250"`) {
		t.Fatalf("expected list values to be expanded in generated pod manifest:\n%s", command)
	}
}

func TestBuildPodManifestRunsWarmupBeforeMeasuredBenchmark(t *testing.T) {
	config := &vllmBenchConfig{
		Image:          "example.test/vllm-bench:latest",
		Namespace:      "brixbench-adhoc",
		PodName:        "vllm-bench-client",
		ModelHostPath:  "/models",
		RootHostPath:   "/root",
		WarmupRequests: 100,
		VLLMArgs: map[string]interface{}{
			"base-url":      "http://example.test:10080",
			"model":         "qwen3-8b",
			"tokenizer":     "/models/Qwen3-8B",
			"num-prompts":   1000,
			"save-detailed": true,
		},
	}
	config.Artifacts.ResultFilename = "bench_results.json"

	manifest, err := buildPodManifest(config)
	if err != nil {
		t.Fatalf("buildPodManifest() error = %v", err)
	}
	command := string(manifest)

	warmupIndex := strings.Index(command, "START warmup")
	measuredIndex := strings.Index(command, "__AIBRIX_BENCH_RESULTS_BEGIN__")
	if warmupIndex < 0 {
		t.Fatalf("expected warmup command in generated pod manifest:\n%s", command)
	}
	if measuredIndex < 0 {
		t.Fatalf("expected measured result marker in generated pod manifest:\n%s", command)
	}
	if warmupIndex > measuredIndex {
		t.Fatalf("expected warmup to run before measured benchmark:\n%s", command)
	}
	if !strings.Contains(command, `--num-prompts "100"`) {
		t.Fatalf("expected warmup num-prompts override in generated pod manifest:\n%s", command)
	}
	if !strings.Contains(command, `--num-prompts "1000"`) {
		t.Fatalf("expected measured num-prompts to remain unchanged in generated pod manifest:\n%s", command)
	}
}
