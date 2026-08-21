/*
Copyright 2025 The Aibrix Team.

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

// Package metrics is a shared leaf package for Prometheus metrics emitted by
// the stormservice, roleset, and podset controllers. It has no dependency on
// any of those controller packages, so importing it from all three cannot
// introduce an import cycle.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// State label values shared by the gauges below.
const (
	StateDesired     = "desired"
	StateReady       = "ready"
	StateUnavailable = "unavailable"
)

var (
	// StormServiceRoleSetReplicas reports, per StormService, how many of its
	// RoleSets are in each state. This is a StormService-level count of
	// RoleSets and must not be conflated with RoleSetRolePodReplicas below,
	// which counts Pods within a single RoleSet.
	StormServiceRoleSetReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "stormservice_roleset_replicas",
			Help:      "Number of RoleSets owned by a StormService, by state (desired, ready, unavailable)",
		},
		[]string{"namespace", "name", "state"},
	)

	// RoleSetRolePodReplicas reports, per role within a RoleSet, how many
	// Pods are in each state.
	RoleSetRolePodReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "roleset_role_pod_replicas",
			Help:      "Number of Pods for a role within a RoleSet, by state (desired, ready, unavailable)",
		},
		[]string{"namespace", "roleset", "role", "state"},
	)

	// RoleSetRoleInPlaceFallbackTotal counts in-place rollout fallbacks to
	// pod recreation, bucketed into a small set of reason classes to avoid
	// label cardinality explosion from free-text reason strings.
	RoleSetRoleInPlaceFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "roleset_role_inplace_fallback_total",
			Help:      "Total number of times an in-place rollout for a role fell back to pod recreation, by reason class",
		},
		[]string{"namespace", "roleset", "role", "reason_class"},
	)

	// StormServiceRolloutDuration observes how long a StormService rollout
	// took, measured from the last not-Ready->Ready transition.
	StormServiceRolloutDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "stormservice_rollout_duration_seconds",
			Help:      "Duration in seconds of a StormService rollout, measured from the last not-Ready->Ready transition",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"namespace", "name", "strategy"},
	)
)

func init() {
	// Register with controller-runtime metrics registry
	metrics.Registry.MustRegister(
		StormServiceRoleSetReplicas,
		RoleSetRolePodReplicas,
		RoleSetRoleInPlaceFallbackTotal,
		StormServiceRolloutDuration,
	)
}
