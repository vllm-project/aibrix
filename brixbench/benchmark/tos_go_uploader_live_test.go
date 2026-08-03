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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live smoke against the real TOS bucket. Opt-in:
//
//	BENCHMARK_TOS_SMOKE=1 TOS_ACCESS_KEY=... TOS_SECRET_KEY=... \
//	  go test ./benchmark -run TestLiveTOSGoUploaderSmoke -count=1 -v
func TestLiveTOSGoUploaderSmoke(t *testing.T) {
	if os.Getenv("BENCHMARK_TOS_SMOKE") != "1" {
		t.Skip("set BENCHMARK_TOS_SMOKE=1 to run live TOS Go uploader smoke")
	}
	uploader, err := newGoTOSUploader()
	if err != nil {
		t.Fatal(err)
	}
	config := configuredPublisher()
	stamp := time.Now().UTC().Format("20060102T150405Z")
	runID := "smoke-go-api-" + stamp
	artifactURI := config.runURI(runID) + "smoke.txt"
	csvURI := fmt.Sprintf("tos://%s/%s/aggregates/benchmark_metrics.csv", config.bucket, config.prefix)

	dir := t.TempDir()
	localArtifact := filepath.Join(dir, "smoke.txt")
	if err := os.WriteFile(localArtifact, []byte("brixbench-go-tos-smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := uploader.Upload(localArtifact, artifactURI); err != nil {
		t.Fatalf("artifact upload: %v", err)
	}
	downloaded := filepath.Join(dir, "smoke.down.txt")
	if err := uploader.Download(artifactURI, downloaded); err != nil {
		t.Fatalf("artifact download: %v", err)
	}

	row1 := []map[string]string{{
		"schema_version":   "1.0",
		"row_id":           runID + ":case-a",
		"run_id":           runID,
		"testcase":         "case-a",
		"status":           "passed",
		"platform":         "aibrix",
		"platform_version": "v0.6.0",
		"platform_commit":  "deadbee",
		"series_label":     "Aibrix v0.6.0 + vllm 0.22.0 + pd",
		"topology":         "4p4d-singlenode",
	}}
	if err := appendAggregateCSV(uploader, config, row1); err != nil {
		t.Fatalf("csv create/append: %v", err)
	}
	row2 := []map[string]string{{
		"schema_version":   "1.0",
		"row_id":           runID + ":case-b",
		"run_id":           runID,
		"testcase":         "case-b",
		"status":           "failed",
		"platform":         "dynamo",
		"platform_version": "v1.2.1",
		"series_label":     "Dynamo v1.2.1 + vllm 0.21.0 + round-robin",
		"topology":         "4p4d-singlenode",
	}}
	if err := appendAggregateCSV(uploader, config, row2); err != nil {
		t.Fatalf("csv second append: %v", err)
	}

	localCSV := filepath.Join(dir, "benchmark_metrics.csv")
	if err := uploader.Download(csvURI, localCSV); err != nil {
		t.Fatalf("csv download: %v", err)
	}
	body, err := os.ReadFile(localCSV)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "platform_commit") {
		t.Fatalf("csv missing platform_commit header")
	}
	if strings.Count(text, "schema_version") != 1 {
		t.Fatalf("expected single header, got:\n%s", text)
	}
	if !strings.Contains(text, runID+":case-a") || !strings.Contains(text, runID+":case-b") {
		t.Fatalf("csv missing appended rows:\n%s", text)
	}

	// Cleanup smoke artifacts and CSV so Step 3 starts clean.
	if err := uploader.Delete(artifactURI); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if err := uploader.Delete(csvURI); err != nil {
		t.Fatalf("delete csv: %v", err)
	}
	t.Logf("live TOS smoke OK; cleaned %s and %s", artifactURI, csvURI)
}
