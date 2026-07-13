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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		pm := &modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "obs", Name: "gauge-activating"},
			Status: modelv1alpha1.ModelClaimStatus{
				DesiredReplicas: 1, ReadyReplicas: 0,
				Instances: []modelv1alpha1.ModelClaimInstance{{Pod: "p", Phase: modelv1alpha1.ModelClaimActivating}},
			},
		}
		setClaimGauges(pm)
		assert.Equal(t, 0.0, testutil.ToFloat64(claimReadyReplicas.WithLabelValues("obs", "gauge-activating")))
		assert.Equal(t, 1.0, testutil.ToFloat64(claimActivating.WithLabelValues("obs", "gauge-activating")))
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
}
