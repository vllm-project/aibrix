/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package external

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// externalRouterMetrics intentionally exposes only fixed label dimensions to
// avoid cardinality growth from request or candidate data.
type externalRouterMetrics struct {
	requests *prometheus.CounterVec
	fallback *prometheus.CounterVec
	duration prometheus.Histogram
	inflight prometheus.Gauge
	circuit  *prometheus.GaugeVec
}

func newExternalRouterMetrics(registerer prometheus.Registerer) *externalRouterMetrics {
	metrics := &externalRouterMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_external_router_requests_total",
			Help:      "External router requests by fixed outcome.",
		}, []string{"outcome"}),
		fallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_external_router_fallback_total",
			Help:      "External router fallback invocations by fixed reason.",
		}, []string{"reason"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_external_router_duration_seconds",
			Help:      "External routing service exchange duration.",
			Buckets:   prometheus.ExponentialBuckets(0.0005, 2, 12),
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_external_router_inflight",
			Help:      "Current external routing HTTP exchanges.",
		}),
		circuit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_external_router_circuit_state",
			Help:      "External router circuit state (1 for current state).",
		}, []string{"state"}),
	}
	metrics.requests = registerCounterVec(registerer, metrics.requests)
	metrics.fallback = registerCounterVec(registerer, metrics.fallback)
	metrics.duration = registerHistogram(registerer, metrics.duration)
	metrics.inflight = registerGauge(registerer, metrics.inflight)
	metrics.circuit = registerGaugeVec(registerer, metrics.circuit)
	metrics.setCircuit("closed")
	return metrics
}

func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registerer.Register(collector); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
	}
	return collector
}

func registerGaugeVec(registerer prometheus.Registerer, collector *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := registerer.Register(collector); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(*prometheus.GaugeVec)
		}
	}
	return collector
}

func registerHistogram(registerer prometheus.Registerer, collector prometheus.Histogram) prometheus.Histogram {
	if err := registerer.Register(collector); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(prometheus.Histogram)
		}
	}
	return collector
}

func registerGauge(registerer prometheus.Registerer, collector prometheus.Gauge) prometheus.Gauge {
	if err := registerer.Register(collector); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(prometheus.Gauge)
		}
	}
	return collector
}

func (m *externalRouterMetrics) setCircuit(state string) {
	// Export the state machine as a one-hot gauge set for simple alerting.
	for _, name := range []string{"closed", "open", "half_open"} {
		value := 0.0
		if name == state {
			value = 1
		}
		m.circuit.WithLabelValues(name).Set(value)
	}
}
