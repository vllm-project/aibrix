/*
Copyright 2024 The Aibrix Team.

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

package monitor

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type Monitor interface {
	// RecordScaleAction exports the latest scaling decision of a PodAutoscaler.
	// Only the latest one is exported, so a series from an earlier algorithm or
	// reason does not keep reporting its old desired replica count.
	RecordScaleAction(namespace, name, algorithm, reason string, desiredReplicas int32)

	// ForgetScaleAction removes every scale action series recorded for a
	// PodAutoscaler, so a deleted one stops being exported.
	ForgetScaleAction(namespace, name string)
}

type podAutoscalerKey struct {
	namespace, name string
}

type scaleActionLabels struct {
	algorithm, reason string
}

type monitor struct {
	mu sync.Mutex
	// lastScaleAction is the algorithm and reason of the series each
	// PodAutoscaler last reported.
	lastScaleAction map[podAutoscalerKey]scaleActionLabels
}

func New() Monitor {
	return &monitor{lastScaleAction: make(map[podAutoscalerKey]scaleActionLabels)}
}

func (m *monitor) RecordScaleAction(namespace, name, algorithm, reason string, desiredReplicas int32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	autoscalerScaleAction.With(prometheus.Labels{
		"namespace": namespace,
		"name":      name,
		"algorithm": algorithm,
		"reason":    reason,
	}).Set(float64(desiredReplicas))

	// The reason is a label, so a new reason starts a new series. Drop the one
	// it replaces; it would otherwise keep its last value until the object is
	// deleted, and a query over the PodAutoscaler would mix old and new
	// desired replica counts. The new series is set first so a scrape in
	// between never finds the PodAutoscaler missing.
	key := podAutoscalerKey{namespace: namespace, name: name}
	current := scaleActionLabels{algorithm: algorithm, reason: reason}
	if previous, ok := m.lastScaleAction[key]; ok && previous != current {
		autoscalerScaleAction.DeleteLabelValues(namespace, name, previous.algorithm, previous.reason)
	}
	m.lastScaleAction[key] = current
}

func (m *monitor) ForgetScaleAction(namespace, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.lastScaleAction, podAutoscalerKey{namespace: namespace, name: name})
	autoscalerScaleAction.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"name":      name,
	})
}
