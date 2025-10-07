// Copyright 2025 The AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kvcache

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
)

// Metric labels
const (
	LabelPodKey    = "pod_key"
	LabelEventType = "event_type"
	LabelErrorType = "error_type"
)

// ZMQClientMetrics holds all metrics for the ZMQ client
type ZMQClientMetrics struct {
	podKey string

	// Connection metrics
	connectionCount    prometheus.Counter
	disconnectionCount prometheus.Counter
	reconnectAttempts  prometheus.Counter

	// Event metrics
	eventsReceived      *prometheus.CounterVec
	eventsProcessed     *prometheus.CounterVec
	eventProcessingTime prometheus.Observer
	missedEvents        prometheus.Counter

	// Replay metrics
	replayRequests prometheus.Counter
	replaySuccess  prometheus.Counter
	replayFailures prometheus.Counter

	// Error metrics
	errors *prometheus.CounterVec

	// State metrics
	connected      prometheus.Gauge
	lastSequenceID prometheus.Gauge
}

// KVCacheMetrics holds all KV cache metrics
type KVCacheMetrics struct {
	// Connection metrics
	zmqConnectionTotal        *prometheus.CounterVec
	zmqDisconnectionTotal     *prometheus.CounterVec
	zmqReconnectAttemptsTotal *prometheus.CounterVec

	// Event metrics
	zmqEventsReceivedTotal     *prometheus.CounterVec
	zmqEventsProcessedTotal    *prometheus.CounterVec
	zmqEventProcessingDuration *prometheus.HistogramVec
	zmqMissedEventsTotal       *prometheus.CounterVec

	// Replay metrics
	zmqReplayRequestsTotal *prometheus.CounterVec
	zmqReplaySuccessTotal  *prometheus.CounterVec
	zmqReplayFailuresTotal *prometheus.CounterVec

	// Error metrics
	zmqErrorsTotal *prometheus.CounterVec

	// State metrics
	zmqConnectionStatus *prometheus.GaugeVec
	zmqLastSequenceID   *prometheus.GaugeVec
}

var (
	// Global metrics instance - only created when KV event sync is enabled
	kvCacheMetrics     *KVCacheMetrics
	kvCacheMetricsOnce sync.Once
	kvCacheMetricsMu   sync.RWMutex
)

// createKVCacheMetrics creates all KV cache metrics (but doesn't register them)
func createKVCacheMetrics() *KVCacheMetrics {
	return &KVCacheMetrics{
		// Connection metrics
		zmqConnectionTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_connections_total",
				Help: "Total number of ZMQ connections established",
			},
			[]string{LabelPodKey},
		),
		zmqDisconnectionTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_disconnections_total",
				Help: "Total number of ZMQ disconnections",
			},
			[]string{LabelPodKey},
		),
		zmqReconnectAttemptsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_reconnect_attempts_total",
				Help: "Total number of ZMQ reconnection attempts",
			},
			[]string{LabelPodKey},
		),

		// Event metrics
		zmqEventsReceivedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_events_received_total",
				Help: "Total number of KV cache events received",
			},
			[]string{LabelPodKey, LabelEventType},
		),
		zmqEventsProcessedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_events_processed_total",
				Help: "Total number of KV cache events successfully processed",
			},
			[]string{LabelPodKey, LabelEventType},
		),
		zmqEventProcessingDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "aibrix_kvcache_zmq_event_processing_duration_seconds",
				Help:    "Time taken to process KV cache events",
				Buckets: prometheus.ExponentialBuckets(0.00001, 2, 15), // 10us to ~160ms
			},
			[]string{LabelPodKey},
		),
		zmqMissedEventsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_missed_events_total",
				Help: "Total number of missed events detected",
			},
			[]string{LabelPodKey},
		),

		// Replay metrics
		zmqReplayRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_replay_requests_total",
				Help: "Total number of replay requests sent",
			},
			[]string{LabelPodKey},
		),
		zmqReplaySuccessTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_replay_success_total",
				Help: "Total number of successful replay responses",
			},
			[]string{LabelPodKey},
		),
		zmqReplayFailuresTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_replay_failures_total",
				Help: "Total number of failed replay requests",
			},
			[]string{LabelPodKey},
		),

		// Error metrics
		zmqErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "aibrix_kvcache_zmq_errors_total",
				Help: "Total number of errors encountered",
			},
			[]string{LabelPodKey, LabelErrorType},
		),

		// State metrics
		zmqConnectionStatus: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "aibrix_kvcache_zmq_connection_status",
				Help: "Current connection status (1=connected, 0=disconnected)",
			},
			[]string{LabelPodKey},
		),
		zmqLastSequenceID: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "aibrix_kvcache_zmq_last_sequence_id",
				Help: "Last processed sequence ID",
			},
			[]string{LabelPodKey},
		),
	}
}

// registerKVCacheMetrics registers all metrics with Prometheus
func (m *KVCacheMetrics) register() error {
	collectors := []prometheus.Collector{
		m.zmqConnectionTotal,
		m.zmqDisconnectionTotal,
		m.zmqReconnectAttemptsTotal,
		m.zmqEventsReceivedTotal,
		m.zmqEventsProcessedTotal,
		m.zmqEventProcessingDuration,
		m.zmqMissedEventsTotal,
		m.zmqReplayRequestsTotal,
		m.zmqReplaySuccessTotal,
		m.zmqReplayFailuresTotal,
		m.zmqErrorsTotal,
		m.zmqConnectionStatus,
		m.zmqLastSequenceID,
	}

	for _, collector := range collectors {
		if err := prometheus.Register(collector); err != nil {
			// If already registered, it's ok (might happen in tests)
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return fmt.Errorf("failed to register metric: %w", err)
			}
		}
	}

	return nil
}

// InitializeMetrics initializes KV cache metrics if not already done
// This should be called when KV event sync is enabled
func InitializeMetrics() error {
	var err error
	kvCacheMetricsOnce.Do(func() {
		kvCacheMetricsMu.Lock()
		defer kvCacheMetricsMu.Unlock()

		metrics := createKVCacheMetrics()
		if registerErr := metrics.register(); registerErr != nil {
			err = registerErr
			klog.Errorf("Failed to register KV cache metrics: %v", registerErr)
			return
		}
		kvCacheMetrics = metrics
		klog.Info("KV cache metrics registered successfully")
	})
	return err
}

// getMetrics returns the global metrics instance if available
func getMetrics() *KVCacheMetrics {
	kvCacheMetricsMu.RLock()
	defer kvCacheMetricsMu.RUnlock()
	return kvCacheMetrics
}

// NewZMQClientMetrics creates a new metrics instance for a ZMQ client
func NewZMQClientMetrics(podKey string) *ZMQClientMetrics {
	metrics := getMetrics()
	if metrics == nil {
		// Return a no-op metrics instance
		return &ZMQClientMetrics{
			podKey: podKey,
		}
	}

	return &ZMQClientMetrics{
		podKey:              podKey,
		connectionCount:     metrics.zmqConnectionTotal.WithLabelValues(podKey),
		disconnectionCount:  metrics.zmqDisconnectionTotal.WithLabelValues(podKey),
		reconnectAttempts:   metrics.zmqReconnectAttemptsTotal.WithLabelValues(podKey),
		eventsReceived:      metrics.zmqEventsReceivedTotal,
		eventsProcessed:     metrics.zmqEventsProcessedTotal,
		eventProcessingTime: metrics.zmqEventProcessingDuration.WithLabelValues(podKey),
		missedEvents:        metrics.zmqMissedEventsTotal.WithLabelValues(podKey),
		replayRequests:      metrics.zmqReplayRequestsTotal.WithLabelValues(podKey),
		replaySuccess:       metrics.zmqReplaySuccessTotal.WithLabelValues(podKey),
		replayFailures:      metrics.zmqReplayFailuresTotal.WithLabelValues(podKey),
		errors:              metrics.zmqErrorsTotal,
		connected:           metrics.zmqConnectionStatus.WithLabelValues(podKey),
		lastSequenceID:      metrics.zmqLastSequenceID.WithLabelValues(podKey),
	}
}

// IncrementConnectionCount increments the connection counter
func (m *ZMQClientMetrics) IncrementConnectionCount() {
	if m.connectionCount != nil {
		m.connectionCount.Inc()
	}
	if m.connected != nil {
		m.connected.Set(1)
	}
}

// IncrementDisconnectionCount increments the disconnection counter
func (m *ZMQClientMetrics) IncrementDisconnectionCount() {
	if m.disconnectionCount != nil {
		m.disconnectionCount.Inc()
	}
	if m.connected != nil {
		m.connected.Set(0)
	}
}

// IncrementReconnectAttempts increments the reconnect attempts counter
func (m *ZMQClientMetrics) IncrementReconnectAttempts() {
	if m.reconnectAttempts != nil {
		m.reconnectAttempts.Inc()
	}
}

// IncrementEventCount increments the event counter for a specific event type
func (m *ZMQClientMetrics) IncrementEventCount(eventType string) {
	if m.eventsReceived != nil {
		m.eventsReceived.WithLabelValues(m.podKey, eventType).Inc()
	}
	if m.eventsProcessed != nil {
		m.eventsProcessed.WithLabelValues(m.podKey, eventType).Inc()
	}
}

// RecordEventProcessingLatency records the time taken to process an event
func (m *ZMQClientMetrics) RecordEventProcessingLatency(duration time.Duration) {
	if m.eventProcessingTime != nil {
		m.eventProcessingTime.Observe(duration.Seconds())
	}
}

// IncrementMissedEvents increments the missed events counter
func (m *ZMQClientMetrics) IncrementMissedEvents(count int64) {
	if m.missedEvents != nil {
		m.missedEvents.Add(float64(count))
	}
}

// IncrementReplayCount increments the replay request counter
func (m *ZMQClientMetrics) IncrementReplayCount() {
	if m.replayRequests != nil {
		m.replayRequests.Inc()
	}
}

// IncrementReplaySuccess increments the successful replay counter
func (m *ZMQClientMetrics) IncrementReplaySuccess() {
	if m.replaySuccess != nil {
		m.replaySuccess.Inc()
	}
}

// IncrementReplayFailure increments the failed replay counter
func (m *ZMQClientMetrics) IncrementReplayFailure() {
	if m.replayFailures != nil {
		m.replayFailures.Inc()
	}
}

// IncrementErrorCount increments the error counter for a specific error type
func (m *ZMQClientMetrics) IncrementErrorCount(errorType string) {
	if m.errors != nil {
		m.errors.WithLabelValues(m.podKey, errorType).Inc()
	}
}

// UpdateLastSequenceID updates the last processed sequence ID gauge
func (m *ZMQClientMetrics) UpdateLastSequenceID(seqID int64) {
	if m.lastSequenceID != nil {
		m.lastSequenceID.Set(float64(seqID))
	}
}

// Delete removes all metrics for this pod (useful for cleanup)
func (m *ZMQClientMetrics) Delete() {
	metrics := getMetrics()
	if metrics == nil {
		return
	}

	// Delete all labeled metrics
	metrics.zmqConnectionTotal.DeleteLabelValues(m.podKey)
	metrics.zmqDisconnectionTotal.DeleteLabelValues(m.podKey)
	metrics.zmqReconnectAttemptsTotal.DeleteLabelValues(m.podKey)
	metrics.zmqMissedEventsTotal.DeleteLabelValues(m.podKey)
	metrics.zmqReplayRequestsTotal.DeleteLabelValues(m.podKey)
	metrics.zmqReplaySuccessTotal.DeleteLabelValues(m.podKey)
	metrics.zmqReplayFailuresTotal.DeleteLabelValues(m.podKey)
	metrics.zmqConnectionStatus.DeleteLabelValues(m.podKey)
	metrics.zmqLastSequenceID.DeleteLabelValues(m.podKey)
	metrics.zmqEventProcessingDuration.DeleteLabelValues(m.podKey)

	// Delete event type specific metrics
	for _, eventType := range []string{
		string(EventTypeBlockStored),
		string(EventTypeBlockRemoved),
		string(EventTypeAllCleared),
	} {
		metrics.zmqEventsReceivedTotal.DeleteLabelValues(m.podKey, eventType)
		metrics.zmqEventsProcessedTotal.DeleteLabelValues(m.podKey, eventType)
	}

	// Delete error type specific metrics
	for _, errorType := range []string{
		"consume_events",
		"reconnect",
		"decode",
		"handle_event",
	} {
		metrics.zmqErrorsTotal.DeleteLabelValues(m.podKey, errorType)
	}
}
