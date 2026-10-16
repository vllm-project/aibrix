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

package metrics

import (
	goMetrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-metrics/prometheus"
)

// GoMetricsSink wraps the hashicorp/go-metrics global instance, giving
// access to all open-source backends (StatsD, Statsite, Prometheus,
// Circonus, DogStatsD, Stackdriver) through a single Sink.
type GoMetricsSink struct {
	prometheusEnabled bool
}

// NewGoMetricsSink creates the hashicorp/go-metrics global instance from
// the supplied fanout sinks and config.
func NewGoMetricsSink(serviceName string, disableHostname bool, fanout goMetrics.FanoutSink) (*GoMetricsSink, error) {
	cfg := goMetrics.DefaultConfig(serviceName)
	cfg.EnableHostname = !disableHostname

	_, err := goMetrics.NewGlobal(cfg, fanout)
	if err != nil {
		return nil, err
	}

	promEnabled := false
	for _, s := range fanout {
		if _, ok := s.(*prometheus.PrometheusSink); ok {
			promEnabled = true
			break
		}
	}

	return &GoMetricsSink{prometheusEnabled: promEnabled}, nil
}

// PrometheusEnabled reports whether a Prometheus sink was configured.
func (g *GoMetricsSink) PrometheusEnabled() bool { return g.prometheusEnabled }

func (g *GoMetricsSink) Counter(name string, val float32, tags ...Tag) {
	goMetrics.IncrCounterWithLabels(splitName(name), float32(val), toLabels(tags))
}

func (g *GoMetricsSink) Gauge(name string, val float32, tags ...Tag) {
	goMetrics.SetGaugeWithLabels(splitName(name), float32(val), toLabels(tags))
}

func (g *GoMetricsSink) Timer(name string, val float32, tags ...Tag) {
	goMetrics.AddSampleWithLabels(splitName(name), float32(val), toLabels(tags))
}

func (g *GoMetricsSink) Store(name string, val float32, tags ...Tag) {
	g.Gauge(name, val, tags...)
}

func (g *GoMetricsSink) Rate(name string, val float32, tags ...Tag) {
	g.Counter(name, val, tags...)
}

func (g *GoMetricsSink) Close() error {
	goMetrics.Shutdown()
	return nil
}

// toLabels converts our Tag slice to hashicorp/go-metrics Labels.
func toLabels(tags []Tag) []goMetrics.Label {
	if len(tags) == 0 {
		return nil
	}
	labels := make([]goMetrics.Label, len(tags))
	for i, t := range tags {
		labels[i] = goMetrics.Label{Name: t.Name, Value: t.Value}
	}
	return labels
}
