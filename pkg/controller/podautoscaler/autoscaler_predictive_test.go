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
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/prediction"
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

func seedMetricSeries(t *testing.T, scaler *DefaultAutoScaler, pa *autoscalingv1alpha1.PodAutoscaler, end time.Time, values []float64, interval time.Duration) {
	t.Helper()

	metricKey := types.MetricKey{
		Namespace:   pa.Namespace,
		Name:        pa.Spec.ScaleTargetRef.Name,
		MetricName:  predictiveTestMetric,
		PaNamespace: pa.Namespace,
		PaName:      pa.Name,
	}
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

func newPredictiveTestScaler(t *testing.T, pa *autoscalingv1alpha1.PodAutoscaler, now time.Time, values []float64) (*DefaultAutoScaler, ReplicaComputeRequest) {
	t.Helper()

	scaler := &DefaultAutoScaler{
		metricsClient: metrics.NewMetricsClient(time.Second),
		predictor:     prediction.NewLinear(),
	}
	seedMetricSeries(t, scaler, pa, now, values, 10*time.Second)

	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 20
	scalingContext.MaxScaleUpRate = 4
	require.NoError(t, scalingContext.UpdateByPaTypes(pa))

	return scaler, ReplicaComputeRequest{
		PodAutoscaler:   *pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 2,
		Timestamp:       now,
	}
}

func TestPredictiveModeDefaultsToPreviewInDecision(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA("", nil)
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 1, Valid: true, Reason: "stable"})

	require.NotNil(t, result.Predictive)
	assert.Equal(t, autoscalingv1alpha1.PredictiveModePreview, result.Predictive.Mode)
	assert.False(t, result.Predictive.Applied)
	assert.Equal(t, int32(1), result.DesiredReplicas)
	assert.Equal(t, int32(3), result.Predictive.PredictedReplicas)
}

func TestPredictiveAutoRaisesDecisionToProjection(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 1, Valid: true, Reason: "stable"})

	require.NotNil(t, result.Predictive)
	assert.True(t, result.Predictive.Applied)
	assert.Equal(t, int32(3), result.DesiredReplicas)
	assert.Contains(t, result.Reason, "predictive floor=3")
}

func TestPredictiveAutoNeverLowersDecision(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, ptr.To[int64](60))
	pa.Spec.ObserveWindowSeconds = ptr.To[int64](60)

	scaler := &DefaultAutoScaler{
		metricsClient: metrics.NewMetricsClient(time.Second),
		predictor:     prediction.NewLinear(),
	}
	seedMetricSeries(t, scaler, &pa, now, []float64{100, 90, 80}, 20*time.Second)

	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 20
	scalingContext.MaxScaleUpRate = 4
	require.NoError(t, scalingContext.UpdateByPaTypes(&pa))

	request := ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 5,
		Timestamp:       now,
	}

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 5, Valid: true, Reason: "All metrics below target"})

	require.NotNil(t, result.Predictive)
	assert.Equal(t, int32(2), result.Predictive.PredictedReplicas)
	assert.False(t, result.Predictive.Applied)
	assert.Equal(t, int32(5), result.DesiredReplicas)
	assert.Equal(t, "All metrics below target", result.Reason)
}

func TestPredictiveAutoKeepsFloorWithinScaleUpRate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))
	request.CurrentReplicas = 1
	rateCappedContext := scalingctx.NewBaseScalingContext()
	rateCappedContext.MaxReplicas = 20
	rateCappedContext.MaxScaleUpRate = 2
	require.NoError(t, rateCappedContext.UpdateByPaTypes(&pa))
	request.ScalingContext = rateCappedContext

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 1, Valid: true, Reason: "stable"})

	require.NotNil(t, result.Predictive)
	assert.Equal(t, int32(3), result.Predictive.PredictedReplicas)
	assert.True(t, result.Predictive.Applied)
	assert.Equal(t, int32(2), result.DesiredReplicas)
}

// A workload that sits at zero replicas still has to be able to wake up.
// The scale-up rate of zero replicas would otherwise pin the projected
// floor to zero and block the prediction from ever starting the workload.
func TestPredictiveAutoWakesWorkloadFromZeroReplicas(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))
	request.CurrentReplicas = 0

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 0, Valid: true, Reason: "stable"})

	require.NotNil(t, result.Predictive)
	assert.True(t, result.Predictive.Applied)
	assert.Greater(t, result.Predictive.PredictedReplicas, int32(1))
	assert.Equal(t, int32(1), result.DesiredReplicas)
}

func TestPredictiveSkipsHPADecisions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	pa.Spec.ScalingStrategy = autoscalingv1alpha1.HPA
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 4, Valid: true, Reason: "HPA managed by Kubernetes"})

	assert.Nil(t, result.Predictive)
	assert.Equal(t, int32(4), result.DesiredReplicas)
}

func TestPredictiveWithoutSpecIsInactive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	pa.Spec.Predictive = nil
	scaler, request := newPredictiveTestScaler(t, &pa, now, risingSeries(19, 10, 5))

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 1, Valid: true})

	assert.Nil(t, result.Predictive)
	assert.Equal(t, int32(1), result.DesiredReplicas)
}

func TestPredictiveNeedsEnoughHistory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pa := predictiveTestPA(autoscalingv1alpha1.PredictiveModeAuto, nil)
	scaler, request := newPredictiveTestScaler(t, &pa, now, []float64{10, 20})

	result := scaler.applyPredictiveFloor(request, &ReplicaComputeResult{DesiredReplicas: 1, Valid: true})

	assert.Nil(t, result.Predictive)
	assert.Equal(t, int32(1), result.DesiredReplicas)
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
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, predictedReplicas(tt.strategy, tt.current, tt.value, tt.target))
		})
	}
}

func TestComputeDesiredReplicasReportsPredictiveAcrossPipeline(t *testing.T) {
	for _, tt := range []struct {
		name         string
		mode         autoscalingv1alpha1.PredictiveMode
		wantReplicas int32
		wantApplied  bool
	}{
		{
			name:         "preview keeps the reactive decision",
			mode:         autoscalingv1alpha1.PredictiveModePreview,
			wantReplicas: 2,
			wantApplied:  false,
		},
		{
			name:         "auto raises the decision",
			mode:         autoscalingv1alpha1.PredictiveModeAuto,
			wantReplicas: 3,
			wantApplied:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			pa := predictiveTestPA(tt.mode, nil)

			scheme := runtime.NewScheme()
			require.NoError(t, autoscalingv1alpha1.AddToScheme(scheme))
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&pa).Build()

			mockFactory := &mockMetricFetcherFactory{mockMetricFetcher: mockMetricFetcher{metricsValue: 100}}
			autoScaler := NewDefaultAutoScaler(mockFactory, fakeClient)

			scalingContext := scalingctx.NewBaseScalingContext()
			scalingContext.MaxReplicas = 10
			scalingContext.MaxScaleUpRate = 4
			require.NoError(t, scalingContext.UpdateByPaTypes(&pa))

			seedMetricSeries(t, autoScaler, &pa, now, risingSeries(18, 10, 5), 10*time.Second)

			result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
				PodAutoscaler:   pa,
				ScalingContext:  scalingContext,
				CurrentReplicas: 2,
				Pods:            []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}}},
				Timestamp:       now,
			})

			require.NoError(t, err)
			require.NotNil(t, result.Predictive)
			assert.Equal(t, tt.wantReplicas, result.DesiredReplicas)
			assert.Equal(t, tt.wantApplied, result.Predictive.Applied)
			assert.Equal(t, int32(3), result.Predictive.PredictedReplicas)
			assert.InDelta(t, 55, result.Predictive.ObservedValue, 0.0001)
		})
	}
}
