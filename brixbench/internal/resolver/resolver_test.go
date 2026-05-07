package resolver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesSourceBlock(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	if err := os.WriteFile(enginePath, []byte("kind: StormService\nmetadata:\n  labels:\n    deployment: disaggregated\n"), 0644); err != nil {
		t.Fatalf("failed to overwrite engine file: %v", err)
	}

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: disaggregated\n" +
			"    provider: aibrix\n" +
			"    version: v0.6.0\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	scenario, err := Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if len(scenario.Tests) != 1 {
		t.Fatalf("expected 1 test, got %d", len(scenario.Tests))
	}
	if scenario.Tests[0].Version != "v0.6.0" {
		t.Fatalf("expected version v0.6.0, got %s", scenario.Tests[0].Version)
	}
	if scenario.Tests[0].ProviderName() != "aibrix" {
		t.Fatalf("expected provider aibrix, got %s", scenario.Tests[0].ProviderName())
	}
	if scenario.Tests[0].Engine.Type != "vllm" {
		t.Fatalf("expected engine type vllm, got %s", scenario.Tests[0].Engine.Type)
	}
	if scenario.Tests[0].Engine.Manifest != enginePath {
		t.Fatalf("expected engine manifest %s, got %s", enginePath, scenario.Tests[0].Engine.Manifest)
	}
	if scenario.Tests[0].BenchmarkKind != "vllm-bench" {
		t.Fatalf("expected benchmark kind vllm-bench, got %s", scenario.Tests[0].BenchmarkKind)
	}
}

func TestResolveNormalizesLegacyVersionField(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: standard\n" +
			"    provider: aibrix\n" +
			"    version: v0.6.0\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	scenario, err := Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if scenario.Tests[0].Version != "v0.6.0" {
		t.Fatalf("expected version v0.6.0, got %s", scenario.Tests[0].Version)
	}
	if scenario.Tests[0].Benchmark != benchmarkPath {
		t.Fatalf("expected benchmark path %s, got %s", benchmarkPath, scenario.Tests[0].Benchmark)
	}
	if scenario.Tests[0].BenchmarkKind != "vllm-bench" {
		t.Fatalf("expected benchmark kind vllm-bench, got %s", scenario.Tests[0].BenchmarkKind)
	}
}

func TestResolveDefaultsVKEDevToFalse(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: standard\n" +
			"    provider: aibrix\n" +
			"    fullstack: false\n" +
			"    version: v0.6.0\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	scenario, err := Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if scenario.Tests[0].VKEDev {
		t.Fatalf("expected vkeDev to default to false")
	}
}

func TestResolveSupportsGatewayImageConfig(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: standard\n" +
			"    provider: aibrix\n" +
			"    fullstack: false\n" +
			"    version: v0.6.0\n" +
			"    gateway:\n" +
			"      image:\n" +
			"        baseImage: example.com/base:v0.6.0\n" +
			"        outputRepository: example.com/output\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	scenario, err := Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if scenario.Tests[0].Gateway.Image.BaseImage != "example.com/base:v0.6.0" {
		t.Fatalf("expected baseImage to be parsed, got %q", scenario.Tests[0].Gateway.Image.BaseImage)
	}
	if scenario.Tests[0].Gateway.Image.OutputRepository != "example.com/output" {
		t.Fatalf("expected outputRepository to be parsed, got %q", scenario.Tests[0].Gateway.Image.OutputRepository)
	}
}

func TestResolveSupportsLocalPath(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: localpath\n" +
			"    provider: aibrix\n" +
			"    fullstack: false\n" +
			"    localPath: ~/aibrix\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	scenario, err := Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if scenario.Tests[0].LocalPath != "~/aibrix" {
		t.Fatalf("expected localPath ~/aibrix, got %s", scenario.Tests[0].LocalPath)
	}
}

func TestResolveRejectsLocalPathWithCommit(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: invalid\n" +
			"    provider: aibrix\n" +
			"    fullstack: false\n" +
			"    localPath: ~/aibrix\n" +
			"    commit: deadbeef\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	if _, err := Resolve(scenarioPath); err == nil {
		t.Fatalf("expected Resolve to reject localPath combined with commit")
	}
}

func TestResolveRejectsVKEDevWithoutFullStack(t *testing.T) {
	tempDir, enginePath, benchmarkPath := createScenarioFixture(t)
	scenarioPath := filepath.Join(tempDir, "scenario.yaml")

	scenarioYAML := []byte(
		"Scenario: sample\n" +
			"Tests:\n" +
			"  - name: invalid\n" +
			"    provider: aibrix\n" +
			"    fullstack: false\n" +
			"    vkeDev: true\n" +
			"    version: v0.6.0\n" +
			"    engine:\n" +
			"      type: vllm\n" +
			"      manifest: " + enginePath + "\n" +
			"    benchmark: " + benchmarkPath + "\n",
	)
	if err := os.WriteFile(scenarioPath, scenarioYAML, 0644); err != nil {
		t.Fatalf("failed to write scenario file: %v", err)
	}

	if _, err := Resolve(scenarioPath); err == nil {
		t.Fatalf("expected Resolve to reject vkeDev without fullstack")
	}
}

func createScenarioFixture(t *testing.T) (string, string, string) {
	t.Helper()

	tempDir := t.TempDir()
	enginePath := filepath.Join(tempDir, "model.yaml")
	benchmarkPath := filepath.Join(tempDir, "benchmark.yaml")

	writeFixtureFile(t, enginePath, "kind: StormService\n")
	writeFixtureFile(t, benchmarkPath, "kind: vllm-bench\nimage: bench\nbaseURL: http://localhost\nmodel: test\ntokenizer: /data/model\n")
	return tempDir, enginePath, benchmarkPath
}

func writeFixtureFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write fixture %s: %v", path, err)
	}
}
