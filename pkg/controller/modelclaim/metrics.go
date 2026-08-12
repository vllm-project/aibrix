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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	activationResultFailed = "failed"

	policyResultApplied = "applied"
	policyResultSkipped = "skipped"
	policyResultFailed  = "failed"

	policyReasonApplied             = "applied"
	policyReasonNoReclaimPolicy     = "no_reclaim_policy"
	policyReasonNoPods              = "no_pods"
	policyReasonPodListError        = "pod_list_error"
	policyReasonSnapshotError       = "snapshot_error"
	policyReasonEmptySnapshot       = "empty_snapshot"
	policyReasonUnsupportedTopology = "unsupported_topology"
	policyReasonIncompleteMetrics   = "incomplete_metrics"
	policyReasonUnsafePlan          = "unsafe_plan"
	policyReasonNoChange            = "no_change"
	policyReasonRuntimeError        = "runtime_error"
	policyReasonInternalError       = "internal_error"

	policyActionSetKVLimit = "set_kv_limit"
)

var boundedPolicyReasons = map[string]struct{}{
	policyReasonApplied:             {},
	poolPolicyErrorInvalidJSON:      {},
	poolPolicyErrorUnknownField:     {},
	poolPolicyErrorUnsupportedMode:  {},
	poolPolicyErrorInvalidCapacity:  {},
	poolPolicyErrorInvalidFloor:     {},
	policyReasonNoReclaimPolicy:     {},
	policyReasonNoPods:              {},
	policyReasonPodListError:        {},
	policyReasonSnapshotError:       {},
	policyReasonEmptySnapshot:       {},
	policyReasonUnsupportedTopology: {},
	policyReasonIncompleteMetrics:   {},
	policyReasonUnsafePlan:          {},
	policyReasonNoChange:            {},
	policyReasonRuntimeError:        {},
	policyReasonInternalError:       {},
}

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
	claimNoReadyDurationSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_no_ready_duration_seconds",
			Help:      "Seconds since a model claim last had a ready engine, or 0 while at least one engine is ready.",
		},
		[]string{"namespace", "model"},
	)
	// Cardinality is namespace×deployment, bounded by the number of warm pools.
	poolPolicyValid = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_pool_policy_valid",
			Help:      "1 when the warm pool Deployment policy annotation parses and validates, else 0.",
		},
		[]string{"namespace", "deployment"},
	)
	poolPolicyEvaluationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_pool_policy_evaluations_total",
			Help:      "Count of pool-policy evaluation outcomes with a bounded reason.",
		},
		[]string{"namespace", "pool", "result", "reason"},
	)
	poolPolicyActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "modelclaim_pool_policy_actions_total",
			Help:      "Count of pool-policy action outcomes with a bounded action and reason.",
		},
		[]string{"namespace", "pool", "action", "result", "reason"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		claimDesiredReplicas,
		claimReadyReplicas,
		claimActivating,
		claimActivationTotal,
		claimNoReadyDurationSeconds,
		poolPolicyValid,
		poolPolicyEvaluationsTotal,
		poolPolicyActionsTotal,
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
	setClaimGaugesAt(pm, time.Now())
}

func setClaimGaugesAt(pm *modelv1alpha1.ModelClaim, now time.Time) {
	ns, model := pm.Namespace, servedModelName(pm)
	claimDesiredReplicas.WithLabelValues(ns, model).Set(float64(pm.Status.DesiredReplicas))
	claimReadyReplicas.WithLabelValues(ns, model).Set(float64(pm.Status.ReadyReplicas))
	claimNoReadyDurationSeconds.WithLabelValues(ns, model).Set(claimNoReadyDuration(pm, now).Seconds())

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
	claimNoReadyDurationSeconds.DeleteLabelValues(namespace, model)
}

// setPoolPolicyValid tracks the latest parse/validation outcome of a warm pool
// Deployment's policy annotation.
func setPoolPolicyValid(pool types.NamespacedName, valid bool) {
	poolPolicyValid.WithLabelValues(pool.Namespace, pool.Name).Set(b2f(valid))
}

// clearPoolPolicyMetrics drops the gauge series once the annotation is removed,
// so a retired pool policy does not stay frozen at its last value.
func clearPoolPolicyMetrics(pool types.NamespacedName) {
	poolPolicyValid.DeleteLabelValues(pool.Namespace, pool.Name)
}

func recordActivation(namespace, model string, ok bool) {
	result := "success"
	if !ok {
		result = activationResultFailed
	}
	claimActivationTotal.WithLabelValues(namespace, model, result).Inc()
}

// claimNoReadyDuration returns how long a claim has had no ready engine,
// using its creation time until a Ready condition is available.
func claimNoReadyDuration(pm *modelv1alpha1.ModelClaim, now time.Time) time.Duration {
	if pm.Status.ReadyReplicas > 0 {
		return 0
	}

	since := pm.CreationTimestamp.Time
	ready := meta.FindStatusCondition(pm.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	if ready != nil {
		if ready.Status == metav1.ConditionTrue {
			return 0
		}
		since = ready.LastTransitionTime.Time
	}
	if since.IsZero() || now.Before(since) {
		return 0
	}
	return now.Sub(since)
}

// recordPolicyEvaluation records a bounded outcome for a pool-policy evaluation.
func recordPolicyEvaluation(pool types.NamespacedName, result, reason string) {
	result, reason = normalizePolicyLabels(result, reason)
	poolPolicyEvaluationsTotal.WithLabelValues(pool.Namespace, pool.Name, result, reason).Inc()
}

// recordPolicyAction records a bounded outcome for a pool-policy action.
func recordPolicyAction(pool types.NamespacedName, action, result, reason string) {
	result, reason = normalizePolicyLabels(result, reason)
	poolPolicyActionsTotal.WithLabelValues(pool.Namespace, pool.Name, action, result, reason).Inc()
}

// normalizePolicyLabels maps unknown values to bounded failure labels.
func normalizePolicyLabels(result, reason string) (string, string) {
	switch result {
	case policyResultApplied, policyResultSkipped, policyResultFailed:
	default:
		result = policyResultFailed
		reason = policyReasonInternalError
	}
	if _, ok := boundedPolicyReasons[reason]; !ok {
		reason = policyReasonInternalError
	}
	return result, reason
}
