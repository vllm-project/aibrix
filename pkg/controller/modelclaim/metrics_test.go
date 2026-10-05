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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// gaugeHasModel reports whether the named gauge family currently has a series
// for the given namespace/model, and its value. Used to assert clearing.
func gaugeHasModel(t *testing.T, family, namespace, model string) (float64, bool) {
	t.Helper()
	fams, err := crmetrics.Registry.Gather()
	require.NoError(t, err)
	for _, mf := range fams {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			var ns, md string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "namespace":
					ns = l.GetValue()
				case "model":
					md = l.GetValue()
				}
			}
			if ns == namespace && md == model {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestOperationalMetricsRegistered(t *testing.T) {
	defer claimNoReadyDurationSeconds.DeleteLabelValues("obs", "registration")
	defer poolPolicyEvaluationsTotal.DeleteLabelValues(
		"obs", "registration", policyResultSkipped, policyReasonNoChange,
	)
	defer poolPolicyActionsTotal.DeleteLabelValues(
		"obs", "registration", policyActionSetKVLimit, policyResultSkipped, policyReasonNoChange,
	)
	claimNoReadyDurationSeconds.WithLabelValues("obs", "registration").Set(0)
	poolPolicyEvaluationsTotal.WithLabelValues(
		"obs", "registration", policyResultSkipped, policyReasonNoChange,
	).Add(0)
	poolPolicyActionsTotal.WithLabelValues(
		"obs", "registration", policyActionSetKVLimit, policyResultSkipped, policyReasonNoChange,
	).Add(0)

	families, err := crmetrics.Registry.Gather()
	require.NoError(t, err)
	names := make(map[string]struct{}, len(families))
	for _, family := range families {
		names[family.GetName()] = struct{}{}
	}

	for _, name := range []string{
		"aibrix_modelclaim_no_ready_duration_seconds",
		"aibrix_modelclaim_pool_policy_evaluations_total",
		"aibrix_modelclaim_pool_policy_actions_total",
	} {
		assert.Contains(t, names, name)
	}
}

func TestRecordActivation(t *testing.T) {
	ns, model := "obs", "counters-1"
	ok0 := testutil.ToFloat64(claimActivationTotal.WithLabelValues(ns, model, "success"))
	fail0 := testutil.ToFloat64(claimActivationTotal.WithLabelValues(ns, model, "failed"))

	recordActivation(ns, model, true)
	recordActivation(ns, model, false)

	assert.Equal(t, ok0+1, testutil.ToFloat64(claimActivationTotal.WithLabelValues(ns, model, "success")))
	assert.Equal(t, fail0+1, testutil.ToFloat64(claimActivationTotal.WithLabelValues(ns, model, "failed")))
}

func TestSetClaimGauges(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		pm := &modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "obs", Name: "gauge-active"},
			Status: modelv1alpha1.ModelClaimStatus{
				DesiredReplicas: 1, ReadyReplicas: 1,
				Instances: []modelv1alpha1.ModelClaimInstance{{Pod: "p", Phase: modelv1alpha1.ModelClaimActive}},
			},
		}
		setClaimGauges(pm)
		assert.Equal(t, 1.0, testutil.ToFloat64(claimDesiredReplicas.WithLabelValues("obs", "gauge-active")))
		assert.Equal(t, 1.0, testutil.ToFloat64(claimReadyReplicas.WithLabelValues("obs", "gauge-active")))
		assert.Equal(t, 0.0, testutil.ToFloat64(claimActivating.WithLabelValues("obs", "gauge-active")))
	})

	t.Run("activating", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		pm := &modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "obs", Name: "gauge-activating"},
			Status: modelv1alpha1.ModelClaimStatus{
				DesiredReplicas: 1, ReadyReplicas: 0,
				Instances: []modelv1alpha1.ModelClaimInstance{{Pod: "p", Phase: modelv1alpha1.ModelClaimActivating}},
				Conditions: []metav1.Condition{{
					Type:               string(modelv1alpha1.ModelClaimConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(now.Add(-15 * time.Second)),
				}},
			},
		}
		setClaimGaugesAt(pm, now)
		assert.Equal(t, 0.0, testutil.ToFloat64(claimReadyReplicas.WithLabelValues("obs", "gauge-activating")))
		assert.Equal(t, 1.0, testutil.ToFloat64(claimActivating.WithLabelValues("obs", "gauge-activating")))
		assert.Equal(t, 15.0, testutil.ToFloat64(claimNoReadyDurationSeconds.WithLabelValues("obs", "gauge-activating")))
	})
}

func TestClearClaimMetrics(t *testing.T) {
	pm := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "obs", Name: "gauge-clear"},
		Status:     modelv1alpha1.ModelClaimStatus{DesiredReplicas: 1, ReadyReplicas: 1},
	}
	setClaimGauges(pm)
	_, ok := gaugeHasModel(t, "aibrix_modelclaim_ready_replicas", "obs", "gauge-clear")
	require.True(t, ok, "series must exist after setClaimGauges")

	clearClaimMetrics("obs", "gauge-clear")
	_, ok = gaugeHasModel(t, "aibrix_modelclaim_ready_replicas", "obs", "gauge-clear")
	assert.False(t, ok, "series must be gone after clearClaimMetrics")
	_, ok = gaugeHasModel(t, "aibrix_modelclaim_activating", "obs", "gauge-clear")
	assert.False(t, ok)
	_, ok = gaugeHasModel(t, "aibrix_modelclaim_no_ready_duration_seconds", "obs", "gauge-clear")
	assert.False(t, ok)
}

func TestNoReadyDurationUsesCreationUntilReadyConditionExists(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	pm := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
	}

	assert.Equal(t, 30*time.Second, claimNoReadyDuration(pm, now))
	pm.Status.ReadyReplicas = 1
	assert.Zero(t, claimNoReadyDuration(pm, now))
}

func TestClearPoolPolicyMetrics(t *testing.T) {
	pool := types.NamespacedName{Namespace: "obs", Name: "gauge-clear"}
	setPoolPolicyValid(pool, true)
	assert.Equal(t, 1.0, testutil.ToFloat64(
		poolPolicyValid.WithLabelValues(pool.Namespace, pool.Name),
	))

	clearPoolPolicyMetrics(pool)

	assert.False(t, poolPolicyValid.DeleteLabelValues(pool.Namespace, pool.Name))
}

func TestPoolPolicyMetricLabelsAreBounded(t *testing.T) {
	pool := types.NamespacedName{Namespace: "obs", Name: "bounded-labels"}
	evaluationLabels := []string{
		pool.Namespace, pool.Name, policyResultFailed, policyReasonInternalError,
	}
	actionLabels := []string{
		pool.Namespace, pool.Name, policyActionSetKVLimit,
		policyResultFailed, policyReasonInternalError,
	}
	defer poolPolicyEvaluationsTotal.DeleteLabelValues(evaluationLabels...)
	defer poolPolicyActionsTotal.DeleteLabelValues(actionLabels...)
	evaluationsBefore := testutil.ToFloat64(
		poolPolicyEvaluationsTotal.WithLabelValues(evaluationLabels...),
	)
	actionsBefore := testutil.ToFloat64(
		poolPolicyActionsTotal.WithLabelValues(actionLabels...),
	)

	recordPolicyEvaluation(pool, "unexpected-result", "free-form error text")
	recordPolicyAction(
		pool, policyActionSetKVLimit, "unexpected-result", "free-form error text",
	)

	assert.Equal(t, evaluationsBefore+1, testutil.ToFloat64(
		poolPolicyEvaluationsTotal.WithLabelValues(evaluationLabels...),
	))
	assert.Equal(t, actionsBefore+1, testutil.ToFloat64(
		poolPolicyActionsTotal.WithLabelValues(actionLabels...),
	))
}
