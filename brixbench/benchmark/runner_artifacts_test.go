package benchmark

import (
	"encoding/csv"
	"encoding/json"
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

func generateVLLMBenchFigures(logRoot string) (bool, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return false, fmt.Errorf("failed to resolve benchmark package path")
	}

	packageDir := filepath.Dir(thisFile)
	repoRoot := filepath.Dir(packageDir)
	pythonPath := filepath.Join(repoRoot, ".venv", "bin", "python")
	scriptPath := filepath.Join(packageDir, "plot_summary_vllm_bench.py")

	if _, err := os.Stat(pythonPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat python interpreter: %w", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat plot script: %w", err)
	}

	absLogRoot, err := filepath.Abs(logRoot)
	if err != nil {
		return false, fmt.Errorf("failed to resolve log root: %w", err)
	}

	cmd := exec.Command(pythonPath, scriptPath, absLogRoot)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("plot summary failed: %w\n%s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func generateScenarioFigures(logRoot string, summary scenarioSummary) (bool, error) {
	benchmarkKind, ok := singleScenarioBenchmarkKind(summary)
	if !ok {
		return false, nil
	}

	switch benchmarkKind {
	case "vllm-bench":
		return generateVLLMBenchFigures(logRoot)
	default:
		return false, nil
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

	summaryGeneratedAt := nowInPacificTime()
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
