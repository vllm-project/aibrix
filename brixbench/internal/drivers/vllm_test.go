package drivers

import "testing"

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
