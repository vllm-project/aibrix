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

package observability

import (
	"context"
	"errors"
	"testing"
)

func TestPrometheusPushExporterExportReturnsNotImplemented(t *testing.T) {
	exporter := NewPrometheusPushExporter("http://pushgateway.example", "benchmark-suite")

	err := exporter.Export(context.Background(), map[string]interface{}{"ttft_ms": 123.4}, map[string]string{"scenario": "hello"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Export() error = %v, want ErrNotImplemented", err)
	}
}
