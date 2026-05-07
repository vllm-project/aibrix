package observability

import "context"

// MetricExporter is an interface for sending benchmark results to external observability systems.
type MetricExporter interface {
	// Export sends the collected metrics to the configured sink (e.g., Prometheus Pushgateway, Datadog, etc.).
	Export(ctx context.Context, metrics map[string]interface{}, labels map[string]string) error
}

// PrometheusPushExporter implements MetricExporter by pushing metrics to a Prometheus Pushgateway.
type PrometheusPushExporter struct {
	PushgatewayURL string
	JobName        string
}

func NewPrometheusPushExporter(url, job string) *PrometheusPushExporter {
	return &PrometheusPushExporter{
		PushgatewayURL: url,
		JobName:        job,
	}
}

func (e *PrometheusPushExporter) Export(ctx context.Context, metrics map[string]interface{}, labels map[string]string) error {
	// In reality, use prometheus/client_golang/prometheus/push to send metrics.
	// This would convert the map[string]interface{} into Prometheus Gauge/Counter metrics.
	// For now, we simulate the export.
	for k, v := range metrics {
		// e.g., pushMetric(k, v, labels)
		_ = k
		_ = v
	}
	return nil
}
