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

package config

import (
	"os"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// MetricsConfig holds all configuration for the multi-backend metrics
// subsystem. Each backend is enabled by providing its address or
// required credentials; all enabled backends are composed into a fanout.
type MetricsConfig struct {
	// ServiceName is the metrics service identifier used as prefix.
	// Defaults to "aibrix-console".
	ServiceName string
	// DisableHostname suppresses the hostname from metric keys.
	DisableHostname bool

	// --- Open-source backends ---

	// StatsDAddr is the address of a StatsD server.
	StatsDAddr string
	// StatsiteAddr is the address of a Statsite server.
	StatsiteAddr string
	// PrometheusEnabled enables the Prometheus metrics endpoint.
	PrometheusEnabled bool
	// PrometheusRetentionTime is how long Prometheus metrics are retained.
	// Defaults to 24h.
	PrometheusRetentionTime time.Duration

	// DogStatsDAddr is the address of a DogStatsD instance.
	DogStatsDAddr string
	// DogStatsDTags are global tags sent with each DogStatsD packet.
	DogStatsDTags []string

	// --- Other custom backends ---

	*MetricsConfigExtension
}

// loadMetricsConfig reads metrics-related env vars. Returns nil when no
// metrics backend is enabled.
func loadMetricsConfig() *MetricsConfig {
	mc := &MetricsConfig{
		ServiceName:            envOrDefault("METRICS_SERVICE_NAME", "aibrix-console"),
		DisableHostname:        envBool("METRICS_DISABLE_HOSTNAME", false),
		StatsDAddr:             envOrDefault("METRICS_STATSD_ADDR", ""),
		StatsiteAddr:           envOrDefault("METRICS_STATSITE_ADDR", ""),
		PrometheusEnabled:      envBool("METRICS_PROMETHEUS_ENABLED", false),
		DogStatsDAddr:          envOrDefault("METRICS_DOGSTATSD_ADDR", ""),
		MetricsConfigExtension: loadMetricsConfigExtension(),
	}

	// DogStatsDTags from comma-separated env var
	if v := os.Getenv("METRICS_DOGSTATSD_TAGS"); v != "" {
		mc.DogStatsDTags = strings.Split(v, ",")
	}

	// Prometheus retention time
	if v := os.Getenv("METRICS_PROMETHEUS_RETENTION_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			klog.Warningf("invalid METRICS_PROMETHEUS_RETENTION_TIME %q: %v; using default 24h\n", v, err)
		} else {
			mc.PrometheusRetentionTime = d
		}
	}

	// Return nil when no backend is enabled so Setup() can short-circuit.
	if !mc.PrometheusEnabled && mc.StatsDAddr == "" && mc.StatsiteAddr == "" &&
		mc.DogStatsDAddr == "" && mc.MetricsConfigExtension == nil {
		return nil
	}

	return mc
}
