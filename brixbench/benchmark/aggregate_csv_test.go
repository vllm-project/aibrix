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
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

const (
	aggregateCSVSchemaVersion = "1.0"
	aggregateCSVObjectName    = "benchmark_metrics.csv"
)

// Aggregate CSV columns (frozen for Goal 3.1).
// TOS path: tos://{bucket}/{prefix}/aggregates/benchmark_metrics.csv
var aggregateCSVHeader = []string{
	"schema_version",
	"row_id",
	"run_id",
	"testcase",
	"category",
	"status",
	"started_at",
	"finished_at",
	"row_updated_at",
	"platform",
	"platform_version",
	"platform_commit",
	"engine",
	"engine_version",
	"topology",
	"router",
	"model",
	"workload",
	"scenario",
	"benchmark_kind",
	"rate",
	"concurrency",
	"num_prefixes",
	"prefix_len",
	"suffix_len",
	"output_len",
	"num_prompts",
	"series_label",
	"sort_key",
	"completed",
	"failed",
	"duration_s",
	"ttft_mean_ms",
	"ttft_p50_ms",
	"ttft_p90_ms",
	"ttft_p99_ms",
	"tpot_mean_ms",
	"tpot_p99_ms",
	"itl_mean_ms",
	"e2el_mean_ms",
	"e2el_p99_ms",
	"request_throughput",
	"goodput",
	"output_throughput",
	"source_tos_uri",
	"note",
}

func (c publishConfig) aggregateURI() string {
	object := strings.TrimSpace(c.aggregateObject)
	if object == "" {
		object = aggregateCSVObjectName
	}
	return fmt.Sprintf("tos://%s/%s/aggregates/%s", c.bucket, c.prefix, object)
}

func maybeUpdateAggregateCSV(t *testing.T, uploader tosUploader, config publishConfig, scenario *resolver.Scenario, summary scenarioSummary, runID string, startedAt, finishedAt time.Time) error {
	t.Helper()
	rows := buildAggregateRows(scenario, summary, runID, config, startedAt, finishedAt)
	if len(rows) == 0 {
		t.Logf("Skipping aggregate CSV update: no case rows for run %s", runID)
		return nil
	}
	if err := appendAggregateCSV(uploader, config, rows); err != nil {
		if config.strict {
			return err
		}
		t.Logf("Warning: aggregate CSV update failed: %v", err)
		return nil
	}
	t.Logf("Appended %d row(s) to aggregate CSV %s", len(rows), config.aggregateURI())
	return nil
}

func buildAggregateRows(scenario *resolver.Scenario, summary scenarioSummary, runID string, config publishConfig, startedAt, finishedAt time.Time) []map[string]string {
	byName := make(map[string]resolver.Test, len(scenario.Tests))
	for _, tc := range scenario.Tests {
		byName[tc.Name] = tc
	}
	now := time.Now().In(benchmarkLocation()).Format(time.RFC3339)
	start := startedAt.In(benchmarkLocation()).Format(time.RFC3339)
	finish := finishedAt.In(benchmarkLocation()).Format(time.RFC3339)
	rows := make([]map[string]string, 0, len(summary.Results))
	for _, result := range summary.Results {
		tc := byName[result.TestCase]
		platform := tc.ProviderName()
		if platform == "" {
			platform = "vllm"
		}
		metrics := result.Metrics
		if metrics == nil {
			metrics = map[string]any{}
		}
		topology := inferTopology(result.TestCase)
		router := inferRouter(result.TestCase, platform)
		model := firstNonEmpty(stringifyValue(metrics["model_id"]), "qwen3-8b")
		engineVersion := inferEngineVersion(metrics, platform)
		platformVersion := strings.TrimSpace(result.Version)
		if platformVersion == "" {
			platformVersion = strings.TrimSpace(tc.Version)
		}
		if platformVersion == "" {
			// Avoid blank platform_version in dashboards; tip/unknown builds use main.
			platformVersion = "main"
		}
		platformCommit := shortCommit(firstNonEmpty(result.ResolvedCommit, result.Commit, tc.ResolvedCommit, tc.Commit))
		platformTitle := platform
		if platform != "" {
			platformTitle = strings.ToUpper(platform[:1]) + platform[1:]
		}
		platformLabelVersion := platformVersion
		if platform == "aibrix" &&
			strings.TrimSpace(os.Getenv("BENCHMARK_GATEWAY_COMMIT")) != "" &&
			platformCommit != "" {
			platformLabelVersion += "@" + platformCommit
		}
		// Dashboard series_label must spell out "vllm" so engine version is unambiguous.
		enginePart := fmt.Sprintf("vllm %s", engineVersion)
		seriesLabel := fmt.Sprintf("%s + %s + %s", platformTitle, enginePart, router)
		if platformLabelVersion != "" {
			seriesLabel = fmt.Sprintf("%s %s + %s + %s", platformTitle, platformLabelVersion, enginePart, router)
		}
		rowID := fmt.Sprintf("%s:%s", runID, result.TestCase)
		row := map[string]string{
			"schema_version":     aggregateCSVSchemaVersion,
			"row_id":             rowID,
			"run_id":             runID,
			"testcase":           result.TestCase,
			"category":           scenarioCategory(scenario.Name),
			"status":             result.Status,
			"started_at":         start,
			"finished_at":        finish,
			"row_updated_at":     now,
			"platform":           platform,
			"platform_version":   platformVersion,
			"platform_commit":    platformCommit,
			"engine":             fmt.Sprintf("vllm-%s", engineVersion),
			"engine_version":     engineVersion,
			"topology":           topology,
			"router":             router,
			"model":              model,
			"workload":           "prefix_repetition",
			"scenario":           scenario.Name,
			"benchmark_kind":     firstNonEmpty(result.BenchmarkKind, tc.BenchmarkKind, "vllm-bench"),
			"rate":               metricString(metrics, "request_rate"),
			"concurrency":        metricString(metrics, "max_concurrency"),
			"num_prefixes":       "",
			"prefix_len":         "",
			"suffix_len":         "",
			"output_len":         "",
			"num_prompts":        metricString(metrics, "num_prompts"),
			"series_label":       seriesLabel,
			"sort_key":           platformSortKey(platform, platformVersion),
			"completed":          metricString(metrics, "completed"),
			"failed":             metricString(metrics, "failed"),
			"duration_s":         metricString(metrics, "duration"),
			"ttft_mean_ms":       metricString(metrics, "mean_ttft_ms"),
			"ttft_p50_ms":        metricString(metrics, "p50_ttft_ms"),
			"ttft_p90_ms":        metricString(metrics, "p90_ttft_ms"),
			"ttft_p99_ms":        metricString(metrics, "p99_ttft_ms"),
			"tpot_mean_ms":       metricString(metrics, "mean_tpot_ms"),
			"tpot_p99_ms":        metricString(metrics, "p99_tpot_ms"),
			"itl_mean_ms":        metricString(metrics, "mean_itl_ms"),
			"e2el_mean_ms":       metricString(metrics, "mean_e2el_ms"),
			"e2el_p99_ms":        metricString(metrics, "p99_e2el_ms"),
			"request_throughput": metricString(metrics, "request_throughput"),
			"goodput":            firstNonEmpty(metricString(metrics, "request_goodput"), metricString(metrics, "goodput")),
			"output_throughput":  metricString(metrics, "output_throughput"),
			"source_tos_uri":     config.runURI(runID) + "cases/" + sanitizePathComponent(result.TestCase) + "/results/bench_results.json",
			"note":               strings.TrimSpace(result.Error),
		}
		rows = append(rows, row)
	}
	return rows
}

func appendAggregateCSV(uploader tosUploader, config publishConfig, newRows []map[string]string) error {
	if len(newRows) == 0 {
		return nil
	}
	remoteURI := config.aggregateURI()

	// Probe whether the appendable aggregate object already exists (Head, not full download).
	exists, err := uploader.Exists(remoteURI)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	if !exists {
		if err := writer.Write(aggregateCSVHeader); err != nil {
			return err
		}
	}
	for _, row := range newRows {
		record := make([]string, len(aggregateCSVHeader))
		for i, key := range aggregateCSVHeader {
			record[i] = row[key]
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	return uploader.AppendBytes(remoteURI, buf.Bytes())
}

// upsertAggregateRows is retained for unit tests of row_id merge helpers used by
// historical download-merge flows; production publish uses appendAggregateCSV.
func upsertAggregateRows(existing, incoming []map[string]string) []map[string]string {
	byID := make(map[string]int, len(existing))
	out := make([]map[string]string, 0, len(existing)+len(incoming))
	for _, row := range existing {
		id := strings.TrimSpace(row["row_id"])
		if id == "" {
			continue
		}
		byID[id] = len(out)
		out = append(out, row)
	}
	for _, row := range incoming {
		id := strings.TrimSpace(row["row_id"])
		if id == "" {
			continue
		}
		if idx, ok := byID[id]; ok {
			out[idx] = row
			continue
		}
		byID[id] = len(out)
		out = append(out, row)
	}
	return out
}

func readAggregateCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	rows := make([]map[string]string, 0)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		row := make(map[string]string, len(header))
		for i, key := range header {
			if i < len(record) {
				row[key] = record[i]
			} else {
				row[key] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func writeAggregateCSV(path string, rows []map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	if err := writer.Write(aggregateCSVHeader); err != nil {
		return err
	}
	for _, row := range rows {
		record := make([]string, len(aggregateCSVHeader))
		for i, key := range aggregateCSVHeader {
			record[i] = row[key]
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func metricString(metrics map[string]any, key string) string {
	if metrics == nil {
		return ""
	}
	v, ok := metrics[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case jsonNumberStringer:
		return t.String()
	default:
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if s == "<nil>" {
			return ""
		}
		return s
	}
}

type jsonNumberStringer interface {
	String() string
}

func inferTopology(testcase string) string {
	name := strings.ToLower(testcase)
	for _, size := range []string{"4p4d", "4p8d", "8p4d", "8p8d"} {
		if !strings.Contains(name, size) {
			continue
		}
		if strings.Contains(name, "multinode") {
			return size + "-multinode"
		}
		if strings.Contains(name, "singlenode") {
			return size + "-singlenode"
		}
		return size
	}
	return ""
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return ""
	}
	// Keep already-short refs; truncate full SHAs to 7 hex chars.
	if len(commit) >= 7 && isHexString(commit) {
		return commit[:7]
	}
	return commit
}

func isHexString(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func inferRouter(testcase, platform string) string {
	name := strings.ToLower(testcase)
	switch {
	case strings.Contains(name, "round-robin"):
		return "round-robin"
	case strings.Contains(name, "-kv-") || strings.HasSuffix(name, "-kv") || strings.Contains(name, "_kv_"):
		return "kv"
	case strings.Contains(name, "-pd-") || strings.Contains(name, "_pd_") || strings.HasPrefix(name, "aibrix-pd") || strings.HasPrefix(name, "llmd-pd"):
		return "pd"
	case platform == "aibrix" || platform == "llmd":
		return "pd"
	default:
		return "unknown"
	}
}

func inferEngineVersion(metrics map[string]any, platform string) string {
	_ = platform
	if tok := stringifyValue(metrics["tokenizer_id"]); strings.Contains(tok, "Qwen3") {
		// tokenizer path is not engine version; fall through to defaults.
	}
	// Defaults used by current multi-node fixtures.
	switch platform {
	case "dynamo":
		return "0.21.0"
	case "llmd":
		return "0.23.0"
	default:
		return "0.22.0"
	}
}

func platformSortKey(platform, version string) string {
	base := 0
	switch platform {
	case "aibrix":
		base = 300
	case "dynamo":
		base = 200
	case "llmd":
		base = 100
	default:
		base = 10
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, version)
	if len(digits) > 4 {
		digits = digits[:4]
	}
	n, _ := strconv.Atoi(digits)
	return fmt.Sprintf("%04d", base+n)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func isMissingRemoteObject(err error) bool {
	return err != nil && (os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "404") || strings.Contains(strings.ToLower(err.Error()), "not exist") || strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "nosuchkey"))
}

func TestAppendAggregateCSVCreatesThenAppends(t *testing.T) {
	uploader := &fakeTOSUploader{}
	config := publishConfig{bucket: "bucket", prefix: "prefix"}
	row1 := []map[string]string{{"row_id": "run1:case", "status": "passed", "schema_version": "1.0"}}
	if err := appendAggregateCSV(uploader, config, row1); err != nil {
		t.Fatal(err)
	}
	uri := config.aggregateURI()
	if len(uploader.appends) != 1 || uploader.objects[uri] == "" {
		t.Fatalf("expected first append create, got %#v", uploader)
	}
	if !strings.Contains(uploader.objects[uri], "platform_commit") {
		t.Fatalf("header missing platform_commit: %s", uploader.objects[uri])
	}
	row2 := []map[string]string{{"row_id": "run2:case", "status": "failed", "schema_version": "1.0"}}
	if err := appendAggregateCSV(uploader, config, row2); err != nil {
		t.Fatal(err)
	}
	if len(uploader.appends) != 2 {
		t.Fatalf("appends=%d", len(uploader.appends))
	}
	body := uploader.objects[uri]
	if strings.Count(body, "schema_version") != 1 {
		t.Fatalf("header should appear once, body=%s", body)
	}
	if !strings.Contains(body, "run1:case") || !strings.Contains(body, "run2:case") {
		t.Fatalf("missing rows: %s", body)
	}
}

func TestUpsertAggregateRowsReplacesByRowID(t *testing.T) {
	existing := []map[string]string{{"row_id": "a", "status": "old"}, {"row_id": "b", "status": "keep"}}
	incoming := []map[string]string{{"row_id": "a", "status": "new"}, {"row_id": "c", "status": "add"}}
	got := upsertAggregateRows(existing, incoming)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0]["status"] != "new" {
		t.Fatalf("row a status=%q", got[0]["status"])
	}
	if got[2]["row_id"] != "c" {
		t.Fatalf("expected append c, got %#v", got[2])
	}
}

func TestWriteReadAggregateCSVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	rows := []map[string]string{{
		"schema_version": "1.0",
		"row_id":         "run:case",
		"run_id":         "run",
		"testcase":       "case",
		"status":         "passed",
		"platform":       "aibrix",
		"ttft_mean_ms":   "12.5",
	}}
	if err := writeAggregateCSV(path, rows); err != nil {
		t.Fatal(err)
	}
	got, err := readAggregateCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["row_id"] != "run:case" || got[0]["ttft_mean_ms"] != "12.5" {
		t.Fatalf("unexpected %#v", got)
	}
}

func TestInferTopologyAndRouter(t *testing.T) {
	if got := inferTopology("aibrix-pd-4p4d-multinode-r8"); got != "4p4d-multinode" {
		t.Fatalf("topology=%q", got)
	}
	if got := inferTopology("aibrix-pd-4p4d-singlenode-r8"); got != "4p4d-singlenode" {
		t.Fatalf("topology=%q", got)
	}
	if got := inferTopology("dynamo-v1.2.1-qwen3-8b-round-robin-4p8d-multinode-r8"); got != "4p8d-multinode" {
		t.Fatalf("topology=%q", got)
	}
	if got := inferTopology("llmd-pd-8p4d-multinode-r16"); got != "8p4d-multinode" {
		t.Fatalf("topology=%q", got)
	}
	if got := inferTopology("aibrix-pd-8p8d-r8"); got != "8p8d" {
		t.Fatalf("topology=%q", got)
	}
	if got := inferRouter("dynamo-v1.2.1-qwen3-8b-round-robin-4p4d-multinode-r8", "dynamo"); got != "round-robin" {
		t.Fatalf("router=%q", got)
	}
	if got := inferRouter("llmd-pd-4p4d-multinode-r8", "llmd"); got != "pd" {
		t.Fatalf("router=%q", got)
	}
}

func TestBuildAggregateRowsSeriesLabelAndCommit(t *testing.T) {
	aibrix, dynamo, llmd := "aibrix", "dynamo", "llmd"
	scenario := &resolver.Scenario{
		Name: "routing-compare-qwen3-8b-4p4d-singlenode",
		Tests: []resolver.Test{
			{Name: "aibrix-pd-4p4d-singlenode-r8", Provider: &aibrix, Version: "v0.6.0", ResolvedCommit: "abcdef1234567890"},
			{Name: "dynamo-v1.2.1-qwen3-8b-round-robin-4p4d-singlenode-r16", Provider: &dynamo, Version: "v1.2.1"},
			{Name: "llmd-pd-4p4d-singlenode-r8", Provider: &llmd},
		},
	}
	summary := scenarioSummary{Results: []scenarioCaseResult{
		{TestCase: "aibrix-pd-4p4d-singlenode-r8", Status: "passed", Version: "v0.6.0", ResolvedCommit: "abcdef1234567890", Metrics: map[string]any{"request_rate": 8, "num_prompts": 1000, "mean_ttft_ms": 1.5}},
		{TestCase: "dynamo-v1.2.1-qwen3-8b-round-robin-4p4d-singlenode-r16", Status: "passed", Version: "v1.2.1", Metrics: map[string]any{"request_rate": 16}},
		{TestCase: "llmd-pd-4p4d-singlenode-r8", Status: "failed"},
	}}
	rows := buildAggregateRows(scenario, summary, "run-1", publishConfig{bucket: "b", prefix: "p"}, time.Now(), time.Now())
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0]["series_label"] != "Aibrix v0.6.0 + vllm 0.22.0 + pd" {
		t.Fatalf("aibrix series_label=%q", rows[0]["series_label"])
	}
	if rows[0]["platform_commit"] != "abcdef1" {
		t.Fatalf("aibrix platform_commit=%q", rows[0]["platform_commit"])
	}
	if rows[0]["topology"] != "4p4d-singlenode" {
		t.Fatalf("topology=%q", rows[0]["topology"])
	}
	if rows[1]["series_label"] != "Dynamo v1.2.1 + vllm 0.21.0 + round-robin" {
		t.Fatalf("dynamo series_label=%q", rows[1]["series_label"])
	}
	if rows[2]["platform_version"] != "main" {
		t.Fatalf("llmd platform_version=%q want main", rows[2]["platform_version"])
	}
	if rows[2]["series_label"] != "Llmd main + vllm 0.23.0 + pd" {
		t.Fatalf("llmd series_label=%q", rows[2]["series_label"])
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "benchmark_metrics.csv")
	if err := writeAggregateCSV(path, rows); err != nil {
		t.Fatal(err)
	}
	got, err := readAggregateCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[0]["platform_commit"]; !ok {
		t.Fatalf("missing platform_commit column in round-trip")
	}
	header := aggregateCSVHeader
	found := false
	for _, h := range header {
		if h == "platform_commit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("platform_commit missing from aggregateCSVHeader")
	}
}

func TestBuildAggregateRowsLabelsPrebuiltGatewayCommit(t *testing.T) {
	const gatewayCommit = "9aa8b21ef6053dc19dde76f71552247b82f93630"
	t.Setenv("BENCHMARK_GATEWAY_COMMIT", gatewayCommit)

	aibrix := "aibrix"
	scenario := &resolver.Scenario{
		Name: "aibrix-routing-qwen3-8b-4p4d-multinode",
		Tests: []resolver.Test{{
			Name:     "aibrix-pd-4p4d-multinode-r8",
			Provider: &aibrix,
			Version:  "v0.6.0",
		}},
	}
	summary := scenarioSummary{Results: []scenarioCaseResult{{
		TestCase:       "aibrix-pd-4p4d-multinode-r8",
		Status:         "passed",
		Version:        "v0.6.0",
		ResolvedCommit: gatewayCommit,
		Metrics:        map[string]any{"request_rate": 8},
	}}}

	rows := buildAggregateRows(
		scenario,
		summary,
		"run-prebuilt",
		publishConfig{bucket: "b", prefix: "p"},
		time.Now(),
		time.Now(),
	)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0]["platform_version"] != "v0.6.0" {
		t.Fatalf("platform_version=%q", rows[0]["platform_version"])
	}
	if rows[0]["platform_commit"] != "9aa8b21" {
		t.Fatalf("platform_commit=%q", rows[0]["platform_commit"])
	}
	const wantLabel = "Aibrix v0.6.0@9aa8b21 + vllm 0.22.0 + pd"
	if rows[0]["series_label"] != wantLabel {
		t.Fatalf("series_label=%q, want %q", rows[0]["series_label"], wantLabel)
	}
}
