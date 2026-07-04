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

package modelclaim

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// ModelClaim control-plane observability. These metrics are purely additive:
// they reflect lifecycle state the controller already computes (desired/ready
// replicas, activation readiness, and activation outcomes) and change no
// control logic. Registered with the controller-runtime registry, so they are
// exported on the controller-manager's metrics endpoint alongside the other
// aibrix_* controller metrics.
//
// Cardinality is namespace×model (models are O(tens..hundreds), bounded). The
// gauge series for a model are deleted when the model is (so a removed model
// does not leave a series frozen at its last value); counters are left to
// stop exporting on their own, per Prometheus convention.
var (
	claimDesiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_desired_replicas",
			Help:      "Desired number of active engine instances for a model claim.",
		},
		[]string{"namespace", "model"},
	)
	claimReadyReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_ready_replicas",
			Help:      "Number of serveable (Active) engine instances for a model claim.",
		},
		[]string{"namespace", "model"},
	)
	claimActivating = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_activating",
			Help:      "1 when the model claim has an engine still booting (Activating), else 0.",
		},
		[]string{"namespace", "model"},
	)
	claimActivationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_activation_total",
			Help:      "Count of engine activation outcomes (result=success|failed) for a model claim.",
		},
		[]string{"namespace", "model", "result"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		claimDesiredReplicas,
		claimReadyReplicas,
		claimActivating,
		claimActivationTotal,
	)
}

// b2f maps a bool to a gauge value (1/0).
func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// setClaimGauges refreshes the per-model gauges from the model's current
// status. Called at the end of each reconcile so the gauges track the truth the
// controller just computed.
func setClaimGauges(pm *modelv1alpha1.ModelClaim) {
	ns, model := pm.Namespace, servedModelName(pm)
	claimDesiredReplicas.WithLabelValues(ns, model).Set(float64(pm.Status.DesiredReplicas))
	claimReadyReplicas.WithLabelValues(ns, model).Set(float64(pm.Status.ReadyReplicas))

	activating := false
	for i := range pm.Status.Instances {
		if pm.Status.Instances[i].Phase == modelv1alpha1.ModelClaimActivating {
			activating = true
			break
		}
	}
	claimActivating.WithLabelValues(ns, model).Set(b2f(activating))
}

// clearClaimMetrics drops a model's gauge series on deletion, so a removed model
// does not leave a gauge frozen at its last value.
func clearClaimMetrics(namespace, model string) {
	claimDesiredReplicas.DeleteLabelValues(namespace, model)
	claimReadyReplicas.DeleteLabelValues(namespace, model)
	claimActivating.DeleteLabelValues(namespace, model)
}

func recordActivation(namespace, model string, ok bool) {
	result := "success"
	if !ok {
		result = "failed"
	}
	claimActivationTotal.WithLabelValues(namespace, model, result).Inc()
}
