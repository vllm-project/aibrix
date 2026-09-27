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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const predictiveTestMetric = "gpu_cache_usage_perc"

func predictiveTestPA(mode autoscalingv1alpha1.PredictiveMode, horizonSeconds *int64) autoscalingv1alpha1.PodAutoscaler {
	return autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-predictive"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Kind: "Deployment", Name: "test-llm"},
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			Predictive: &autoscalingv1alpha1.PredictiveSpec{
				Mode:           mode,
				HorizonSeconds: horizonSeconds,
			},
			ObserveWindowSeconds: ptr.To[int64](180),
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     predictiveTestMetric,
					TargetValue:      "50",
				},
			},
		},
	}
}

func predictiveMetricKey(pa *autoscalingv1alpha1.PodAutoscaler) types.MetricKey {
	return types.MetricKey{
		Namespace:   pa.Namespace,
		Name:        pa.Spec.ScaleTargetRef.Name,
		MetricName:  pa.Spec.MetricsSources[0].TargetMetric,
		PaNamespace: pa.Namespace,
		PaName:      pa.Name,
	}
}

func seedMetricSeries(t *testing.T, scaler *DefaultAutoScaler, pa *autoscalingv1alpha1.PodAutoscaler, end time.Time, values []float64, interval time.Duration) {
	t.Helper()

	metricKey := predictiveMetricKey(pa)
	stable, panicWindow := metricWindowDurations(*pa)
	require.NoError(t, scaler.metricsClient.ConfigureMetricWindows(metricKey, stable, panicWindow))

	start := end.Add(-time.Duration(len(values)-1) * interval)
	for i, value := range values {
		require.NoError(t, scaler.metricsClient.UpdateMetrics(start.Add(time.Duration(i)*interval), metricKey, value))
	}
}

func risingSeries(count int, start, step float64) []float64 {
	values := make([]float64, 0, count)
	for i := 0; i < count; i++ {
		values = append(values, start+float64(i)*step)
	}
	return values
}

func fallingSeries(count int, start, step float64) []float64 {
	values := make([]float64, 0, count)
	for i := 0; i < count; i++ {
		values = append(values, start-float64(i)*step)
	}
	return values
}

func newPredictiveScaler(t *testing.T, pa *autoscalingv1alpha1.PodAutoscaler, now time.Time, values []float64, mockValue float64) (*DefaultAutoScaler, ReplicaComputeRequest) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, autoscalingv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pa).Build()

	scaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{
		mockMetricFetcher: mockMetricFetcher{metricsValue: mockValue},
	}, fakeClient)
	seedMetricSeries(t, scaler, pa, now, values, 10*time.Second)

	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 20
	scalingContext.MaxScaleUpRate = 4
	require.NoError(t, scalingContext.UpdateByPaTypes(pa))

	return scaler, ReplicaComputeRequest{
		PodAutoscaler:   *pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 2,
		Pods:            []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}}},
		Timestamp:       now,
	}
}

func TestPredictivePreviewReportsWithoutChangingTheDecision(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	series := risingSeries(18, 10, 10)

	paBaseline := predictiveTestPA(autoscalingv1alpha1.PredictiveModePreview, nil)
	paBaseline.Spec.Predictive = nil
	baselineScaler, baselineRequest := newPredictiveScaler(t, &paBaseline, now, series, 100)
	baseline, err := baselineScaler.ComputeDesiredReplicas(context.TODO(), baselineRequest)
	require.NoError(t, err)

	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModePreview, nil)
	scaler, request := newPredictiveScaler(t, &pa, now, series, 100)
	result, err := scaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	assert.Equal(t, baseline.DesiredReplicas, result.DesiredReplicas,
		"the projection must not change the decision in this step")
	require.NotNil(t, result.Predictive)
	assert.Equal(t, autoscalingv1alpha1.PredictiveModePreview, result.Predictive.Mode)
	assert.Equal(t, predictiveTestMetric, result.Predictive.MetricName)
	assert.Greater(t, result.Predictive.PredictedReplicas, result.DesiredReplicas)
	assert.Equal(t, max(result.DesiredReplicas, result.Predictive.PredictedReplicas), result.Predictive.ComposedReplicas)
}

func TestPredictiveAutoIsObservationOnlyInThisStep(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveScaler(t, &pa, now, risingSeries(18, 10, 10), 100)

	result, err := scaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	require.NotNil(t, result.Predictive)
	assert.Equal(t, autoscalingv1alpha1.PredictiveModeAuto, result.Predictive.Mode)
	// The metric reads 100 against a target of 50, so the reactive count is 2.
	// Auto is accepted and reported, but it does not raise the decision until
	// the composition lands in a follow-up.
	assert.Equal(t, int32(2), result.DesiredReplicas)
	assert.Greater(t, result.Predictive.ComposedReplicas, result.DesiredReplicas)
}

func TestPredictiveWeakFloorKeepsComposedAtTheReactiveCount(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveScaler(t, &pa, now, fallingSeries(18, 180, 10), 10)

	result, err := scaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	require.NotNil(t, result.Predictive)
	// A forecast that lands at zero is reported as zero, not dropped, and a
	// floor below the reactive recommendation does not raise the composition.
	assert.Equal(t, int32(0), result.Predictive.PredictedReplicas)
	assert.Equal(t, result.DesiredReplicas, result.Predictive.ComposedReplicas)
}

func TestPredictiveCompositionPassesThroughThePendingGuard(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveScaler(t, &pa, now, risingSeries(18, 10, 10), 100)

	request.CurrentReplicas = 4
	request.ReplicaState = ReplicaState{ReadyReplicas: 2, PendingReplicas: 2}

	result, err := scaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	require.NotNil(t, result.Predictive)
	assert.True(t, result.PendingReplicaGuardActive)
	// The guard holds the pending pods at the current count, and the composed
	// count passes through the same adjustment: without the guard the composed
	// count would stay at the prediction.
	assert.Equal(t, int32(4), result.DesiredReplicas)
	assert.Greater(t, result.Predictive.PredictedReplicas, int32(4))
	assert.Equal(t, int32(4), result.Predictive.ComposedReplicas)
}

func TestProjectMetricSkipsHPAStrategy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	pa.Spec.ScalingStrategy = autoscalingv1alpha1.HPA

	scaler := &DefaultAutoScaler{metricsClient: metrics.NewMetricsClient(time.Second)}
	seedMetricSeries(t, scaler, &pa, now, risingSeries(18, 10, 10), 10*time.Second)

	request := ReplicaComputeRequest{PodAutoscaler: pa, CurrentReplicas: 2, Timestamp: now}
	assert.Nil(t, scaler.projectMetric(request, pa.Spec.MetricsSources[0], predictiveMetricKey(&pa)))
}

func TestProjectMetricRequiresConfiguredPredictiveAndEnoughHistory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	pa.Spec.Predictive = nil
	scaler := &DefaultAutoScaler{metricsClient: metrics.NewMetricsClient(time.Second)}
	seedMetricSeries(t, scaler, &pa, now, risingSeries(18, 10, 10), 10*time.Second)
	request := ReplicaComputeRequest{PodAutoscaler: pa, ScalingContext: testScalingContext(t, &pa), CurrentReplicas: 2, Timestamp: now}
	assert.Nil(t, scaler.projectMetric(request, pa.Spec.MetricsSources[0], predictiveMetricKey(&pa)))

	sparse := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	sparseScaler := &DefaultAutoScaler{metricsClient: metrics.NewMetricsClient(time.Second)}
	seedMetricSeries(t, sparseScaler, &sparse, now, []float64{10, 20}, 10*time.Second)
	sparseRequest := ReplicaComputeRequest{PodAutoscaler: sparse, ScalingContext: testScalingContext(t, &sparse), CurrentReplicas: 2, Timestamp: now}
	assert.Nil(t, sparseScaler.projectMetric(sparseRequest, sparse.Spec.MetricsSources[0], predictiveMetricKey(&sparse)))
}

func TestPredictedReplicasMirrorsStrategyFormula(t *testing.T) {
	tests := map[string]struct {
		strategy autoscalingv1alpha1.ScalingStrategyType
		current  int32
		value    float64
		target   float64
		want     int32
	}{
		"kpa divides the projected value": {
			strategy: autoscalingv1alpha1.KPA,
			current:  3,
			value:    100,
			target:   50,
			want:     2,
		},
		"apa scales the current count": {
			strategy: autoscalingv1alpha1.APA,
			current:  3,
			value:    100,
			target:   50,
			want:     6,
		},
		"rounds up": {
			strategy: autoscalingv1alpha1.KPA,
			current:  1,
			value:    101,
			target:   50,
			want:     3,
		},
		"a zero projection stays zero": {
			strategy: autoscalingv1alpha1.KPA,
			current:  4,
			value:    0,
			target:   50,
			want:     0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, predictedReplicas(tt.strategy, tt.current, tt.value, tt.target))
		})
	}
}
