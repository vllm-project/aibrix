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

package stormservice

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/stormservice/metrics"
)

func rolloutTimingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	return scheme
}

// observedSum returns the sum of observations recorded for the given label
// combination, so tests can assert an approximate observed duration without
// depending on exact histogram bucket internals.
func observedSum(t *testing.T, namespace, name, strategy string) float64 {
	t.Helper()
	histogram, ok := metrics.StormServiceRolloutDuration.WithLabelValues(namespace, name, strategy).(prometheus.Histogram)
	require.True(t, ok)
	var m dto.Metric
	require.NoError(t, histogram.Write(&m))
	return m.GetHistogram().GetSampleSum()
}

func observedCount(t *testing.T, namespace, name, strategy string) uint64 {
	t.Helper()
	histogram, ok := metrics.StormServiceRolloutDuration.WithLabelValues(namespace, name, strategy).(prometheus.Histogram)
	require.True(t, ok)
	var m dto.Metric
	require.NoError(t, histogram.Write(&m))
	return m.GetHistogram().GetSampleCount()
}

// TestTrackRolloutDuration_NotReadyToReady_ObservesDurationAndClearsAnnotation
// simulates the not-Ready -> Ready transition and asserts a correct duration
// is observed into the histogram and that the rollout-started-at annotation
// is cleared afterwards.
func TestTrackRolloutDuration_NotReadyToReady_ObservesDurationAndClearsAnnotation(t *testing.T) {
	scheme := rolloutTimingTestScheme(t)
	namespace, name := "default", "not-ready-to-ready"
	startedAt := time.Now().Add(-5 * time.Second)

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Annotations: map[string]string{
				RolloutStartedAtAnnotationKey: startedAt.Format(time.RFC3339),
			},
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).Build()
	r := &StormServiceReconciler{Client: fakeClient}

	checkpoint := &orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{
			{Type: orchestrationv1alpha1.StormServiceProgressing},
		},
	}

	countBefore := observedCount(t, namespace, name, string(orchestrationv1alpha1.RollingUpdateStormServiceStrategyType))

	require.NoError(t, r.trackRolloutDuration(context.TODO(), stormService, checkpoint, true))

	_, hasAnnotation := stormService.Annotations[RolloutStartedAtAnnotationKey]
	assert.False(t, hasAnnotation, "rollout-started-at annotation should be cleared after observing")

	// The annotation clear must have been persisted.
	persisted := &orchestrationv1alpha1.StormService{}
	require.NoError(t, fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: name}, persisted))
	_, persistedHasAnnotation := persisted.Annotations[RolloutStartedAtAnnotationKey]
	assert.False(t, persistedHasAnnotation)

	strategy := string(orchestrationv1alpha1.RollingUpdateStormServiceStrategyType)
	countAfter := observedCount(t, namespace, name, strategy)
	assert.Equal(t, countBefore+1, countAfter, "exactly one observation should be recorded")

	sum := observedSum(t, namespace, name, strategy)
	elapsedSinceStart := time.Since(startedAt).Seconds()
	assert.GreaterOrEqual(t, sum, 4.9, "observed duration should be at least ~5s")
	assert.LessOrEqual(t, sum, elapsedSinceStart+1, "observed duration should not exceed elapsed wall-clock time")
}

// TestTrackRolloutDuration_ReadyToNotReady_WritesAnnotation simulates the
// Ready -> not-Ready transition and asserts the rollout-started-at
// annotation gets written and persisted, with no observation emitted.
func TestTrackRolloutDuration_ReadyToNotReady_WritesAnnotation(t *testing.T) {
	scheme := rolloutTimingTestScheme(t)
	namespace, name := "default", "ready-to-not-ready"

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).Build()
	r := &StormServiceReconciler{Client: fakeClient}

	checkpoint := &orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{
			{Type: orchestrationv1alpha1.StormServiceReady},
		},
	}

	before := time.Now()
	require.NoError(t, r.trackRolloutDuration(context.TODO(), stormService, checkpoint, false))
	after := time.Now()

	raw, ok := stormService.Annotations[RolloutStartedAtAnnotationKey]
	require.True(t, ok, "rollout-started-at annotation should be written on Ready->not-Ready transition")
	parsed, err := time.Parse(time.RFC3339, raw)
	require.NoError(t, err)
	assert.False(t, parsed.Before(before.Add(-time.Second)))
	assert.False(t, parsed.After(after.Add(time.Second)))

	persisted := &orchestrationv1alpha1.StormService{}
	require.NoError(t, fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: name}, persisted))
	assert.Equal(t, raw, persisted.Annotations[RolloutStartedAtAnnotationKey])
}

// TestTrackRolloutDuration_NoTransition_IsNoop asserts that when the Ready
// state hasn't changed relative to checkpoint, no annotation or Update call
// happens (verified indirectly: the object is not present in the fake
// client, so any Update call would error).
func TestTrackRolloutDuration_NoTransition_IsNoop(t *testing.T) {
	scheme := rolloutTimingTestScheme(t)
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-transition"},
	}
	// Intentionally do not register stormService with the fake client: if
	// trackRolloutDuration issued an Update call, it would fail with NotFound.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &StormServiceReconciler{Client: fakeClient}

	checkpointReady := &orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{{Type: orchestrationv1alpha1.StormServiceReady}},
	}
	require.NoError(t, r.trackRolloutDuration(context.TODO(), stormService, checkpointReady, true))

	checkpointProgressing := &orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{{Type: orchestrationv1alpha1.StormServiceProgressing}},
	}
	require.NoError(t, r.trackRolloutDuration(context.TODO(), stormService, checkpointProgressing, false))

	assert.Empty(t, stormService.Annotations)
}

// TestTrackRolloutDuration_FirstReconcileToReady_SkipsObservation simulates
// the very first reconcile of a brand-new object transitioning straight to
// Ready (empty checkpoint.Conditions, no rollout-started-at annotation) and
// asserts no observation is emitted.
func TestTrackRolloutDuration_FirstReconcileToReady_SkipsObservation(t *testing.T) {
	scheme := rolloutTimingTestScheme(t)
	namespace, name := "default", "first-reconcile"

	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stormService).Build()
	r := &StormServiceReconciler{Client: fakeClient}

	strategy := string(orchestrationv1alpha1.RollingUpdateStormServiceStrategyType)
	countBefore := observedCount(t, namespace, name, strategy)

	// checkpoint.Conditions is empty: this is the object's first reconcile.
	require.NoError(t, r.trackRolloutDuration(context.TODO(), stormService, &orchestrationv1alpha1.StormServiceStatus{}, true))

	assert.Equal(t, countBefore, observedCount(t, namespace, name, strategy), "no observation should be recorded without a prior annotation")
	assert.NotContains(t, stormService.Annotations, RolloutStartedAtAnnotationKey)
}

// TestTrackRolloutDuration_SurvivesControllerRestart simulates a controller
// restart mid-rollout: a fresh StormServiceReconciler (no in-memory state
// carried over) operating on an object whose rollout-started-at annotation
// and not-Ready status were persisted by a previous, now-gone, reconciler
// instance. Rollout duration must still be computed correctly purely from
// the persisted annotation.
func TestTrackRolloutDuration_SurvivesControllerRestart(t *testing.T) {
	scheme := rolloutTimingTestScheme(t)
	namespace, name := "default", "restart-mid-rollout"
	startedAt := time.Now().Add(-30 * time.Second)

	// State as persisted by the "previous" controller process before it
	// restarted: annotation present, status still Progressing.
	persistedBeforeRestart := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Annotations: map[string]string{
				RolloutStartedAtAnnotationKey: startedAt.Format(time.RFC3339),
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			Conditions: orchestrationv1alpha1.Conditions{
				{Type: orchestrationv1alpha1.StormServiceProgressing},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persistedBeforeRestart).Build()

	// Simulate the restart: a brand-new reconciler struct, and a freshly
	// Get()'d copy of the object rather than anything carried over in memory.
	freshReconciler := &StormServiceReconciler{Client: fakeClient}
	stormService := &orchestrationv1alpha1.StormService{}
	require.NoError(t, fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: name}, stormService))

	checkpoint := stormService.Status.DeepCopy()

	strategy := string(orchestrationv1alpha1.RollingUpdateStormServiceStrategyType)
	countBefore := observedCount(t, namespace, name, strategy)

	require.NoError(t, freshReconciler.trackRolloutDuration(context.TODO(), stormService, checkpoint, true))

	assert.Equal(t, countBefore+1, observedCount(t, namespace, name, strategy))
	sum := observedSum(t, namespace, name, strategy)
	assert.GreaterOrEqual(t, sum, 29.0, "duration should be computed from the persisted annotation alone, not any in-memory state")

	assert.NotContains(t, stormService.Annotations, RolloutStartedAtAnnotationKey)
}
