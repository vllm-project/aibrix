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
