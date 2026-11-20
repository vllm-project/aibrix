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

package podautoscaler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStabilizedRecommendationHoldsCooldownWindows(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := []timestampedRecommendation{{recommendation: 6, timestamp: now.Add(-10 * time.Second)}}
	stale := []timestampedRecommendation{{recommendation: 6, timestamp: now.Add(-40 * time.Second)}}

	// No history returns the recommendation itself.
	assert.Equal(t, int32(7), stabilizedRecommendation(nil, now, 7, 3, time.Minute, 5*time.Minute))
	// Scale-up is held to the lowest recommendation seen in the scale-up window.
	assert.Equal(t, int32(6), stabilizedRecommendation(recent, now, 10, 2, time.Minute, 5*time.Minute))
	// A recommendation outside the scale-down window does not hold a scale-down.
	assert.Equal(t, int32(2), stabilizedRecommendation(stale, now, 2, 8, 0, 30*time.Second))
	// A recent recommendation above the target holds the scale-down.
	assert.Equal(t, int32(6), stabilizedRecommendation(recent, now, 2, 8, 0, 5*time.Minute))
}

func TestPreviewStabilizedMatchesAppliedPathWithoutRecording(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reconciler := &PodAutoscalerReconciler{now: func() time.Time { return now }}
	pa := &autoscalingv1alpha1.PodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-pa"}}
	scalingContext := scalingctx.NewBaseScalingContext()

	// An empty history does not hold anything back, and the preview leaves no
	// trace, so it returns the same answer twice.
	assert.Equal(t, int32(6), reconciler.previewStabilized(pa, scalingContext, 6, 2))
	assert.Empty(t, reconciler.recommendations)
	assert.Equal(t, int32(6), reconciler.previewStabilized(pa, scalingContext, 6, 2))

	// The applied path records the recommendation.
	assert.Equal(t, int32(6), reconciler.stabilizeRecommendation(pa, scalingContext, 6, 2))
	require.Len(t, reconciler.recommendations["default/test-pa"], 1)

	// The recorded scale-up holds a later scale-down in the preview through the
	// scale-down window, the same way the applied path would.
	assert.Equal(t, int32(6), reconciler.previewStabilized(pa, scalingContext, 4, 8))
}

func TestPredictiveStatusForBuildsEntry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reconciler := &PodAutoscalerReconciler{now: func() time.Time { return now }}
	pa := &autoscalingv1alpha1.PodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-pa"}}
	scalingContext := scalingctx.NewBaseScalingContext()
	evaluation := &PredictiveResult{
		Mode:              autoscalingv1alpha1.PredictiveModePreview,
		MetricName:        "gpu_cache_usage_perc",
		ObservedValue:     12.3456,
		PredictedValue:    23.4567,
		PredictedReplicas: 3,
		ComposedReplicas:  5,
	}

	status := reconciler.predictiveStatusFor(pa, scalingContext, evaluation, 4, 1, 10, 2)

	require.NotNil(t, status)
	assert.Equal(t, autoscalingv1alpha1.PredictiveModePreview, status.Mode)
	assert.Equal(t, "gpu_cache_usage_perc", status.Metric)
	assert.Equal(t, "12.346", status.ObservedValue)
	assert.Equal(t, "23.457", status.PredictedValue)
	assert.Equal(t, int32(3), status.PredictedReplicas)
	assert.Equal(t, int32(4), status.ReactiveReplicas)
	assert.Equal(t, int32(5), status.WouldBeReplicas)
	require.NotNil(t, status.LastUpdated)
	assert.Equal(t, now, status.LastUpdated.Time)

	// The would-be count passes through the replica bounds like the applied
	// decision does.
	bounded := reconciler.predictiveStatusFor(pa, scalingContext, evaluation, 4, 1, 3, 2)
	assert.Equal(t, int32(3), bounded.WouldBeReplicas)
}

func TestMergePredictiveStatus(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older := metav1.NewTime(now.Add(-time.Minute))
	newer := metav1.NewTime(now)
	spec := &autoscalingv1alpha1.PredictiveSpec{}
	previous := &autoscalingv1alpha1.PredictiveStatus{
		Mode:              autoscalingv1alpha1.PredictiveModePreview,
		Metric:            "gpu_cache_usage_perc",
		ObservedValue:     "1.000",
		PredictedValue:    "2.000",
		PredictedReplicas: 3,
		ReactiveReplicas:  2,
		WouldBeReplicas:   4,
		LastUpdated:       &older,
	}

	// The entry is dropped when spec.predictive is removed.
	assert.Nil(t, mergePredictiveStatus(nil, previous, previous))

	// A round without a projection keeps the previous entry.
	assert.Same(t, previous, mergePredictiveStatus(spec, nil, previous))

	// An unchanged entry keeps its observation time.
	unchanged := &autoscalingv1alpha1.PredictiveStatus{
		Mode:              autoscalingv1alpha1.PredictiveModePreview,
		Metric:            "gpu_cache_usage_perc",
		ObservedValue:     "1.000",
		PredictedValue:    "2.000",
		PredictedReplicas: 3,
		ReactiveReplicas:  2,
		WouldBeReplicas:   4,
		LastUpdated:       &newer,
	}
	merged := mergePredictiveStatus(spec, unchanged, previous)
	assert.Same(t, &older, merged.LastUpdated)

	// A changed projection carries its own observation time.
	changed := *unchanged
	changed.PredictedValue = "9.000"
	changed.LastUpdated = &newer
	result := mergePredictiveStatus(spec, &changed, previous)
	assert.Equal(t, "9.000", result.PredictedValue)
	assert.Equal(t, &newer, result.LastUpdated)
}

func TestSetStatusMergesPredictiveEntries(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	observation := metav1.NewTime(now)
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{Predictive: &autoscalingv1alpha1.PredictiveSpec{}},
	}
	entry := &autoscalingv1alpha1.PredictiveStatus{
		Mode:              autoscalingv1alpha1.PredictiveModePreview,
		Metric:            "gpu_cache_usage_perc",
		ObservedValue:     "1.000",
		PredictedValue:    "2.000",
		PredictedReplicas: 3,
		ReactiveReplicas:  2,
		WouldBeReplicas:   3,
		LastUpdated:       &observation,
	}

	setStatus(pa, 2, 2, false, "stable", false, true, nil, entry)
	require.NotNil(t, pa.Status.Predictive)
	assert.Equal(t, &observation, pa.Status.Predictive.LastUpdated)

	// A round without a projection keeps the previous entry.
	setStatus(pa, 2, 2, false, "stable", false, true, nil, nil)
	require.NotNil(t, pa.Status.Predictive)
	assert.Equal(t, &observation, pa.Status.Predictive.LastUpdated)

	// The entry is dropped when spec.predictive is removed.
	pa.Spec.Predictive = nil
	setStatus(pa, 2, 2, false, "stable", false, true, nil, nil)
	assert.Nil(t, pa.Status.Predictive)
}
