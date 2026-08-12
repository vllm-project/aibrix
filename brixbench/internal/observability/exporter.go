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
)

// ErrNotImplemented indicates that the configured exporter has no real sink implementation yet.
var ErrNotImplemented = errors.New("metrics exporter is not implemented")

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
	_ = ctx
	_ = metrics
	_ = labels
	return ErrNotImplemented
}
