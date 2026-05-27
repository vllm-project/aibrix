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
