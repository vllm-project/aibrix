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
	"context"
	"errors"
	"testing"

	"github.com/vllm-project/aibrix/brixbench/internal/observability"
)

func TestConfiguredMetricExporterDisabledByDefault(t *testing.T) {
	t.Setenv("BENCHMARK_PUSHGATEWAY_URL", "")
	t.Setenv("BENCHMARK_PUSHGATEWAY_JOB", "")

	if exporter := configuredMetricExporter(); exporter != nil {
		t.Fatalf("configuredMetricExporter() = %#v, want nil", exporter)
	}
}

func TestConfiguredMetricExporterUsesEnv(t *testing.T) {
	t.Setenv("BENCHMARK_PUSHGATEWAY_URL", "http://pushgateway.example")
	t.Setenv("BENCHMARK_PUSHGATEWAY_JOB", "custom-job")

	exporter, ok := configuredMetricExporter().(*observability.PrometheusPushExporter)
	if !ok {
		t.Fatalf("configuredMetricExporter() type = %T, want *observability.PrometheusPushExporter", configuredMetricExporter())
	}
	if exporter.PushgatewayURL != "http://pushgateway.example" {
		t.Fatalf("PushgatewayURL = %q, want %q", exporter.PushgatewayURL, "http://pushgateway.example")
	}
	if exporter.JobName != "custom-job" {
		t.Fatalf("JobName = %q, want %q", exporter.JobName, "custom-job")
	}
}

func TestConfiguredMetricExporterUsesDefaultJobName(t *testing.T) {
	t.Setenv("BENCHMARK_PUSHGATEWAY_URL", "http://pushgateway.example")
	t.Setenv("BENCHMARK_PUSHGATEWAY_JOB", "")

	exporter, ok := configuredMetricExporter().(*observability.PrometheusPushExporter)
	if !ok {
		t.Fatalf("configuredMetricExporter() type = %T, want *observability.PrometheusPushExporter", configuredMetricExporter())
	}
	if exporter.JobName != defaultPushgatewayJobName {
		t.Fatalf("JobName = %q, want %q", exporter.JobName, defaultPushgatewayJobName)
	}
}

func TestExportMetricsIfConfiguredDisabledByDefault(t *testing.T) {
	err := exportMetricsIfConfigured(context.Background(), nil, map[string]any{"ttft_ms": 123.4}, map[string]string{"scenario": "hello"})
	if err != nil {
		t.Fatalf("exportMetricsIfConfigured() error = %v, want nil", err)
	}
}

func TestExportMetricsIfConfiguredReturnsNotImplemented(t *testing.T) {
	exporter := observability.NewPrometheusPushExporter("http://pushgateway.example", "benchmark-suite")

	err := exportMetricsIfConfigured(context.Background(), exporter, map[string]any{"ttft_ms": 123.4}, map[string]string{"scenario": "hello"})
	if !errors.Is(err, observability.ErrNotImplemented) {
		t.Fatalf("exportMetricsIfConfigured() error = %v, want ErrNotImplemented", err)
	}
}
