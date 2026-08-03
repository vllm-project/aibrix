/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

const (
	defaultTOSBucket = "aibrix-artifact-testing"
	defaultTOSPrefix = "benchmarks/brixbench-results"
	oversizedFile    = 50 * 1024 * 1024
)

type publishConfig struct {
	bucket string
	prefix string
	tier   string
	strict bool
}

type publishMapping struct {
	source string
	target string
}

var newTOSUploader = func() (tosUploader, error) { return newGoTOSUploader() }

type fakeTOSUploader struct {
	uploadErr error
	appendErr error
	uploads   []string
	appends   []string
	deletes   []string
	objects   map[string]string
}

func (u *fakeTOSUploader) Upload(localPath, remoteURI string) error {
	u.uploads = append(u.uploads, remoteURI)
	if u.objects == nil {
		u.objects = map[string]string{}
	}
	body, _ := os.ReadFile(localPath)
	u.objects[remoteURI] = string(body)
	return u.uploadErr
}

func (u *fakeTOSUploader) Download(remoteURI, localPath string) error {
	if u.objects == nil {
		return os.ErrNotExist
	}
	body, ok := u.objects[remoteURI]
	if !ok {
		return os.ErrNotExist
	}
	return os.WriteFile(localPath, []byte(body), 0o644)
}

func (u *fakeTOSUploader) Delete(remoteURI string) error {
	u.deletes = append(u.deletes, remoteURI)
	if u.objects != nil {
		delete(u.objects, remoteURI)
	}
	return nil
}

func (u *fakeTOSUploader) AppendBytes(remoteURI string, data []byte) error {
	u.appends = append(u.appends, remoteURI)
	if u.appendErr != nil {
		return u.appendErr
	}
	if u.objects == nil {
		u.objects = map[string]string{}
	}
	u.objects[remoteURI] += string(data)
	return nil
}

type publishReceipt struct {
	SchemaVersion string   `json:"schema_version"`
	RunID         string   `json:"run_id"`
	Status        string   `json:"status"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	Attempts      int      `json:"attempts"`
	Strict        bool     `json:"strict"`
	TOSURI        string   `json:"tos_uri"`
	FilesUploaded int      `json:"files_uploaded"`
	FilesFailed   int      `json:"files_failed"`
	Warnings      []string `json:"warnings"`
	Errors        []string `json:"errors"`
	IndexUpdated  bool     `json:"index_updated"`
}

func configuredPublisher() publishConfig {
	bucket := strings.TrimSpace(os.Getenv("BENCHMARK_TOS_BUCKET"))
	if bucket == "" {
		bucket = defaultTOSBucket
	}
	prefix := strings.Trim(strings.TrimSpace(os.Getenv("BENCHMARK_TOS_PREFIX")), "/")
	if prefix == "" {
		prefix = defaultTOSPrefix
	}
	tier := strings.ToLower(strings.TrimSpace(os.Getenv("BENCHMARK_PUBLISH_TIER")))
	if tier != "minimal" && tier != "full" {
		tier = "standard"
	}
	return publishConfig{bucket: bucket, prefix: prefix, tier: tier, strict: publishStrictEnabled()}
}

func (c publishConfig) runURI(runID string) string {
	return fmt.Sprintf("tos://%s/%s/runs/%s/", c.bucket, c.prefix, runID)
}

func stagePublishInputs(caseLogDir string, testCase resolver.Test) error {
	manifestDir := filepath.Join(caseLogDir, ".publish-staging", "manifests")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
	}
	inputs := []struct {
		source string
		target string
	}{
		{testCase.Engine.Manifest, "engine-manifest.yaml"},
		{testCase.Benchmark, "benchmark-workload.yaml"},
	}
	if testCase.ProviderName() == "dynamo" {
		inputs = append(inputs, struct {
			source string
			target string
		}{testCase.Platform.ValuesFile, "platform-values.yaml"})
	}
	for _, input := range inputs {
		if strings.TrimSpace(input.source) == "" {
			continue
		}
		if err := copySanitizedYAML(input.source, filepath.Join(manifestDir, input.target)); err != nil {
			return err
		}
	}
	if testCase.ProviderName() == "aibrix" {
		for _, source := range testCase.Gateway.Resources {
			if err := copySanitizedYAML(source, filepath.Join(manifestDir, "gateway-overrides", filepath.Base(source))); err != nil {
				return err
			}
		}
	}
	return nil
}

func copySanitizedYAML(source, target string) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.WriteFile(target, redactSensitiveLines(body), 0644)
}

func redactSensitiveLines(body []byte) []byte {
	lines := bytes.Split(body, []byte("\n"))
	sensitiveIndent := -1
	for i, line := range lines {
		lower := strings.ToLower(string(line))
		trimmed := strings.TrimSpace(string(line))
		indent := len(line) - len(bytes.TrimLeft(line, " \t"))
		if strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "stringData:") {
			sensitiveIndent = indent
			continue
		}
		if sensitiveIndent >= 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") && indent <= sensitiveIndent {
			sensitiveIndent = -1
		}
		if sensitiveIndent >= 0 && indent > sensitiveIndent && bytes.Contains(line, []byte(":")) {
			if colon := bytes.IndexByte(line, ':'); colon >= 0 {
				lines[i] = append(append([]byte(nil), line[:colon+1]...), []byte(" REDACTED")...)
			}
			continue
		}
		if strings.Contains(lower, "password:") || strings.Contains(lower, "token:") || strings.Contains(lower, "secret:") || strings.Contains(lower, "accesskey:") || strings.Contains(lower, "secretkey:") {
			if colon := bytes.IndexByte(line, ':'); colon >= 0 {
				lines[i] = append(append([]byte(nil), line[:colon+1]...), []byte(" REDACTED")...)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func corePublishMappings(logRoot string, tier string) []publishMapping {
	mappings := []publishMapping{
		{filepath.Join(logRoot, "metadata.json"), "metadata.json"},
		{filepath.Join(logRoot, "summary.json"), "summary/summary.json"},
		{filepath.Join(logRoot, "summary.csv"), "summary/summary.csv"},
	}
	if tier != "minimal" {
		mappings = append(mappings, publishMapping{filepath.Join(logRoot, "brixbench.log"), "logs/brixbench.log"})
	}
	if tier == "full" {
		mappings = append(mappings, publishMapping{filepath.Join(logRoot, "figures"), "figures"})
	}
	return mappings
}

func casePublishMappings(logRoot string, testCase resolver.Test, tier string) []publishMapping {
	caseRoot := caseLogRoot(logRoot, testCase.Name)
	mappings := []publishMapping{{filepath.Join(caseRoot, "bench_results.json"), filepath.Join("cases", sanitizePathComponent(testCase.Name), "results", "bench_results.json")}}
	if tier == "minimal" {
		return mappings
	}
	targetRoot := filepath.Join("cases", sanitizePathComponent(testCase.Name))
	mappings = append(mappings,
		publishMapping{filepath.Join(caseRoot, "vllm-bench-pod.yaml"), filepath.Join(targetRoot, "manifests", "vllm-bench-pod.yaml")},
		publishMapping{filepath.Join(caseRoot, "vllm-bench-client.log"), filepath.Join(targetRoot, "logs", "vllm-bench-client.log")},
		publishMapping{filepath.Join(caseRoot, "engine-logs"), filepath.Join(targetRoot, "logs", "engine")},
		publishMapping{filepath.Join(caseRoot, "resource-yaml"), filepath.Join(targetRoot, "manifests", "resource-yaml")},
		publishMapping{filepath.Join(caseRoot, ".publish-staging", "manifests", "engine-manifest.yaml"), filepath.Join(targetRoot, "manifests", "engine-manifest.yaml")},
		publishMapping{filepath.Join(caseRoot, ".publish-staging", "manifests", "benchmark-workload.yaml"), filepath.Join(targetRoot, "manifests", "benchmark-workload.yaml")},
	)
	switch testCase.ProviderName() {
	case "aibrix":
		mappings = append(mappings,
			publishMapping{filepath.Join(caseRoot, "gateway-logs"), filepath.Join(targetRoot, "logs", "gateway")},
			publishMapping{filepath.Join(caseRoot, ".publish-staging", "manifests", "gateway-overrides"), filepath.Join(targetRoot, "manifests", "gateway-overrides")},
		)
	case "dynamo":
		mappings = append(mappings,
			publishMapping{filepath.Join(caseRoot, "dynamo-runtime"), filepath.Join(targetRoot, "manifests", "dynamo-runtime")},
			publishMapping{filepath.Join(caseRoot, ".publish-staging", "manifests", "platform-values.yaml"), filepath.Join(targetRoot, "manifests", "platform-values.yaml")},
		)
	}
	if tier == "full" {
		mappings = append(mappings, publishMapping{filepath.Join(caseRoot, "commands"), filepath.Join(targetRoot, "logs", "commands")})
		if testCase.ProviderName() == "aibrix" {
			mappings = append(mappings, publishMapping{filepath.Join(caseRoot, "cluster-snapshots"), filepath.Join(targetRoot, "debug", "cluster-snapshots")})
		}
		if testCase.ProviderName() == "dynamo" {
			mappings = append(mappings, publishMapping{filepath.Join(caseRoot, "dynamo-artifacts"), filepath.Join(targetRoot, "debug", "dynamo-artifacts")})
		}
	}
	return mappings
}

func existingPublishFiles(mappings []publishMapping) ([]publishMapping, error) {
	var files []publishMapping
	for _, mapping := range mappings {
		info, err := os.Stat(mapping.source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if info.Size() == 0 {
				continue
			}
			files = append(files, mapping)
			continue
		}
		err = filepath.WalkDir(mapping.source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() == 0 {
				return nil
			}
			relative, err := filepath.Rel(mapping.source, path)
			if err != nil {
				return err
			}
			files = append(files, publishMapping{path, filepath.Join(mapping.target, relative)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].target < files[j].target })
	return files, nil
}

func maybePublishScenarioArtifacts(t *testing.T, scenario *resolver.Scenario, scenarioPath, logRoot, runID string, startedAt time.Time, summary scenarioSummary) error {
	t.Helper()
	if !publishEnabled() {
		return nil
	}
	config := configuredPublisher()
	finishedAt := time.Now().In(benchmarkLocation())
	if err := patchScenarioMetadata(logRoot, startedAt, finishedAt); err != nil {
		return err
	}
	stagedScenarioPath := filepath.Join(logRoot, ".publish-staging", "inputs", "scenario.yaml")
	if err := copySanitizedYAML(scenarioPath, stagedScenarioPath); err != nil {
		return fmt.Errorf("failed to stage scenario input: %w", err)
	}
	manifest, status, versions := buildPublishDocuments(scenario, summary, runID, config, startedAt, finishedAt)
	extraFiles := []publishMapping{
		{filepath.Join(logRoot, "manifest.json"), "manifest.json"},
		{filepath.Join(logRoot, "status.json"), "status.json"},
		{filepath.Join(logRoot, "versions.json"), "versions.json"},
		{stagedScenarioPath, "inputs/scenario.yaml"},
	}
	for _, document := range []struct {
		path string
		body any
	}{{filepath.Join(logRoot, "manifest.json"), manifest}, {filepath.Join(logRoot, "status.json"), status}, {filepath.Join(logRoot, "versions.json"), versions}} {
		if err := writeJSON(document.path, document.body); err != nil {
			return err
		}
	}
	mappings := append(corePublishMappings(logRoot, config.tier), extraFiles...)
	for _, testCase := range scenario.Tests {
		mappings = append(mappings, casePublishMappings(logRoot, testCase, config.tier)...)
	}
	files, err := existingPublishFiles(mappings)
	if err != nil {
		return err
	}
	return publishFilesWithAggregate(t, config, logRoot, runID, manifest, files, scenario, summary, startedAt, finishedAt)
}

func patchScenarioMetadata(logRoot string, startedAt, finishedAt time.Time) error {
	metadata := scenarioRunMetadata{}
	body, err := os.ReadFile(filepath.Join(logRoot, "metadata.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return err
	}
	metadata.StartedAt = startedAt.In(benchmarkLocation()).Format(time.RFC3339)
	metadata.FinishedAt = finishedAt.Format(time.RFC3339)
	return writeJSON(filepath.Join(logRoot, "metadata.json"), metadata)
}

func buildPublishDocuments(scenario *resolver.Scenario, summary scenarioSummary, runID string, config publishConfig, startedAt, finishedAt time.Time) (map[string]any, map[string]any, map[string]any) {
	cases := make([]string, 0, len(summary.Results))
	statusCases := make([]map[string]string, 0, len(summary.Results))
	versionCases := make([]map[string]any, 0, len(scenario.Tests))
	for _, result := range summary.Results {
		cases = append(cases, result.TestCase)
		statusCases = append(statusCases, map[string]string{"testcase": result.TestCase, "status": result.Status, "error": result.Error})
	}
	platform, version := "vllm", ""
	for _, testCase := range scenario.Tests {
		provider := testCase.ProviderName()
		if provider == "" {
			provider = "vllm"
		}
		if version == "" {
			platform, version = provider, testCase.Version
		}
		versionCases = append(versionCases, map[string]any{
			"testcase": testCase.Name, "provider": provider, "version": testCase.Version,
			"commit": nullableString(testCase.Commit), "resolved_commit": nullableString(testCase.ResolvedCommit),
			"engine_type": testCase.Engine.Type, "benchmark_kind": testCase.BenchmarkKind,
			"benchmark_namespace": benchmarkNamespaceForTestCase(testCase),
		})
	}
	runURI := config.runURI(runID)
	manifest := map[string]any{
		"schema_version": "1.0", "run_id": runID, "publish": true, "publish_tier": config.tier, "publish_strict": config.strict,
		"source": "brixbench", "category": scenarioCategory(scenario.Name), "platform": platform, "version": version, "scenario": scenario.Name,
		"timezone": benchmarkLocation().String(), "started_at": startedAt.In(benchmarkLocation()).Format(time.RFC3339),
		"finished_at": finishedAt.Format(time.RFC3339), "status": statusForSummary(summary), "cases": cases, "tos_uri": runURI,
	}
	status := map[string]any{"schema_version": "1.0", "run_id": runID, "status": statusForSummary(summary), "case_count": summary.CaseCount, "successful": summary.Successful, "failed": summary.Failed, "cases": statusCases}
	versions := map[string]any{"schema_version": "1.0", "run_id": runID, "brixbench": map[string]string{"module": "github.com/vllm-project/aibrix/brixbench"}, "cases": versionCases}
	return manifest, status, versions
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scenarioCategory(name string) string {
	name = strings.ToLower(name)
	switch {
	case strings.Contains(name, "routing"):
		return "routing"
	case strings.Contains(name, "kvcache"):
		return "kvcache"
	case strings.Contains(name, "hello"), strings.Contains(name, "smoke"):
		return "smoke"
	default:
		return "other"
	}
}

func statusForSummary(summary scenarioSummary) string {
	if summary.Failed == 0 {
		return "passed"
	}
	return "failed"
}

func publishFiles(t *testing.T, config publishConfig, logRoot, runID string, manifest map[string]any, files []publishMapping) error {
	startedAt := time.Now().In(benchmarkLocation())
	receipt := publishReceipt{SchemaVersion: "1.0", RunID: runID, StartedAt: startedAt.Format(time.RFC3339), Strict: config.strict, TOSURI: config.runURI(runID)}
	uploader, err := newTOSUploader()
	if err != nil {
		return err
	}
	for _, file := range files {
		if info, err := os.Stat(file.source); err == nil && info.Size() > oversizedFile {
			receipt.Warnings = append(receipt.Warnings, fmt.Sprintf("%s exceeds 50MB", file.target))
		}
		if err := uploadWithRetry(uploader, file.source, config.runURI(runID)+filepath.ToSlash(file.target), config.strict, &receipt); err != nil {
			receipt.FilesFailed++
			receipt.Errors = append(receipt.Errors, err.Error())
		} else {
			receipt.FilesUploaded++
		}
	}
	if receipt.FilesFailed > 0 {
		receipt.Status = "partial"
	} else {
		receipt.Status = "success"
	}
	receipt.FinishedAt = time.Now().In(benchmarkLocation()).Format(time.RFC3339)
	receiptPath := filepath.Join(logRoot, "tos_upload.json")
	if err := writeJSON(receiptPath, receipt); err != nil {
		return err
	}
	if err := uploadWithRetry(uploader, receiptPath, config.runURI(runID)+"tos_upload.json", config.strict, &receipt); err != nil {
		receipt.Status = "failed"
		receipt.Errors = append(receipt.Errors, err.Error())
	} else {
		receipt.FilesUploaded++
	}
	if err := updateTOSIndex(uploader, config, runID, manifest, receipt); err != nil {
		receipt.Status = "failed"
		receipt.Errors = append(receipt.Errors, err.Error())
	} else {
		receipt.IndexUpdated = true
	}
	if err := writeJSON(receiptPath, receipt); err != nil {
		return err
	}
	_ = uploader.Upload(receiptPath, config.runURI(runID)+"tos_upload.json")
	if config.strict && receipt.Status != "success" {
		return fmt.Errorf("artifact publishing failed: %s", strings.Join(receipt.Errors, "; "))
	}
	if receipt.Status != "success" {
		t.Logf("Warning: artifact publishing finished with status %s: %s", receipt.Status, strings.Join(receipt.Errors, "; "))
	}
	return nil
}

// publishFilesWithAggregate uploads per-run artifacts then appends rows to the aggregate CSV.
func publishFilesWithAggregate(t *testing.T, config publishConfig, logRoot, runID string, manifest map[string]any, files []publishMapping, scenario *resolver.Scenario, summary scenarioSummary, startedAt, finishedAt time.Time) error {
	t.Helper()
	if err := publishFiles(t, config, logRoot, runID, manifest, files); err != nil {
		return err
	}
	uploader, err := newTOSUploader()
	if err != nil {
		return err
	}
	return maybeUpdateAggregateCSV(t, uploader, config, scenario, summary, runID, startedAt, finishedAt)
}

func uploadWithRetry(uploader tosUploader, localPath, remoteURI string, strict bool, receipt *publishReceipt) error {
	attempts := 1
	if strict {
		attempts = 3
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		receipt.Attempts++
		err = uploader.Upload(localPath, remoteURI)
		if err == nil {
			return nil
		}
	}
	return err
}

func updateTOSIndex(uploader tosUploader, config publishConfig, runID string, manifest map[string]any, receipt publishReceipt) error {
	tempDir, err := os.MkdirTemp("", "brixbench-tos-index-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	indexPath := filepath.Join(tempDir, "runs.jsonl")
	indexURI := fmt.Sprintf("tos://%s/%s/index/runs.jsonl", config.bucket, config.prefix)
	_ = uploader.Download(indexURI, indexPath)
	existing, _ := os.ReadFile(indexPath)
	lines := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(existing)), "\n") {
		if !strings.Contains(line, fmt.Sprintf("\"run_id\":\"%s\"", runID)) {
			lines = append(lines, line)
		}
	}
	entry := map[string]any{"run_id": runID, "started_at": manifest["started_at"], "finished_at": manifest["finished_at"], "category": manifest["category"], "scenario": manifest["scenario"], "platform": manifest["platform"], "version": manifest["version"], "status": manifest["status"], "tos_uri": manifest["tos_uri"], "upload_status": receipt.Status}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	lines = append(lines, string(body))
	if err := os.WriteFile(indexPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return err
	}
	return uploader.Upload(indexPath, indexURI)
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0644)
}

func TestCasePublishMappingsIncludesProviderExtensions(t *testing.T) {
	root := t.TempDir()
	dynamo := resolver.Test{Name: "dynamo-case", Provider: stringPointer("dynamo")}
	aibrix := resolver.Test{Name: "aibrix-case", Provider: stringPointer("aibrix")}

	assertPublishTarget(t, casePublishMappings(root, dynamo, "standard"), "cases/dynamo-case/manifests/platform-values.yaml")
	assertPublishTarget(t, casePublishMappings(root, dynamo, "full"), "cases/dynamo-case/debug/dynamo-artifacts")
	assertPublishTarget(t, casePublishMappings(root, aibrix, "standard"), "cases/aibrix-case/logs/gateway")
	assertPublishTarget(t, casePublishMappings(root, aibrix, "full"), "cases/aibrix-case/debug/cluster-snapshots")
}

func TestPublishDisabledIsNoop(t *testing.T) {
	t.Setenv("BENCHMARK_PUBLISH_RESULTS", "false")
	publisherCreated := false
	original := newTOSUploader
	newTOSUploader = func() (tosUploader, error) {
		publisherCreated = true
		return &fakeTOSUploader{}, nil
	}
	defer func() { newTOSUploader = original }()

	scenario := &resolver.Scenario{Name: "smoke"}
	if err := maybePublishScenarioArtifacts(t, scenario, "missing.yaml", t.TempDir(), "run", time.Now(), scenarioSummary{}); err != nil {
		t.Fatalf("maybePublishScenarioArtifacts() error = %v", err)
	}
	if publisherCreated {
		t.Fatal("publisher was created when publishing was disabled")
	}
}

func TestPublishStrictFailsAfterRetries(t *testing.T) {
	uploader := &fakeTOSUploader{uploadErr: fmt.Errorf("unavailable")}
	original := newTOSUploader
	newTOSUploader = func() (tosUploader, error) { return uploader, nil }
	defer func() { newTOSUploader = original }()

	logRoot := t.TempDir()
	source := filepath.Join(logRoot, "summary.json")
	if err := os.WriteFile(source, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	err := publishFiles(t, publishConfig{bucket: "bucket", prefix: "prefix", strict: true}, logRoot, "run", map[string]any{}, []publishMapping{{source: source, target: "summary/summary.json"}})
	if err == nil {
		t.Fatal("publishFiles() succeeded with a failing strict uploader")
	}
	if len(uploader.uploads) < 3 {
		t.Fatalf("uploads = %d, want at least 3 retries", len(uploader.uploads))
	}
}

func TestRedactSensitiveLinesMasksSecretData(t *testing.T) {
	body := []byte("apiVersion: v1\nkind: Secret\nstringData:\n  password: clear-text\n  token: abc\nmetadata:\n  name: test\n")
	redacted := string(redactSensitiveLines(body))
	if strings.Contains(redacted, "clear-text") || strings.Contains(redacted, "abc") {
		t.Fatalf("secret data was not redacted: %s", redacted)
	}
}

func assertPublishTarget(t *testing.T, mappings []publishMapping, want string) {
	t.Helper()
	for _, mapping := range mappings {
		if filepath.ToSlash(mapping.target) == want {
			return
		}
	}
	t.Fatalf("mapping target %q not found", want)
}

func stringPointer(value string) *string {
	return &value
}
