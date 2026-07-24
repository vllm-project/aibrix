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

package benchmark

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/observability"
	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

type scenarioCaseResult struct {
	TestCase       string         `json:"testcase"`
	BenchmarkKind  string         `json:"benchmarkKind,omitempty"`
	Version        string         `json:"version,omitempty"`
	Commit         string         `json:"commit,omitempty"`
	ResolvedCommit string         `json:"resolvedCommit,omitempty"`
	Status         string         `json:"status"`
	GatewayURL     string         `json:"gatewayURL,omitempty"`
	ResultPath     string         `json:"resultPath,omitempty"`
	Error          string         `json:"error,omitempty"`
	Metrics        map[string]any `json:"metrics,omitempty"`
}

type scenarioSummary struct {
	Scenario   string               `json:"scenario"`
	Generated  string               `json:"generatedAt"`
	CaseCount  int                  `json:"caseCount"`
	Successful int                  `json:"successful"`
	Failed     int                  `json:"failed"`
	Results    []scenarioCaseResult `json:"results"`
}

type scenarioRunMetadata struct {
	RunID       string `json:"runId"`
	Scenario    string `json:"scenario"`
	GeneratedAt string `json:"generatedAt"`
}

func collectScenarioMetricKeys(results []scenarioCaseResult) []string {
	keys := make(map[string]struct{})
	for _, result := range results {
		for key := range result.Metrics {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			keys[key] = struct{}{}
		}
	}

	metricKeys := make([]string, 0, len(keys))
	for key := range keys {
		metricKeys = append(metricKeys, key)
	}
	sort.Strings(metricKeys)
	return metricKeys
}

func scenarioSummaryHeader(metricKeys []string) []string {
	header := []string{
		"testcase",
		"benchmark_kind",
		"status",
		"version",
		"commit",
		"resolved_commit",
		"gateway_url",
		"result_path",
		"error",
	}
	return append(header, metricKeys...)
}

func scenarioSummaryRecord(result scenarioCaseResult, metricKeys []string) []string {
	record := []string{
		result.TestCase,
		result.BenchmarkKind,
		result.Status,
		result.Version,
		result.Commit,
		result.ResolvedCommit,
		result.GatewayURL,
		result.ResultPath,
		result.Error,
	}
	for _, key := range metricKeys {
		record = append(record, stringifyValue(result.Metrics[key]))
	}
	return record
}

func writeScenarioSummary(logRoot string, summary scenarioSummary) error {
	if err := os.MkdirAll(logRoot, 0755); err != nil {
		return fmt.Errorf("failed to create scenario log root %s: %w", logRoot, err)
	}

	jsonPath := filepath.Join(logRoot, "summary.json")
	jsonBytes, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal summary json: %w", err)
	}
	if writeErr := os.WriteFile(jsonPath, jsonBytes, 0644); writeErr != nil {
		return fmt.Errorf("failed to write summary json: %w", writeErr)
	}

	csvPath := filepath.Join(logRoot, "summary.csv")
	csvFile, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("failed to create summary csv: %w", err)
	}
	defer csvFile.Close()

	writer := csv.NewWriter(csvFile)
	metricKeys := collectScenarioMetricKeys(summary.Results)
	header := scenarioSummaryHeader(metricKeys)
	if err := writer.Write(header); err != nil {
		return fmt.Errorf("failed to write summary csv header: %w", err)
	}

	for _, result := range summary.Results {
		record := scenarioSummaryRecord(result, metricKeys)
		if err := writer.Write(record); err != nil {
			return fmt.Errorf("failed to write summary csv row: %w", err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("failed to flush summary csv: %w", err)
	}
	return nil
}

func writeScenarioRunMetadata(logRoot string, metadata scenarioRunMetadata) error {
	if err := os.MkdirAll(logRoot, 0755); err != nil {
		return fmt.Errorf("failed to create scenario log root %s: %w", logRoot, err)
	}

	metadataPath := filepath.Join(logRoot, "metadata.json")
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal run metadata json: %w", err)
	}
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write run metadata json: %w", err)
	}
	return nil
}

func collectScenarioBenchmarkKinds(summary scenarioSummary) []string {
	kinds := make(map[string]struct{})
	for _, result := range summary.Results {
		kind := strings.TrimSpace(result.BenchmarkKind)
		if kind == "" {
			continue
		}
		kinds[kind] = struct{}{}
	}

	values := make([]string, 0, len(kinds))
	for kind := range kinds {
		values = append(values, kind)
	}
	sort.Strings(values)
	return values
}

func singleScenarioBenchmarkKind(summary scenarioSummary) (string, bool) {
	kinds := collectScenarioBenchmarkKinds(summary)
	if len(kinds) != 1 {
		return "", false
	}
	return kinds[0], true
}

func resolveFigurePython(repoRoot string) (string, bool, error) {
	for _, name := range []string{"python3", "python"} {
		pythonPath, err := exec.LookPath(name)
		if err == nil {
			return pythonPath, true, nil
		}
		if !errors.Is(err, exec.ErrNotFound) {
			return "", false, fmt.Errorf("failed to find %s on PATH: %w", name, err)
		}
	}

	venvPythonPath := filepath.Join(repoRoot, ".venv", "bin", "python")
	_, err := os.Stat(venvPythonPath)
	if err == nil {
		return venvPythonPath, true, nil
	}
	if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("failed to stat python interpreter: %w", err)
	}
	return "", false, nil
}

func isMissingPythonDependency(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "modulenotfounderror") ||
		strings.Contains(output, "no module named") ||
		strings.Contains(output, "is required to generate figures")
}

func TestResolveFigurePythonUsesPathBeforeVenv(t *testing.T) {
	repoRoot := t.TempDir()
	binDir := t.TempDir()

	pathPython := filepath.Join(binDir, "python3")
	if err := os.WriteFile(pathPython, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write fake PATH python: %v", err)
	}

	venvBinDir := filepath.Join(repoRoot, ".venv", "bin")
	if err := os.MkdirAll(venvBinDir, 0755); err != nil {
		t.Fatalf("failed to create fake venv bin: %v", err)
	}
	venvPython := filepath.Join(venvBinDir, "python")
	if err := os.WriteFile(venvPython, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write fake venv python: %v", err)
	}

	t.Setenv("PATH", binDir)

	pythonPath, ok, err := resolveFigurePython(repoRoot)
	if err != nil {
		t.Fatalf("resolveFigurePython returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected to resolve python")
	}
	if pythonPath != pathPython {
		t.Fatalf("expected PATH python %s, got %s", pathPython, pythonPath)
	}
}

func TestResolveFigurePythonFallsBackToVenv(t *testing.T) {
	repoRoot := t.TempDir()
	emptyPathDir := t.TempDir()

	venvBinDir := filepath.Join(repoRoot, ".venv", "bin")
	if err := os.MkdirAll(venvBinDir, 0755); err != nil {
		t.Fatalf("failed to create fake venv bin: %v", err)
	}
	venvPython := filepath.Join(venvBinDir, "python")
	if err := os.WriteFile(venvPython, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write fake venv python: %v", err)
	}

	t.Setenv("PATH", emptyPathDir)

	pythonPath, ok, err := resolveFigurePython(repoRoot)
	if err != nil {
		t.Fatalf("resolveFigurePython returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected to resolve python")
	}
	if pythonPath != venvPython {
		t.Fatalf("expected venv python %s, got %s", venvPython, pythonPath)
	}
}

func TestIsMissingPythonDependency(t *testing.T) {
	outputs := []string{
		"ModuleNotFoundError: No module named 'matplotlib'",
		"matplotlib is required to generate figures. Install it with `pip install matplotlib`.",
	}
	for _, output := range outputs {
		if !isMissingPythonDependency(output) {
			t.Fatalf("expected missing dependency output to be detected: %s", output)
		}
	}

	if isMissingPythonDependency("summary.json does not exist") {
		t.Fatal("unexpected missing dependency detection for non-dependency error")
	}
}

func generateVLLMBenchFigures(logRoot string) (bool, string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return false, "", fmt.Errorf("failed to resolve benchmark package path")
	}

	packageDir := filepath.Dir(thisFile)
	repoRoot := filepath.Dir(packageDir)
	scriptPath := filepath.Join(packageDir, "plot_summary_vllm_bench.py")

	pythonPath, ok, err := resolveFigurePython(repoRoot)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "python was not found on PATH or in brixbench/.venv/bin/python", nil
	}
	if _, statErr := os.Stat(scriptPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, "plot_summary_vllm_bench.py was not found", nil
		}
		return false, "", fmt.Errorf("failed to stat plot script: %w", statErr)
	}

	absLogRoot, err := filepath.Abs(logRoot)
	if err != nil {
		return false, "", fmt.Errorf("failed to resolve log root: %w", err)
	}

	cmd := exec.Command(pythonPath, scriptPath, absLogRoot)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputText := strings.TrimSpace(string(output))
		if isMissingPythonDependency(outputText) {
			return false, fmt.Sprintf("plot dependencies are missing: %s", outputText), nil
		}
		return false, "", fmt.Errorf("plot summary failed: %w\n%s", err, outputText)
	}
	return true, "", nil
}

func generateScenarioFigures(logRoot string, summary scenarioSummary) (bool, string, error) {
	benchmarkKind, ok := singleScenarioBenchmarkKind(summary)
	if !ok {
		return false, "benchmark kind was mixed or unavailable", nil
	}

	switch benchmarkKind {
	case "vllm-bench":
		return generateVLLMBenchFigures(logRoot)
	default:
		return false, fmt.Sprintf("benchmark kind %q is not supported for figure generation", benchmarkKind), nil
	}
}

func runScenarioTests(t *testing.T, scenario *resolver.Scenario, scenarioLogRoot string, exporter observability.MetricExporter) []scenarioCaseResult {
	t.Helper()

	resultsByCase := make([]scenarioCaseResult, 0, len(scenario.Tests))
	for _, testCase := range scenario.Tests {
		tc := testCase
		var caseResult scenarioCaseResult
		executed := false

		t.Run(tc.Name, func(t *testing.T) {
			executed = true
			var err error
			caseResult, err = executeScenarioTestCase(t, scenario.Name, scenarioLogRoot, tc, exporter)
			if err != nil {
				t.Fatal(err)
			}
		})

		if executed {
			resultsByCase = append(resultsByCase, caseResult)
		}
	}
	return resultsByCase
}

func buildScenarioSummary(scenarioName string, resultsByCase []scenarioCaseResult) scenarioSummary {
	sort.Slice(resultsByCase, func(i, j int) bool {
		return resultsByCase[i].TestCase < resultsByCase[j].TestCase
	})

	summaryGeneratedAt := nowInUTC()
	summary := scenarioSummary{
		Scenario:  scenarioName,
		Generated: summaryGeneratedAt.Format(time.RFC3339),
		CaseCount: len(resultsByCase),
		Results:   resultsByCase,
	}
	for _, result := range resultsByCase {
		if result.Status == "passed" {
			summary.Successful++
		} else {
			summary.Failed++
		}
	}
	return summary
}

func writeScenarioArtifacts(logRoot string, runID string, summary scenarioSummary) error {
	if err := writeScenarioSummary(logRoot, summary); err != nil {
		return err
	}
	return writeScenarioRunMetadata(logRoot, scenarioRunMetadata{
		RunID:       runID,
		Scenario:    summary.Scenario,
		GeneratedAt: summary.Generated,
	})
}
