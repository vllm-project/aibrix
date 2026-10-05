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
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-metrics/datadog"
	goMetricsPrometheus "github.com/hashicorp/go-metrics/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/config"
)

// Emitter is the package-level default Sink. It is set by Setup and
// read by call sites throughout the application.
var Emitter Sink = NoopSink{}

// Setup initialises the metrics subsystem from cfg.Metrics, creates all
// configured backend sinks, and sets the package-level Emitter.
// Returns the Sink and a Prometheus HTTP handler (or nil).
func Setup(cfg *config.Config) (Sink, http.Handler, error) {
	mc := cfg.Metrics

	if mc == nil {
		Emitter = NoopSink{}
		return Emitter, nil, nil
	}

	serviceName := mc.ServiceName
	if serviceName == "" {
		serviceName = "aibrix-console"
	}

	var fanout FanoutSink
	var goMetricsFanout metrics.FanoutSink
	var promHandler http.Handler

	ok := false
	defer func() {
		if !ok {
			for _, s := range goMetricsFanout {
				if ss, ok := s.(metrics.ShutdownSink); ok {
					ss.Shutdown()
				}
			}

			for _, s := range fanout {
				_ = s.Close()
			}
		}
	}()

	// --- Open-source backends ---

	if mc.PrometheusEnabled {
		retention := mc.PrometheusRetentionTime
		if retention == 0 {
			retention = 24 * time.Hour
		}
		sink, err := goMetricsPrometheus.NewPrometheusSinkFrom(goMetricsPrometheus.PrometheusOpts{
			Expiration: retention,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("prometheus sink: %w", err)
		}
		goMetricsFanout = append(goMetricsFanout, sink)
		promHandler = promhttp.Handler()
	}

	if mc.StatsiteAddr != "" {
		sink, err := metrics.NewStatsiteSink(mc.StatsiteAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("statsite sink: %w", err)
		}
		goMetricsFanout = append(goMetricsFanout, sink)
	}

	if mc.StatsDAddr != "" {
		sink, err := metrics.NewStatsdSink(mc.StatsDAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("statsd sink: %w", err)
		}
		goMetricsFanout = append(goMetricsFanout, sink)
	}

	if mc.DogStatsDAddr != "" {
		sink, err := datadog.NewDogStatsdSink(mc.DogStatsDAddr, serviceName)
		if err != nil {
			return nil, nil, fmt.Errorf("dogstatsd sink: %w", err)
		}
		sink.SetTags(mc.DogStatsDTags)
		goMetricsFanout = append(goMetricsFanout, sink)
	}

	if len(goMetricsFanout) > 0 {
		gmSink, err := NewGoMetricsSink(serviceName, mc.DisableHostname, goMetricsFanout)
		if err != nil {
			return nil, nil, fmt.Errorf("go-metrics init: %w", err)
		}
		fanout = append(fanout, gmSink)
		goMetricsFanout = metrics.FanoutSink{}
	}

	// --- Custom backends ---

	if ext := mc.MetricsConfigExtension; ext != nil {
		sinks, err := setupMetricsExtension(mc.MetricsConfigExtension)
		if err != nil {
			return nil, nil, fmt.Errorf("metrics extension setup: %w", err)
		}
		fanout = append(fanout, sinks...)
	}

	if len(fanout) == 0 {
		klog.Warningf("metrics config provided but no backend enabled; metrics will be discarded")
		Emitter = NoopSink{}
		ok = true
		return Emitter, nil, nil
	}

	if len(fanout) == 1 {
		Emitter = fanout[0]
	} else {
		Emitter = fanout
	}

	ok = true
	return Emitter, promHandler, nil
}

// Shutdown closes the global Emitter.
func Shutdown() {
	if Emitter != nil {
		_ = Emitter.Close()
	}
}
